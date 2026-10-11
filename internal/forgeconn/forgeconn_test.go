package forgeconn

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/forgeapi"
	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/github-scout/internal/ghquota"
)

// fakeAPI answers the reads the adapter makes from function fields; an
// unset field panics, so a test that reaches a read it did not stage fails
// loudly.
type fakeAPI struct {
	repos      func(opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Repository], error)
	myPRs      func(opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.PullRequest], error)
	listPRs    func(opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.PullRequest], error)
	myIssues   func(opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Issue], error)
	listIssues func(opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Issue], error)
	runs       func(opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Run], error)
	status     func(ref string) (forgeapi.CommitChecks, error)
	whoamiErr  error
	product    forgeapi.Product
	budget     forgeapi.BudgetState
	closed     bool
}

func (f *fakeAPI) ConnectionCaps(context.Context) (forgeapi.ConnectionCaps, error) {
	return forgeapi.ConnectionCaps{Product: f.product}, nil
}

func (f *fakeAPI) Whoami(context.Context) (forgeapi.Account, error) {
	if f.whoamiErr != nil {
		return forgeapi.Account{}, f.whoamiErr
	}
	return forgeapi.Account{Login: "me"}, nil
}

func (f *fakeAPI) BudgetState() forgeapi.BudgetState { return f.budget }

func (f *fakeAPI) Close() { f.closed = true }

func (f *fakeAPI) ListRepos(_ context.Context, opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Repository], error) {
	return f.repos(opts...)
}

func (f *fakeAPI) ListMyPRs(_ context.Context, opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.PullRequest], error) {
	return f.myPRs(opts...)
}

func (f *fakeAPI) ListMyIssues(_ context.Context, opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Issue], error) {
	return f.myIssues(opts...)
}

func (f *fakeAPI) ListPRs(_ context.Context, _ forgeapi.RepoRef, opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.PullRequest], error) {
	return f.listPRs(opts...)
}

func (f *fakeAPI) ListIssues(_ context.Context, _ forgeapi.RepoRef, opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Issue], error) {
	return f.listIssues(opts...)
}

func (f *fakeAPI) ListRuns(_ context.Context, _ forgeapi.RepoRef, opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Run], error) {
	return f.runs(opts...)
}

func (f *fakeAPI) CommitStatus(_ context.Context, _ forgeapi.RepoRef, ref string) (forgeapi.CommitChecks, error) {
	return f.status(ref)
}

func client(t *testing.T, f *fakeAPI) *Client {
	t.Helper()
	if f.product == forgeapi.ProductUnknown {
		f.product = forgeapi.ProductGitHub
	}
	family := map[forgeapi.Product]forgeapi.Family{
		forgeapi.ProductGitHub: forgeapi.FamilyGitHub, forgeapi.ProductGitLab: forgeapi.FamilyGitLab,
		forgeapi.ProductGitea: forgeapi.FamilyGitea, forgeapi.ProductForgejo: forgeapi.FamilyGitea,
	}[f.product]
	c, err := newClient(t.Context(), f, family, []string{"o"}, func(forge.Product) {})
	if err != nil {
		t.Fatalf("Setup: newClient = %v", err)
	}
	return c
}

var ghRepo = forge.Repo{Path: "o/r"}.WithHandle(forgeapi.RepoRef{Family: forgeapi.FamilyGitHub, Selector: "o/r", DisplayPath: "o/r"})

func settings(t *testing.T, opts []forgeapi.ListOption) forgeapi.ListSettings {
	t.Helper()
	s, err := forgeapi.ResolveList(opts...)
	if err != nil {
		t.Fatalf("ResolveList = %v", err)
	}
	return s
}

func TestNewClient_names_the_product_and_account(t *testing.T) {
	for p, want := range map[forgeapi.Product]forge.Product{
		forgeapi.ProductGitHub: forge.ProductGitHub, forgeapi.ProductGitLab: forge.ProductGitLab,
		forgeapi.ProductGitea: forge.ProductGitea, forgeapi.ProductForgejo: forge.ProductForgejo,
	} {
		c := client(t, &fakeAPI{product: p})
		if c.Product() != want || c.Login() != "me" {
			t.Errorf("newClient(%v) = product %v login %q, want %v me", p, c.Product(), c.Login(), want)
		}
	}
}

func TestNewClient_refuses_an_unknown_product_and_a_foreign_owner_form(t *testing.T) {
	if _, err := newClient(t.Context(), &fakeAPI{product: forgeapi.ProductUnknown}, forgeapi.FamilyGitHub, []string{"o"}, func(forge.Product) {}); !errors.Is(err, forge.ErrConnection) {
		t.Errorf("newClient(unknown product) = %v, want forge.ErrConnection", err)
	}
	if _, err := newClient(t.Context(), &fakeAPI{product: forgeapi.ProductGitHub}, forgeapi.FamilyGitHub, []string{"group/sub"}, func(forge.Product) {}); !errors.Is(err, forge.ErrConnection) {
		t.Errorf("newClient(GitHub, owner group/sub) = %v, want forge.ErrConnection: a path owner is GitLab's alone", err)
	}
	if _, err := newClient(t.Context(), &fakeAPI{product: forgeapi.ProductGitLab}, forgeapi.FamilyGitLab, []string{"group/sub"}, func(forge.Product) {}); err != nil {
		t.Errorf("newClient(GitLab, owner group/sub) = %v, want nil", err)
	}
}

func TestNewClient_a_failure_after_detection_names_the_product(t *testing.T) {
	rejected := &forgeapi.Error{Kind: forgeapi.KindUnauthorized, Status: 401}
	tests := []struct {
		f      *fakeAPI
		want   error
		name   string
		owners []string
	}{
		{name: "whoami_401", f: &fakeAPI{product: forgeapi.ProductForgejo, whoamiErr: rejected}, owners: []string{"o"}, want: forge.ErrTokenInvalid},
		{name: "owner_form", f: &fakeAPI{product: forgeapi.ProductGitea}, owners: []string{"group/sub"}, want: forge.ErrConnection},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newClient(t.Context(), tc.f, forgeapi.FamilyGitea, tc.owners, func(forge.Product) {})
			oe, ok := errors.AsType[*forge.OpenError](err)
			if !ok || oe.Product != productOf(tc.f.product) || !errors.Is(err, tc.want) {
				t.Errorf("newClient(%s) = %v, want a *forge.OpenError naming %v and wrapping %v", tc.name, err, tc.f.product, tc.want)
			}
		})
	}
	_, err := newClient(t.Context(), &fakeAPI{product: forgeapi.ProductUnknown}, forgeapi.FamilyGitHub, []string{"o"}, func(forge.Product) {})
	if _, ok := errors.AsType[*forge.OpenError](err); ok {
		t.Errorf("newClient(unknown product) = %v, want no product named", err)
	}
}

func TestRunsSince_maps_completed_runs_sets_unmapped_ones_apart_and_dates_the_oldest_row(t *testing.T) {
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	since := created.Add(-time.Hour)
	var sent forgeapi.ListSettings
	f := &fakeAPI{runs: func(opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Run], error) {
		sent = settings(t, opts)
		return forgeapi.Page[forgeapi.Run]{Items: []forgeapi.Run{
			{
				Ref: forgeapi.RunRef{ID: 11, Number: 3}, Name: "CI", Branch: "main", WebURL: "u", CreatedAt: created,
				StartedAt: created.Add(time.Minute), UpdatedAt: created.Add(9 * time.Minute), State: forgeapi.CheckFailing, Trigger: forgeapi.TriggerSchedule,
			},
			{Ref: forgeapi.RunRef{ID: 12, Number: 4}, Name: "CI", Branch: "main", WebURL: "p", CreatedAt: created.Add(-time.Minute), State: forgeapi.CheckPending},
			{Ref: forgeapi.RunRef{ID: 13, Number: 5}, Name: "Docs", Branch: "main", WebURL: "x", CreatedAt: created, State: forgeapi.CheckUnknown},
			{Ref: forgeapi.RunRef{ID: 14}, State: forgeapi.CheckPassing},
			{Ref: forgeapi.RunRef{ID: 15}, State: forgeapi.CheckNeutral, Trigger: forgeapi.TriggerPullRequest},
			{Ref: forgeapi.RunRef{ID: 16, Number: 6}, Name: "CI", Branch: "main", WebURL: "q", CreatedAt: created.Add(2 * time.Minute), State: forgeapi.CheckPending},
		}}, nil
	}}
	list, err := client(t, f).RunsSince(t.Context(), ghRepo, since)
	if err != nil {
		t.Fatalf("RunsSince = %v", err)
	}
	if !sent.SinceSet || !sent.Since.Equal(since) {
		t.Errorf("RunsSince sent since %v (set %t), want %v", sent.Since, sent.SinceSet, since)
	}
	if len(list.Rows) != 3 || list.End != forge.EndWhole {
		t.Fatalf("RunsSince = %d runs ending %v, want 3 runs, whole", len(list.Rows), list.End)
	}
	want := forge.Run{
		CreatedAt: created, StartedAt: created.Add(time.Minute), UpdatedAt: created.Add(9 * time.Minute), Repo: "o/r",
		Workflow: "CI", Branch: "main", Trigger: "schedule", URL: "u", State: forge.RunFailing, ID: 11, Number: 3,
	}
	if list.Rows[0] != want {
		t.Errorf("RunsSince row = %+v, want %+v", list.Rows[0], want)
	}
	if list.Rows[1].State != forge.RunPassing || list.Rows[2].State != forge.RunNeutral {
		t.Errorf("RunsSince states = %v %v, want passing neutral", list.Rows[1].State, list.Rows[2].State)
	}
	if !list.Oldest.Equal(created.Add(-time.Minute)) {
		t.Errorf("RunsSince oldest = %s, want the pending run's %s: the oldest row of any state is where a cut listing reaches", list.Oldest, created.Add(-time.Minute))
	}
	if !list.OldestPending.Equal(created.Add(-time.Minute)) {
		t.Errorf("RunsSince oldest pending = %s, want run 12's %s, the older of the two pending runs", list.OldestPending, created.Add(-time.Minute))
	}
	wantUnmapped := []forge.Run{{CreatedAt: created, Repo: "o/r", Workflow: "Docs", Branch: "main", Trigger: "unknown", URL: "x", ID: 13, Number: 5}}
	if !slices.Equal(list.Unmapped, wantUnmapped) {
		t.Errorf("RunsSince unmapped = %+v, want %+v, with no state", list.Unmapped, wantUnmapped)
	}
	if list.Rows[2].Trigger != forge.TriggerPullRequest {
		t.Errorf("RunsSince pull request trigger = %q, want %q, the spelling the collector excludes from the default branch",
			list.Rows[2].Trigger, forge.TriggerPullRequest)
	}
}

func TestRunsSince_ends_a_cut_listing_cut(t *testing.T) {
	tests := map[string]struct {
		runs func(opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Run], error)
	}{
		"result_window": {runs: func(...forgeapi.ListOption) (forgeapi.Page[forgeapi.Run], error) {
			return forgeapi.Page[forgeapi.Run]{Partial: &forgeapi.Partial{Reason: forgeapi.PartialResultWindow}}, nil
		}},
		"ceiling": {runs: func(...forgeapi.ListOption) (forgeapi.Page[forgeapi.Run], error) {
			return forgeapi.Page[forgeapi.Run]{
				Items: []forgeapi.Run{{Ref: forgeapi.RunRef{ID: 1}, State: forgeapi.CheckPassing}},
				Next:  "more", Partial: &forgeapi.Partial{Reason: forgeapi.PartialPaginationCap},
			}, nil
		}},
		"budget": {runs: func(...forgeapi.ListOption) (forgeapi.Page[forgeapi.Run], error) {
			return forgeapi.Page[forgeapi.Run]{Next: "more", Partial: &forgeapi.Partial{Reason: forgeapi.PartialBudget}}, nil
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			list, err := client(t, &fakeAPI{runs: tc.runs}).RunsSince(t.Context(), ghRepo, time.Now())
			if err != nil || list.End != forge.EndCut {
				t.Errorf("RunsSince(%s) = ending %v, %v, want cut, no error", name, list.End, err)
			}
		})
	}
}

func TestRunsSince_follows_a_continuation_past_a_page_the_bound_left_empty(t *testing.T) {
	calls := 0
	f := &fakeAPI{runs: func(opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Run], error) {
		calls++
		if settings(t, opts).After == "" {
			return forgeapi.Page[forgeapi.Run]{Next: "p2"}, nil
		}
		return forgeapi.Page[forgeapi.Run]{Items: []forgeapi.Run{{Ref: forgeapi.RunRef{ID: 9}, State: forgeapi.CheckFailing}}}, nil
	}}
	list, err := client(t, f).RunsSince(t.Context(), ghRepo, time.Now())
	if err != nil || list.End != forge.EndWhole || len(list.Rows) != 1 || calls != 2 {
		t.Errorf("RunsSince with page 1 empty under the bound = %d runs ending %v calls %d, %v, want run 9 from page 2, whole, 2 calls", len(list.Rows), list.End, calls, err)
	}
}

func TestRunsSince_follows_the_continuation(t *testing.T) {
	calls := 0
	f := &fakeAPI{runs: func(opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Run], error) {
		calls++
		if settings(t, opts).After == "" {
			return forgeapi.Page[forgeapi.Run]{
				Items: []forgeapi.Run{{Ref: forgeapi.RunRef{ID: 1}, State: forgeapi.CheckPassing}},
				Next:  "p2", Partial: &forgeapi.Partial{Reason: forgeapi.PartialPaginationCap},
			}, nil
		}
		return forgeapi.Page[forgeapi.Run]{Items: []forgeapi.Run{{Ref: forgeapi.RunRef{ID: 2}, State: forgeapi.CheckPassing}}}, nil
	}}
	list, err := client(t, f).RunsSince(t.Context(), ghRepo, time.Now())
	if err != nil || len(list.Rows) != 2 || list.End != forge.EndWhole || calls != 2 {
		t.Errorf("RunsSince over two calls = %d runs ending %v calls %d, %v, want 2 runs, whole, 2 calls", len(list.Rows), list.End, calls, err)
	}
}

func TestRunsSince_only_giteas_undated_refusal_is_unsupported(t *testing.T) {
	refusing := func(code, message string) func(...forgeapi.ListOption) (forgeapi.Page[forgeapi.Run], error) {
		return func(...forgeapi.ListOption) (forgeapi.Page[forgeapi.Run], error) {
			return forgeapi.Page[forgeapi.Run]{}, &forgeapi.Error{Op: "ListRuns", Code: code, Kind: forgeapi.KindUpstream, Status: http.StatusOK, Message: message}
		}
	}
	gitea := forge.Repo{Path: "o/r"}.WithHandle(forgeapi.RepoRef{Family: forgeapi.FamilyGitea})
	c := client(t, &fakeAPI{product: forgeapi.ProductGitea, runs: refusing(forgeapi.CodeRunUndated, "no creation time")})
	if _, err := c.RunsSince(t.Context(), gitea, time.Now()); !errors.Is(err, forge.ErrRunsUndated) {
		t.Errorf("RunsSince on Gitea over a run_undated refusal = %v, want forge.ErrRunsUndated", err)
	}
	// forgeapi declares undated runs for Gitea before v28 alone, so on Forgejo
	// the refusal is a malformed answer, not a product limit.
	c = client(t, &fakeAPI{product: forgeapi.ProductForgejo, runs: refusing(forgeapi.CodeRunUndated, "no creation time")})
	if _, err := c.RunsSince(t.Context(), gitea, time.Now()); err == nil || errors.Is(err, forge.ErrRunsUndated) {
		t.Errorf("RunsSince on Forgejo over a run_undated refusal = %v, want a plain failure, never forge.ErrRunsUndated", err)
	}
	for producer, message := range map[string]string{
		"no_id":           "the run listing answered a run with no id, which is the run's identity",
		"negative_number": "the run listing answered a negative run number, which is no place in the repository's run sequence",
		"no_total_count":  "the run listing answered no total_count it can count by",
	} {
		c := client(t, &fakeAPI{product: forgeapi.ProductGitea, runs: refusing(forgeapi.CodeValidation, message)})
		if _, err := c.RunsSince(t.Context(), gitea, time.Now()); err == nil || errors.Is(err, forge.ErrRunsUndated) {
			t.Errorf("RunsSince on Gitea over a %s validation refusal = %v, want a plain failure, never forge.ErrRunsUndated", producer, err)
		}
	}
}

func TestMapErr_branches_on_kind_and_code(t *testing.T) {
	tests := []struct {
		err  *forgeapi.Error
		want error
		name string
	}{
		{name: "unauthorized", err: &forgeapi.Error{Kind: forgeapi.KindUnauthorized, Status: 401, Message: "rate limited"}, want: forge.ErrTokenInvalid},
		{name: "rate_limited", err: &forgeapi.Error{Kind: forgeapi.KindRateLimited, Status: 429, Code: forgeapi.CodeValidation}, want: forge.ErrRateLimited},
		{name: "rate_limited_codeless", err: &forgeapi.Error{Kind: forgeapi.KindRateLimited, Status: 429}, want: forge.ErrRateLimited},
		{name: "deferred", err: &forgeapi.Error{Kind: forgeapi.KindRateLimited}, want: forge.ErrReadDeferred},
		{name: "owner_unresolved", err: &forgeapi.Error{Kind: forgeapi.KindNotFound, Code: forgeapi.CodeOwnerUnresolved, Status: 404}, want: forge.ErrOwnerUnresolved},
		{name: "scope", err: &forgeapi.Error{Kind: forgeapi.KindForbidden, Code: forgeapi.CodeScopeInsufficient, Status: 403}, want: forge.ErrForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapErr(tc.err); !errors.Is(got, tc.want) {
				t.Errorf("mapErr(%+v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	plain := &forgeapi.Error{Kind: forgeapi.KindTransient, Status: 502, Message: "unauthorized"}
	for _, s := range []error{forge.ErrTokenInvalid, forge.ErrRateLimited, forge.ErrForbidden, forge.ErrOwnerUnresolved} {
		if errors.Is(mapErr(plain), s) {
			t.Errorf("mapErr(transient 502 whose message says unauthorized) = %v, matched %v: the message is never classified on", mapErr(plain), s)
		}
	}
	if !errors.Is(mapErr(context.Canceled), context.Canceled) {
		t.Error("mapErr(context.Canceled) lost the context error")
	}
}

func prPage(n int, partial *forgeapi.Partial) forgeapi.Page[forgeapi.PullRequest] {
	return forgeapi.Page[forgeapi.PullRequest]{Partial: partial, Items: []forgeapi.PullRequest{{
		Ref: forgeapi.PRRef{Number: n, Sigil: "!"}, Repo: forgeapi.RepoRef{DisplayPath: "group/sub/r"}, Title: "t",
		Author: "a", WebURL: "u", HeadSHA: "abc", Labels: []forgeapi.Label{{Name: "bug"}}, Draft: true,
	}}}
}

func TestOpenPRs_maps_rows(t *testing.T) {
	var sent forgeapi.ListSettings
	f := &fakeAPI{myPRs: func(opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.PullRequest], error) {
		sent = settings(t, opts)
		return prPage(7, nil), nil
	}}
	prs, err := client(t, f).OpenPRs(t.Context(), "o")
	if err != nil || len(prs) != 1 {
		t.Fatalf("OpenPRs = %d rows, %v, want 1", len(prs), err)
	}
	if !sent.OwnerSet || sent.Owner != "o" {
		t.Errorf("OpenPRs sent owner %q (set %t), want o", sent.Owner, sent.OwnerSet)
	}
	pr := prs[0]
	if pr.Ref != "!7" || pr.Number != 7 || pr.Repo != "group/sub/r" || pr.HeadSHA != "abc" || !pr.Draft || len(pr.Labels) != 1 || pr.Labels[0] != "bug" {
		t.Errorf("OpenPRs row = %+v, want ref !7 in group/sub/r with head abc, draft, label bug", pr)
	}
	if ref, ok := pr.RepoHandle().(forgeapi.RepoRef); !ok || ref.DisplayPath != "group/sub/r" {
		t.Errorf("OpenPRs row repo handle = %v, want the row's own repository address", pr.RepoHandle())
	}
}

func TestOpenPRs_partial_answers(t *testing.T) {
	tests := []struct {
		pages []forgeapi.Page[forgeapi.PullRequest]
		want  error
		name  string
		rows  int
	}{
		{name: "graphql_partial_then_whole", pages: []forgeapi.Page[forgeapi.PullRequest]{prPage(1, &forgeapi.Partial{Reason: forgeapi.PartialGraphQLPartial}), prPage(2, nil)}, rows: 1},
		{name: "graphql_partial_twice", pages: []forgeapi.Page[forgeapi.PullRequest]{prPage(1, &forgeapi.Partial{Reason: forgeapi.PartialGraphQLPartial}), prPage(2, &forgeapi.Partial{Reason: forgeapi.PartialGraphQLPartial})}, want: forge.ErrSnapshotPartial},
		{name: "result_window", pages: []forgeapi.Page[forgeapi.PullRequest]{prPage(1, &forgeapi.Partial{Reason: forgeapi.PartialResultWindow})}, want: forge.ErrSnapshotPartial},
		{name: "budget", pages: []forgeapi.Page[forgeapi.PullRequest]{prPage(1, &forgeapi.Partial{Reason: forgeapi.PartialBudget})}, want: forge.ErrSnapshotPartial},
		{name: "rate_limited_deferral", pages: []forgeapi.Page[forgeapi.PullRequest]{prPage(1, &forgeapi.Partial{Reason: forgeapi.PartialRateLimited})}, want: forge.ErrReadDeferred},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			call := 0
			f := &fakeAPI{budget: forgeapi.BudgetState{Remaining: 200}, myPRs: func(...forgeapi.ListOption) (forgeapi.Page[forgeapi.PullRequest], error) {
				p := tc.pages[min(call, len(tc.pages)-1)]
				call++
				return p, nil
			}}
			prs, err := client(t, f).OpenPRs(t.Context(), "o")
			if tc.want != nil {
				if !errors.Is(err, tc.want) || prs != nil {
					t.Errorf("OpenPRs(%s) = %d rows, %v, want no rows and %v", tc.name, len(prs), err, tc.want)
				}
				return
			}
			if err != nil || len(prs) != tc.rows || prs[0].Number != 2 {
				t.Errorf("OpenPRs(%s) = %+v, %v, want the re-read's one row", tc.name, prs, err)
			}
		})
	}
}

// A GitLab merge request whose label page stopped short may hide an excluded
// label, so the listing is a partial snapshot; elsewhere an item partial is
// the folded check, which the app reads separately.
func TestRepoOpenPRs_a_cut_label_list_is_partial_on_gitlab(t *testing.T) {
	cut := func(...forgeapi.ListOption) (forgeapi.Page[forgeapi.PullRequest], error) {
		p := prPage(1, nil)
		p.Items[0].Partial = &forgeapi.Partial{Reason: forgeapi.PartialPaginationCap, Fetched: 1, OmittedAtLeast: 1}
		return p, nil
	}
	gl := forge.Repo{Path: "group/sub/r"}.WithHandle(forgeapi.RepoRef{Family: forgeapi.FamilyGitLab, DisplayPath: "group/sub/r"})
	prs, err := client(t, &fakeAPI{product: forgeapi.ProductGitLab, listPRs: cut}).RepoOpenPRs(t.Context(), gl)
	if !errors.Is(err, forge.ErrSnapshotPartial) || prs != nil {
		t.Errorf("RepoOpenPRs on GitLab over a cut label list = %d rows, %v, want no rows and forge.ErrSnapshotPartial", len(prs), err)
	}
	if _, err := client(t, &fakeAPI{product: forgeapi.ProductGitLab, myPRs: cut}).OpenPRs(t.Context(), "o"); !errors.Is(err, forge.ErrSnapshotPartial) {
		t.Errorf("OpenPRs on GitLab over a cut label list = %v, want forge.ErrSnapshotPartial", err)
	}
	prs, err = client(t, &fakeAPI{product: forgeapi.ProductGitHub, listPRs: cut}).RepoOpenPRs(t.Context(), ghRepo)
	if err != nil || len(prs) != 1 {
		t.Errorf("RepoOpenPRs on GitHub over a cut check fold = %d rows, %v, want the row", len(prs), err)
	}
}

func TestOpenPRs_forge_refusal_is_rate_limited_not_deferred(t *testing.T) {
	f := &fakeAPI{budget: forgeapi.BudgetState{Remaining: 0}, myPRs: func(...forgeapi.ListOption) (forgeapi.Page[forgeapi.PullRequest], error) {
		return prPage(1, &forgeapi.Partial{Reason: forgeapi.PartialRateLimited}), nil
	}}
	if _, err := client(t, f).OpenPRs(t.Context(), "o"); !errors.Is(err, forge.ErrRateLimited) {
		t.Errorf("OpenPRs with the budget spent = %v, want forge.ErrRateLimited", err)
	}
}

func TestOpenPRs_ceiling_is_partial(t *testing.T) {
	f := &fakeAPI{myPRs: func(...forgeapi.ListOption) (forgeapi.Page[forgeapi.PullRequest], error) {
		p := prPage(1, &forgeapi.Partial{Reason: forgeapi.PartialPaginationCap})
		p.Next = "more"
		return p, nil
	}}
	if _, err := client(t, f).OpenPRs(t.Context(), "o"); !errors.Is(err, forge.ErrSnapshotPartial) {
		t.Errorf("OpenPRs past the page ceiling = %v, want forge.ErrSnapshotPartial", err)
	}
}

func TestDiscover_owner_unresolved_and_truncation(t *testing.T) {
	f := &fakeAPI{repos: func(opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Repository], error) {
		switch settings(t, opts).Owner {
		case "ghost":
			return forgeapi.Page[forgeapi.Repository]{}, &forgeapi.Error{Kind: forgeapi.KindNotFound, Code: forgeapi.CodeOwnerUnresolved, Status: 404}
		case "big":
			return forgeapi.Page[forgeapi.Repository]{
				Items:   []forgeapi.Repository{{Ref: forgeapi.RepoRef{DisplayPath: "big/a"}}},
				Partial: &forgeapi.Partial{Reason: forgeapi.PartialResultWindow},
			}, nil
		default:
			return forgeapi.Page[forgeapi.Repository]{Items: []forgeapi.Repository{{
				Ref: forgeapi.RepoRef{DisplayPath: "o/r"}, WebURL: "w", Archived: true, Fork: true, Private: true,
				Affordances: forgeapi.RepoAffordances{DefaultBranch: "trunk"},
			}}}, nil
		}
	}}
	d, err := client(t, f).Discover(t.Context(), []string{"o", "ghost", "big"})
	if err != nil {
		t.Fatalf("Discover = %v", err)
	}
	if len(d.Unresolved) != 1 || d.Unresolved[0] != "ghost" || d.End != forge.EndCut {
		t.Errorf("Discover = unresolved %v ending %v, want [ghost] and cut", d.Unresolved, d.End)
	}
	r := d.ByOwner["o"][0]
	if r.Path != "o/r" || r.DefaultBranch != "trunk" || !r.Archived || !r.Fork || !r.Private {
		t.Errorf("Discover row = %+v, want o/r on trunk, archived, fork, private", r)
	}
	if len(d.ByOwner["big"]) != 1 {
		t.Errorf("Discover kept %d rows of the cut listing, want the 1 read", len(d.ByOwner["big"]))
	}
}

func TestCommitChecks_folds_the_head(t *testing.T) {
	var asked string
	f := &fakeAPI{status: func(ref string) (forgeapi.CommitChecks, error) {
		asked = ref
		return forgeapi.CommitChecks{State: forgeapi.CheckFailing, Partial: &forgeapi.Partial{Reason: forgeapi.PartialPaginationCap}}, nil
	}}
	pr := forge.PullRequest{HeadSHA: "abc"}
	pr.SetRepoHandle(forgeapi.RepoRef{})
	got, err := client(t, f).CommitChecks(t.Context(), &pr)
	if err != nil || got.State != "failing" || got.End != forge.EndCut || asked != "abc" {
		t.Errorf("CommitChecks = %+v, %v (asked %q), want failing, cut, asked for abc", got, err, asked)
	}
	if _, err := client(t, f).CommitChecks(t.Context(), &forge.PullRequest{HeadSHA: "abc"}); err == nil {
		t.Error("CommitChecks on a row with no repository address = nil error, want a refusal")
	}
}

func TestCommitChecks_tells_no_checks_from_an_unread_fold(t *testing.T) {
	cut := &forgeapi.Partial{Reason: forgeapi.PartialPaginationCap}
	for _, tc := range []struct {
		name string
		cc   forgeapi.CommitChecks
		want forge.CheckResult
	}{
		{"no_contexts", forgeapi.CommitChecks{State: forgeapi.CheckUnknown}, forge.CheckResult{State: forge.CheckNone, End: forge.EndWhole}},
		{"unmapped_context", forgeapi.CommitChecks{State: forgeapi.CheckUnknown, Unknown: 1, Total: 1}, forge.CheckResult{State: forge.CheckUnknown, End: forge.EndWhole}},
		{"cut_before_any_context", forgeapi.CommitChecks{State: forgeapi.CheckUnknown, Partial: cut}, forge.CheckResult{State: forge.CheckUnknown, End: forge.EndCut}},
		{"passing", forgeapi.CommitChecks{State: forgeapi.CheckPassing, Passing: 2, Total: 2}, forge.CheckResult{State: forge.CheckPassing, End: forge.EndWhole}},
		{"refused_to_the_token", forgeapi.CommitChecks{State: forgeapi.CheckUnknown, Partial: &forgeapi.Partial{Reason: forgeapi.PartialForbidden}}, forge.CheckResult{State: forge.CheckUnreadable, End: forge.EndWhole}},
		{"answered_beside_errors", forgeapi.CommitChecks{State: forgeapi.CheckUnknown, Partial: &forgeapi.Partial{Reason: forgeapi.PartialGraphQLPartial}}, forge.CheckResult{State: forge.CheckUnknown, End: forge.EndCut}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAPI{status: func(string) (forgeapi.CommitChecks, error) { return tc.cc, nil }}
			pr := forge.PullRequest{HeadSHA: "abc"}
			pr.SetRepoHandle(forgeapi.RepoRef{})
			got, err := client(t, f).CommitChecks(t.Context(), &pr)
			if err != nil || got != tc.want {
				t.Errorf("CommitChecks(%+v) = %+v, %v, want %+v", tc.cc, got, err, tc.want)
			}
		})
	}
}

// A row whose checks forgeapi marked refused in the listing is answered
// unreadable without a checks read, and every other row keeps its read.
func TestOpenPRs_a_row_whose_checks_the_forge_refused_reads_unreadable_unsent(t *testing.T) {
	reads := 0
	f := &fakeAPI{
		myPRs: func(...forgeapi.ListOption) (forgeapi.Page[forgeapi.PullRequest], error) {
			p := prPage(1, nil)
			refused := p.Items[0]
			refused.Ref.Number, refused.Partial = 2, &forgeapi.Partial{Reason: forgeapi.PartialForbidden}
			p.Items = append(p.Items, refused)
			return p, nil
		},
		status: func(string) (forgeapi.CommitChecks, error) {
			reads++
			return forgeapi.CommitChecks{State: forgeapi.CheckPassing, Passing: 1, Total: 1}, nil
		},
	}
	c := client(t, f)
	prs, err := c.OpenPRs(t.Context(), "o")
	if err != nil || len(prs) != 2 || prs[0].ChecksRefused || !prs[1].ChecksRefused {
		t.Fatalf("OpenPRs = %+v, %v, want two rows, the second's checks refused", prs, err)
	}
	for i, want := range []forge.CheckState{forge.CheckPassing, forge.CheckUnreadable} {
		if got, err := c.CommitChecks(t.Context(), &prs[i]); err != nil || got.State != want || got.End != forge.EndWhole {
			t.Errorf("CommitChecks(#%d) = %+v, %v, want %s read whole", prs[i].Number, got, err, want)
		}
	}
	if reads != 1 {
		t.Errorf("checks reads sent = %d, want 1: the refused row needs none", reads)
	}
}

func TestBudget_reports_unknown_as_negative(t *testing.T) {
	reset := time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC)
	if b := client(t, &fakeAPI{budget: forgeapi.BudgetState{Remaining: forgeapi.BudgetRemainingUnknown}}).Budget(); b.Known() {
		t.Errorf("Budget(unknown) = %+v, want unknown", b)
	}
	if b := client(t, &fakeAPI{budget: forgeapi.BudgetState{Remaining: 4000, Reset: reset}}).Budget(); b.Remaining != 4000 || !b.Reset.Equal(reset) {
		t.Errorf("Budget = %+v, want 4000 resetting at %v", b, reset)
	}
}

// refusingMeter is an armed quota meter that observed a GitHub 403 with no
// rate-limit header.
func refusingMeter(t *testing.T) *ghquota.Meter {
	t.Helper()
	m := ghquota.New(nil)
	m.Arm()
	refused := roundTrip(func(*http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Set("X-RateLimit-Remaining", "4000")
		return &http.Response{StatusCode: http.StatusForbidden, Header: h, Body: http.NoBody}, nil
	})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.test/", http.NoBody)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if resp, err := m.Observe(refused).RoundTrip(req); err == nil {
		resp.Body.Close()
	}
	return m
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEvery_read_on_github_waits_for_the_quota_meter(t *testing.T) {
	// Every read field is unset, so a read the meter refused panics.
	c := client(t, &fakeAPI{})
	c.meter = refusingMeter(t)
	pr := forge.PullRequest{HeadSHA: "abc"}
	pr.SetRepoHandle(forgeapi.RepoRef{})
	reads := map[string]func() error{
		"Discover":   func() error { _, err := c.Discover(t.Context(), []string{"o"}); return err },
		"OpenPRs":    func() error { _, err := c.OpenPRs(t.Context(), "o"); return err },
		"OpenIssues": func() error { _, err := c.OpenIssues(t.Context(), "o"); return err },
		"RepoOpenPRs": func() error {
			_, err := c.RepoOpenPRs(t.Context(), ghRepo)
			return err
		},
		"RunsSince":    func() error { _, err := c.RunsSince(t.Context(), ghRepo, time.Now()); return err },
		"CommitChecks": func() error { _, err := c.CommitChecks(t.Context(), &pr); return err },
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			if err := read(); !errors.Is(err, forge.ErrRefused) {
				t.Errorf("%s after GitHub refused a read = %v, want forge.ErrRefused and nothing sent", name, err)
			}
		})
	}
}
