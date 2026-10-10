package forgeconn

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/github-scout/internal/forgeconn/forgeconntest"
	"github.com/cplieger/github-scout/internal/ghquota"
)

var wireNow = time.Now().UTC().Truncate(time.Second)

// world is the instance every product's fake serves in these tests.
func world() forgeconntest.World {
	return forgeconntest.World{
		Login: "me", Owner: "acme",
		Repos: []forgeconntest.Repo{
			{
				Name: "api", DefaultBranch: "main",
				Checks: map[string]string{"aaa111": "failure"},
				Runs: []forgeconntest.Run{
					{
						ID: 30, Number: 3, Workflow: "ci", Branch: "main", Event: "push", Outcome: "failure",
						Created: wireNow.Add(-time.Hour), Started: wireNow.Add(-59 * time.Minute), Updated: wireNow.Add(-50 * time.Minute),
					},
					{
						ID: 31, Number: 4, Workflow: "ci", Branch: "main", Event: "push", Outcome: "pending",
						Created: wireNow.Add(-10 * time.Minute), Started: wireNow.Add(-9 * time.Minute), Updated: wireNow.Add(-9 * time.Minute),
					},
					{
						ID: 10, Number: 1, Workflow: "ci", Branch: "main", Event: "schedule", Outcome: "success",
						Created: wireNow.Add(-100 * time.Hour), Started: wireNow.Add(-100 * time.Hour), Updated: wireNow.Add(-99 * time.Hour),
					},
				},
				PRs: []forgeconntest.Item{{
					Number: 7, Title: "Add thing", Author: "someone", HeadSHA: "aaa111", Labels: []string{"feature"},
					Created: wireNow.Add(-48 * time.Hour), Updated: wireNow.Add(-2 * time.Hour),
				}},
				Issues: []forgeconntest.Item{{
					Number: 8, Title: "Broken", Author: "me", Labels: []string{"bug"},
					Created: wireNow.Add(-72 * time.Hour), Updated: wireNow.Add(-3 * time.Hour),
				}},
			},
			{Name: "old", DefaultBranch: "master", Archived: true},
		},
	}
}

func openFake(t *testing.T, product forgeconntest.Product, w forgeconntest.World) (*Client, *forgeconntest.Server) {
	t.Helper()
	srv := forgeconntest.New(product, w)
	t.Cleanup(srv.Close)
	conn := srv.Connection("test")
	c, err := Open(t.Context(), &conn, slog.New(slog.NewTextHandler(io.Discard, nil)), ghquota.New(nil))
	if err != nil {
		t.Fatalf("Open(%s fake) = %v; requests %v", product, err, srv.Requests())
	}
	t.Cleanup(c.Close)
	return c, srv
}

func TestWire_each_product_reads_through_forgeapi(t *testing.T) {
	products := map[forgeconntest.Product]forge.Product{
		forgeconntest.GitHub: forge.ProductGitHub, forgeconntest.GitLab: forge.ProductGitLab,
		forgeconntest.Gitea: forge.ProductGitea, forgeconntest.Forgejo: forge.ProductForgejo,
	}
	for fake, want := range products {
		t.Run(string(fake), func(t *testing.T) {
			c, srv := openFake(t, fake, world())
			t.Logf("connection setup on %s sent %d request(s): %v", fake, len(srv.Requests()), srv.Requests())
			if c.Product() != want || c.Login() != "me" {
				t.Errorf("Open(%s) = product %v login %q, want %v me", fake, c.Product(), c.Login(), want)
			}
			d, err := c.Discover(t.Context(), []string{"acme"})
			if err != nil {
				t.Fatalf("Discover = %v; unhandled %v", err, srv.Unhandled())
			}
			repos := d.ByOwner["acme"]
			if len(repos) != 2 || d.End != forge.EndWhole || len(d.Unresolved) != 0 {
				t.Fatalf("Discover = %+v, want the owner's two repositories, complete", d)
			}
			api := repos[slices.IndexFunc(repos, func(r forge.Repo) bool { return r.Path == "acme/api" })]
			if api.DefaultBranch != "main" {
				t.Errorf("Discover acme/api default branch = %q, want main", api.DefaultBranch)
			}
			old := repos[slices.IndexFunc(repos, func(r forge.Repo) bool { return r.Path == "acme/old" })]
			if !old.Archived {
				t.Error("Discover acme/old is not archived, want the archived flag carried")
			}
			prs, err := c.OpenPRs(t.Context(), "acme")
			if err != nil || len(prs) != 1 {
				t.Fatalf("OpenPRs = %+v, %v; unhandled %v", prs, err, srv.Unhandled())
			}
			if prs[0].Repo != "acme/api" || prs[0].Number != 7 || prs[0].Author != "someone" || !slices.Equal(prs[0].Labels, []string{"feature"}) {
				t.Errorf("OpenPRs row = %+v, want acme/api 7 by someone labelled feature", prs[0])
			}
			issues, err := c.OpenIssues(t.Context(), "acme")
			if err != nil || len(issues) != 1 || issues[0].Repo != "acme/api" || issues[0].Number != 8 {
				t.Fatalf("OpenIssues = %+v, %v, want acme/api 8", issues, err)
			}
			runs, err := c.RunsSince(t.Context(), api, wireNow.Add(-72*time.Hour))
			if err != nil {
				t.Fatalf("RunsSince = %v; unhandled %v", err, srv.Unhandled())
			}
			if len(runs.Rows) != 1 || runs.Rows[0].ID != 30 || runs.Rows[0].State != forge.RunFailing || runs.Rows[0].Workflow != "ci" {
				t.Errorf("RunsSince = %+v, want run 30 alone: 10 is older than the bound and 31 is pending", runs.Rows)
			}
			if len(srv.Unhandled()) != 0 {
				t.Errorf("the %s fake was asked for routes it does not serve: %v", fake, srv.Unhandled())
			}
		})
	}
}

func TestWire_run_listing_sends_the_server_filter_where_one_exists(t *testing.T) {
	for fake, key := range map[forgeconntest.Product]string{forgeconntest.GitHub: "created=", forgeconntest.GitLab: "created_after="} {
		t.Run(string(fake), func(t *testing.T) {
			c, srv := openFake(t, fake, world())
			d, err := c.Discover(t.Context(), []string{"acme"})
			if err != nil {
				t.Fatalf("Discover = %v", err)
			}
			if _, err := c.RunsSince(t.Context(), d.ByOwner["acme"][0], wireNow.Add(-72*time.Hour)); err != nil {
				t.Fatalf("RunsSince = %v", err)
			}
			if !slices.ContainsFunc(srv.Requests(), func(r string) bool {
				return strings.Contains(r, "/runs?") && strings.Contains(r, key) || strings.Contains(r, "/pipelines?") && strings.Contains(r, key)
			}) {
				t.Errorf("the %s run listing sent no %s filter: %v", fake, key, srv.Requests())
			}
		})
	}
}

func TestWire_a_gitea_run_history_past_the_page_ceiling_reads_whole(t *testing.T) {
	for _, fake := range []forgeconntest.Product{forgeconntest.Gitea, forgeconntest.Forgejo} {
		t.Run(string(fake), func(t *testing.T) {
			w := world()
			fresh := w.Repos[0].Runs[0]
			fresh.ID = 3000
			w.Repos[0].Runs = []forgeconntest.Run{fresh}
			for i := range 2100 {
				old := wireNow.Add(-240*time.Hour - time.Duration(i)*time.Minute)
				w.Repos[0].Runs = append(w.Repos[0].Runs, forgeconntest.Run{
					ID: int64(2100 - i), Number: int64(2100 - i), Workflow: "ci", Branch: "main", Event: "push", Outcome: "success",
					Created: old, Started: old, Updated: old.Add(time.Minute),
				})
			}
			c, srv := openFake(t, fake, w)
			d, err := c.Discover(t.Context(), []string{"acme"})
			if err != nil {
				t.Fatalf("Discover = %v", err)
			}
			before := len(srv.Requests())
			runs, err := c.RunsSince(t.Context(), d.ByOwner["acme"][0], wireNow.Add(-72*time.Hour))
			if err != nil || runs.End == forge.EndCut || len(runs.Rows) != 1 || runs.Rows[0].ID != fresh.ID {
				t.Fatalf("RunsSince over 2,101 runs, 2,100 older than the bound = %+v, %v, want run %d alone, not truncated", runs, err, fresh.ID)
			}
			listed := 0
			for _, r := range srv.Requests()[before:] {
				if strings.Contains(r, "actions/runs") {
					listed++
				}
			}
			if listed > 2 {
				t.Errorf("RunsSince read %d run pages, want at most 2: forgeapi ends the walk at the first page wholly older than the bound", listed)
			}
		})
	}
}

func TestWire_a_gitea_run_below_a_page_older_than_the_bound_within_a_minute_is_read(t *testing.T) {
	bound := wireNow.Add(-72 * time.Hour)
	for _, fake := range []forgeconntest.Product{forgeconntest.Gitea, forgeconntest.Forgejo} {
		t.Run(string(fake), func(t *testing.T) {
			w := world()
			late := w.Repos[0].Runs[0]
			late.ID, late.Number, late.Outcome = 1, 1, "failure"
			w.Repos[0].Runs = nil
			for i := range 50 {
				stepped := bound.Add(-30 * time.Second)
				w.Repos[0].Runs = append(w.Repos[0].Runs, forgeconntest.Run{
					ID: int64(100 + i), Number: int64(100 + i), Workflow: "ci", Branch: "main", Event: "push", Outcome: "success",
					Created: stepped, Started: stepped, Updated: stepped.Add(time.Minute),
				})
			}
			w.Repos[0].Runs = append(w.Repos[0].Runs, late)
			c, _ := openFake(t, fake, w)
			d, err := c.Discover(t.Context(), []string{"acme"})
			if err != nil {
				t.Fatalf("Discover = %v", err)
			}
			runs, err := c.RunsSince(t.Context(), d.ByOwner["acme"][0], bound)
			if err != nil || runs.End == forge.EndCut || len(runs.Rows) != 1 || runs.Rows[0].ID != 1 {
				t.Errorf("RunsSince over a page of 50 runs 30s before the bound above run 1 created after it = %+v, %v, want run 1, not truncated", runs, err)
			}
		})
	}
}

func TestWire_start_times_reach_the_run(t *testing.T) {
	for fake, want := range map[forgeconntest.Product]bool{
		forgeconntest.GitHub: true, forgeconntest.GitLab: false, forgeconntest.Gitea: true, forgeconntest.Forgejo: true,
	} {
		t.Run(string(fake), func(t *testing.T) {
			c, _ := openFake(t, fake, world())
			d, err := c.Discover(t.Context(), []string{"acme"})
			if err != nil {
				t.Fatalf("Discover = %v", err)
			}
			runs, err := c.RunsSince(t.Context(), d.ByOwner["acme"][0], wireNow.Add(-72*time.Hour))
			if err != nil || len(runs.Rows) != 1 {
				t.Fatalf("RunsSince = %+v, %v", runs, err)
			}
			if got := !runs.Rows[0].StartedAt.IsZero(); got != want {
				t.Errorf("RunsSince on %s carries a start time %t, want %t", fake, got, want)
			}
		})
	}
}

func TestWire_pr_head_and_checks(t *testing.T) {
	for fake, hasHead := range map[forgeconntest.Product]bool{
		forgeconntest.GitHub: true, forgeconntest.GitLab: true, forgeconntest.Gitea: false, forgeconntest.Forgejo: false,
	} {
		t.Run(string(fake), func(t *testing.T) {
			c, _ := openFake(t, fake, world())
			prs, err := c.OpenPRs(t.Context(), "acme")
			if err != nil || len(prs) != 1 {
				t.Fatalf("OpenPRs = %v, %v", prs, err)
			}
			if (prs[0].HeadSHA != "") != hasHead {
				t.Fatalf("OpenPRs on %s head %q, want a head %t", fake, prs[0].HeadSHA, hasHead)
			}
			if c.Product().PRHeads() != hasHead {
				t.Errorf("%s.PRHeads() = %t, want %t, what its rows carry", c.Product(), c.Product().PRHeads(), hasHead)
			}
			if !hasHead {
				return
			}
			got, err := c.CommitChecks(t.Context(), &prs[0])
			if err != nil || got.State != "failing" {
				t.Errorf("CommitChecks on %s = %+v, %v, want failing", fake, got, err)
			}
		})
	}
}

func TestWire_a_head_with_no_checks_reads_none(t *testing.T) {
	for _, fake := range []forgeconntest.Product{forgeconntest.GitHub, forgeconntest.GitLab} {
		t.Run(string(fake), func(t *testing.T) {
			w := world()
			w.Repos[0].Checks = nil
			c, _ := openFake(t, fake, w)
			prs, err := c.OpenPRs(t.Context(), "acme")
			if err != nil || len(prs) != 1 {
				t.Fatalf("OpenPRs = %v, %v", prs, err)
			}
			got, err := c.CommitChecks(t.Context(), &prs[0])
			if err != nil || got.State != forge.CheckNone || got.End != forge.EndWhole {
				t.Errorf("CommitChecks on %s with no checks = %+v, %v, want none read whole", fake, got, err)
			}
		})
	}
}

func TestWire_pr_ref_carries_the_product_sigil(t *testing.T) {
	for fake, want := range map[forgeconntest.Product]string{
		forgeconntest.GitHub: "#7", forgeconntest.GitLab: "!7", forgeconntest.Gitea: "#7", forgeconntest.Forgejo: "#7",
	} {
		t.Run(string(fake), func(t *testing.T) {
			c, _ := openFake(t, fake, world())
			prs, err := c.OpenPRs(t.Context(), "acme")
			if err != nil || len(prs) != 1 || prs[0].Ref != want {
				t.Errorf("OpenPRs on %s ref = %v, %v, want %s", fake, prs, err, want)
			}
		})
	}
}

func TestWire_unknown_owner_is_unresolved(t *testing.T) {
	for _, fake := range []forgeconntest.Product{forgeconntest.GitHub, forgeconntest.GitLab, forgeconntest.Gitea, forgeconntest.Forgejo} {
		t.Run(string(fake), func(t *testing.T) {
			c, _ := openFake(t, fake, world())
			d, err := c.Discover(t.Context(), []string{"nobody"})
			if err != nil || len(d.Unresolved) != 1 {
				t.Errorf("Discover(nobody) on %s = %+v, %v, want nobody unresolved", fake, d, err)
			}
		})
	}
}

// TestWire_an_owner_with_no_repository_lists_none lists an owner that
// resolves and owns no repository, a GitLab user whose profile is private
// among them: forgeapi answers it as owning nothing.
func TestWire_an_owner_with_no_repository_lists_none(t *testing.T) {
	tests := []struct {
		name string
		fake forgeconntest.Product
		user bool
	}{
		{"github", forgeconntest.GitHub, false},
		{"gitlab_group", forgeconntest.GitLab, false},
		{"gitlab_user", forgeconntest.GitLab, true},
		{"gitea", forgeconntest.Gitea, false},
		{"forgejo", forgeconntest.Forgejo, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := world()
			w.Repos, w.UserNamespace = nil, tc.user
			c, srv := openFake(t, tc.fake, w)
			d, err := c.Discover(t.Context(), []string{"acme"})
			repos, listed := d.ByOwner["acme"]
			if err != nil || !listed || len(repos) != 0 || len(d.Unresolved) != 0 || d.End != forge.EndWhole {
				t.Errorf("Discover(acme owning nothing) on %s = %+v, %v, want acme listed with no repository, resolved and whole; unhandled %v", tc.name, d, err, srv.Unhandled())
			}
		})
	}
}

func TestWire_graphql_partial_search_is_a_partial_snapshot(t *testing.T) {
	w := world()
	w.GraphQLErrors = true
	c, srv := openFake(t, forgeconntest.GitHub, w)
	if _, err := c.OpenPRs(t.Context(), "acme"); !errors.Is(err, forge.ErrSnapshotPartial) {
		t.Errorf("OpenPRs over a search answering data and errors = %v, want forge.ErrSnapshotPartial", err)
	}
	searches := 0
	for _, r := range srv.Requests() {
		if r == "POST graphql PRMine" {
			searches++
		}
	}
	if searches != 2 {
		t.Errorf("OpenPRs read the search %d time(s), want 2: one re-read on a partial answer", searches)
	}
}

func TestWire_governor_defers_below_the_read_reserve(t *testing.T) {
	w := world()
	w.Remaining = forge.ReadReserve - 50
	srv := forgeconntest.New(forgeconntest.GitHub, w)
	t.Cleanup(srv.Close)
	conn := srv.Connection("test")
	_, err := Open(t.Context(), &conn, slog.New(slog.NewTextHandler(io.Discard, nil)), ghquota.New(nil))
	if !errors.Is(err, forge.ErrReadDeferred) || errors.Is(err, forge.ErrConnection) {
		t.Errorf("Open with %d left under a %d reserve = %v, want forge.ErrReadDeferred and not a connection failure", w.Remaining, forge.ReadReserve, err)
	}
	oe, ok := errors.AsType[*forge.OpenError](err)
	if !ok || oe.Product != forge.ProductGitHub || oe.Budget == nil || oe.Budget.Remaining != w.Remaining || oe.Budget.Reset.IsZero() {
		t.Errorf("Open with %d left under a %d reserve = %#v, want a *forge.OpenError naming github with the detection's budget %d and its reset", w.Remaining, forge.ReadReserve, err, w.Remaining)
	}
}

func TestWire_gitlab_user_namespace_answers_per_project(t *testing.T) {
	w := world()
	w.UserNamespace = true
	c, srv := openFake(t, forgeconntest.GitLab, w)
	d, err := c.Discover(t.Context(), []string{"acme"})
	if err != nil || len(d.ByOwner["acme"]) != 2 {
		t.Fatalf("Discover(user namespace) = %+v, %v, want the user's two projects", d, err)
	}
	if _, err := c.OpenPRs(t.Context(), "acme"); !errors.Is(err, forge.ErrOwnerUnresolved) {
		t.Errorf("OpenPRs(user namespace) = %v, want forge.ErrOwnerUnresolved", err)
	}
	api := d.ByOwner["acme"][slices.IndexFunc(d.ByOwner["acme"], func(r forge.Repo) bool { return r.Path == "acme/api" })]
	prs, err := c.RepoOpenPRs(t.Context(), api)
	if err != nil || len(prs) != 1 || prs[0].Ref != "!7" {
		t.Errorf("RepoOpenPRs = %+v, %v, want !7; unhandled %v", prs, err, srv.Unhandled())
	}
	issues, err := c.RepoOpenIssues(t.Context(), api)
	if err != nil || len(issues) != 1 || issues[0].Number != 8 {
		t.Errorf("RepoOpenIssues = %+v, %v, want #8; unhandled %v", issues, err, srv.Unhandled())
	}
}

func TestWire_gitlab_owner_named_by_digits_alone_is_resolved_by_name(t *testing.T) {
	for _, userNamespace := range []bool{true, false} {
		name := map[bool]string{true: "a_user", false: "a_group"}[userNamespace]
		t.Run(name, func(t *testing.T) {
			w := world()
			w.Owner, w.UserNamespace = "123", userNamespace
			c, srv := openFake(t, forgeconntest.GitLab, w)
			d, err := c.Discover(t.Context(), []string{"123"})
			if err != nil || len(d.ByOwner["123"]) != 2 || len(d.Unresolved) != 0 {
				t.Fatalf("Discover(123, %s) = %+v, %v, want its two projects; requests %v", name, d, err, srv.Requests())
			}
			if !slices.Contains(srv.Requests(), "POST graphql OwnerLookup") {
				t.Errorf("Discover(123, %s) requests %v, want the owner resolved by name first", name, srv.Requests())
			}
			prs, err := c.OpenPRs(t.Context(), "123")
			if userNamespace {
				if !errors.Is(err, forge.ErrOwnerUnresolved) {
					t.Errorf("OpenPRs(123, a user) = %v, %v, want forge.ErrOwnerUnresolved: a user has no owner-wide list", prs, err)
				}
				_, err = c.OpenIssues(t.Context(), "123")
				if !errors.Is(err, forge.ErrOwnerUnresolved) {
					t.Errorf("OpenIssues(123, a user) = %v, want forge.ErrOwnerUnresolved", err)
				}
			} else if err != nil || len(prs) != 1 || prs[0].Ref != "!7" {
				t.Errorf("OpenPRs(123, a group) = %+v, %v, want !7 from the group's list", prs, err)
			}
			if len(srv.Unhandled()) != 0 {
				t.Errorf("the GitLab fake was asked for routes it does not serve: %v", srv.Unhandled())
			}
		})
	}
}

func TestOpen_arms_the_quota_meter_on_github_before_its_account_read(t *testing.T) {
	w := world()
	w.Remaining = 4321
	srv := forgeconntest.New(forgeconntest.GitHub, w)
	t.Cleanup(srv.Close)
	conn := srv.Connection("test")
	m := ghquota.New(nil)
	c, err := Open(t.Context(), &conn, slog.New(slog.NewTextHandler(io.Discard, nil)), m)
	if err != nil {
		t.Fatalf("Open(github fake) = %v", err)
	}
	t.Cleanup(c.Close)
	if b := m.Budget(); b.Remaining != 4321 {
		t.Errorf("meter budget after Open = %+v, want 4321: the account read is observed, so a scan's first read is admitted against it", b)
	}
	for _, product := range []forgeconntest.Product{forgeconntest.GitLab, forgeconntest.Forgejo} {
		srv := forgeconntest.New(product, w)
		t.Cleanup(srv.Close)
		conn := srv.Connection("test")
		m := ghquota.New(nil)
		c, err := Open(t.Context(), &conn, slog.New(slog.NewTextHandler(io.Discard, nil)), m)
		if err != nil {
			t.Fatalf("Open(%s fake) = %v", product, err)
		}
		t.Cleanup(c.Close)
		if b := m.Budget(); b.Known() || c.meter != nil {
			t.Errorf("Open(%s) left the meter %+v and the client metered %t, want it inert: it is GitHub's quota", product, b, c.meter != nil)
		}
	}
}

// TestOpen_a_retried_read_charges_every_attempt_and_the_next_read_waits sends
// one run listing whose first attempt GitHub's edge answers 502 with no
// rate-limit header, as the second does: the meter charges both, so the
// listing that follows is held at the reserve before it is sent.
func TestOpen_a_retried_read_charges_every_attempt_and_the_next_read_waits(t *testing.T) {
	w := world()
	w.Remaining = forge.ReadReserve + 2
	srv := forgeconntest.New(forgeconntest.GitHub, w)
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	var strip, fail atomic.Bool
	var runCalls atomic.Int64
	front := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/actions/runs") {
			runCalls.Add(1)
			if fail.CompareAndSwap(true, false) {
				rw.WriteHeader(http.StatusBadGateway)
				return
			}
		}
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			if !strip.Load() || !strings.HasPrefix(k, "X-Ratelimit-") {
				rw.Header()[k] = v
			}
		}
		rw.WriteHeader(rec.Code)
		_, _ = rw.Write(rec.Body.Bytes())
	}))
	t.Cleanup(front.Close)
	conn := srv.Connection("test")
	conn.URL = front.URL
	m := ghquota.New(nil)
	c, err := Open(t.Context(), &conn, slog.New(slog.NewTextHandler(io.Discard, nil)), m)
	if err != nil {
		t.Fatalf("Setup: Open(github behind a proxy) = %v", err)
	}
	t.Cleanup(c.Close)
	d, err := c.Discover(t.Context(), conn.Owners)
	if err != nil || len(d.ByOwner[w.Owner]) == 0 {
		t.Fatalf("Setup: Discover = %+v, %v", d, err)
	}
	repo := d.ByOwner[w.Owner][0]
	before := m.Budget().Remaining
	if before != forge.ReadReserve+2 {
		t.Fatalf("Setup: meter budget before the listing = %d, want %d", before, forge.ReadReserve+2)
	}
	strip.Store(true)
	fail.Store(true)
	if _, err := c.RunsSince(t.Context(), repo, wireNow.Add(-72*time.Hour)); err != nil {
		t.Fatalf("RunsSince with one retried attempt = %v, want it read", err)
	}
	if got := runCalls.Load(); got != 2 {
		t.Fatalf("Setup: the listing sent %d attempts, want 2: the 502 and its retry", got)
	}
	if got := m.Budget().Remaining; got != before-2 {
		t.Errorf("meter budget after a listing of two attempts = %d, want %d: each attempt spends one", got, before-2)
	}
	if _, err := c.RunsSince(t.Context(), repo, wireNow.Add(-72*time.Hour)); !errors.Is(err, forge.ErrReadDeferred) {
		t.Errorf("RunsSince at the reserve = %v, want forge.ErrReadDeferred", err)
	}
	if got := runCalls.Load(); got != 2 {
		t.Errorf("the listing at the reserve sent %d more requests, want none", got-2)
	}
}
