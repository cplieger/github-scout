// Package forgeconn is forge-scout's one forgeapi adapter: it opens a
// connection, performs every forgeapi read the collector needs, and maps
// forgeapi's rows, partial markers and typed errors onto internal/forge.
// No other package reads a forge through forgeapi or holds its row types.
package forgeconn

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/cplieger/forgeapi"
	"github.com/cplieger/forgeapi/families"
	"github.com/cplieger/github-scout/internal/config"
	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/github-scout/internal/ghquota"
)

// Walk ceilings, in pages: each list call reads one page. Reaching one with a
// continuation left cuts the result.
const (
	maxRepoPages     = 10
	maxSnapshotPages = 40
	maxRepoItemPages = 10
	maxRunPages      = 40
)

// api is the forgeapi family client methods the adapter calls.
type api interface {
	ConnectionCaps(ctx context.Context) (forgeapi.ConnectionCaps, error)
	Whoami(ctx context.Context) (forgeapi.Account, error)
	BudgetState() forgeapi.BudgetState
	ListRepos(ctx context.Context, opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Repository], error)
	ListMyPRs(ctx context.Context, opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.PullRequest], error)
	ListMyIssues(ctx context.Context, opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Issue], error)
	ListPRs(ctx context.Context, repo forgeapi.RepoRef, opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.PullRequest], error)
	ListIssues(ctx context.Context, repo forgeapi.RepoRef, opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Issue], error)
	ListRuns(ctx context.Context, repo forgeapi.RepoRef, opts ...forgeapi.ListOption) (forgeapi.Page[forgeapi.Run], error)
	CommitStatus(ctx context.Context, repo forgeapi.RepoRef, ref string) (forgeapi.CommitChecks, error)
	Close()
}

// Client is one open connection.
type Client struct {
	api api
	// meter admits each call on a GitHub connection, nil on any other.
	meter   *ghquota.Meter
	login   string
	product forge.Product
}

// Open detects the connection's family and product, identifies its account
// and checks the configured owners against the family's owner form. A failure
// is wrapped in forge.ErrConnection unless it is a credential, budget or
// cancellation failure; one after the product is known is a *forge.OpenError
// carrying the product and the budget the open's responses reported. logger
// carries the connection: forgeapi's lines gain the product once it is named.
// meter accounts every attempt, retries included, and admits each call of a
// GitHub connection (see ghquota.Meter.Admit).
func Open(ctx context.Context, c *config.Connection, logger *slog.Logger, meter *ghquota.Meter) (*Client, error) {
	stamp := &forgeHandler{inner: logger.Handler(), product: new(atomic.Int64)}
	core, family, err := families.Open(ctx,
		forgeapi.Connection{WebBaseURL: c.URL, APIBaseURL: c.APIURL},
		forgeapi.WithCredentialSource(staticToken(c.Token)),
		forgeapi.WithMutations(false),
		forgeapi.WithMutationReserve(forge.ReadReserve),
		forgeapi.WithListPages(1),
		forgeapi.WithLogger(slog.New(stamp)),
		forgeapi.WithPrivateAddresses(c.PrivateAddresses),
		forgeapi.WithPlaintextHTTP(c.AllowPlaintext),
		forgeapi.WithAttemptObserver(meter.ObserveAttempt),
		forgeapi.WithRequestObserver(meter.ObserveRequest),
	)
	if err != nil {
		return nil, connectionError("open", err)
	}
	// Armed as soon as the product is known, the meter reads the budget from
	// the account read, before the first read of a scan.
	client, err := newClient(ctx, core, family, c.Owners, func(p forge.Product) {
		stamp.product.Store(int64(p))
		if p == forge.ProductGitHub {
			meter.Arm()
		}
	})
	if err != nil {
		if oe, ok := errors.AsType[*forge.OpenError](err); ok {
			b := budgetOf(core.BudgetState())
			oe.Budget = &b
		}
		core.Close()
		return nil, err
	}
	if client.product == forge.ProductGitHub {
		client.meter = meter
	}
	return client, nil
}

// forgeHandler adds the connection's forge to every record, read when the
// record is handled because the product is detected after the handler is
// handed to forgeapi.
type forgeHandler struct {
	inner   slog.Handler
	product *atomic.Int64
}

func (h *forgeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

//nolint:gocritic // hugeParam: slog.Handler fixes the by-value Record.
func (h *forgeHandler) Handle(ctx context.Context, r slog.Record) error {
	r = r.Clone()
	r.AddAttrs(slog.String("forge", forge.Product(h.product.Load()).String()))
	return h.inner.Handle(ctx, r)
}

func (h *forgeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &forgeHandler{inner: h.inner.WithAttrs(attrs), product: h.product}
}

func (h *forgeHandler) WithGroup(name string) slog.Handler {
	return &forgeHandler{inner: h.inner.WithGroup(name), product: h.product}
}

// newClient identifies the connection on a, calling named with the product
// as soon as the instance names it, before any later read.
func newClient(ctx context.Context, a api, family forgeapi.Family, owners []string, named func(forge.Product)) (*Client, error) {
	caps, err := a.ConnectionCaps(ctx)
	if err != nil {
		return nil, connectionError("connection capabilities", err)
	}
	product := productOf(caps.Product)
	if product == forge.ProductUnknown {
		return nil, fmt.Errorf("%w: the instance named no product", forge.ErrConnection)
	}
	named(product)
	for _, o := range owners {
		if forgeapi.ValidateOwner(family, o) != nil {
			return nil, &forge.OpenError{Product: product, Err: fmt.Errorf("%w: owner %q is not an owner name this %s instance takes", forge.ErrConnection, o, product)}
		}
	}
	account, err := a.Whoami(ctx)
	if err != nil {
		return nil, &forge.OpenError{Product: product, Err: connectionError("identify account", err)}
	}
	return &Client{api: a, login: account.Login, product: product}, nil
}

// Product is the product the connection answered as.
func (c *Client) Product() forge.Product { return c.product }

// Login is the account the token belongs to.
func (c *Client) Login() string { return c.login }

// Budget is the forge's last reported budget.
func (c *Client) Budget() forge.Budget { return budgetOf(c.api.BudgetState()) }

func budgetOf(b forgeapi.BudgetState) forge.Budget {
	if b.Remaining == forgeapi.BudgetRemainingUnknown {
		return forge.Budget{Remaining: -1, Reset: b.Reset}
	}
	return forge.Budget{Remaining: max(b.Remaining, 0), Reset: b.Reset}
}

// Close releases the connection's idle network connections.
func (c *Client) Close() { c.api.Close() }

// Discover lists each owner's repositories. An owner the instance does not
// resolve is listed in Unresolved, and one it answers with no repository
// owns none; a listing cut short ends EndCut and keeps what was read.
func (c *Client) Discover(ctx context.Context, owners []string) (forge.Discovery, error) {
	d := forge.Discovery{ByOwner: make(map[string][]forge.Repo, len(owners)), End: forge.EndWhole}
	for _, o := range owners {
		rows, w, err := walk(ctx, c, maxRepoPages, func(ctx context.Context, after forgeapi.Cursor) (forgeapi.Page[forgeapi.Repository], error) {
			return c.api.ListRepos(ctx, forgeapi.WithOwner(o), forgeapi.WithAfter(after))
		})
		if err != nil {
			mapped := mapErr(err)
			if errors.Is(mapped, forge.ErrOwnerUnresolved) {
				d.Unresolved = append(d.Unresolved, o)
				continue
			}
			return forge.Discovery{}, fmt.Errorf("list repositories of %q: %w", o, mapped)
		}
		if err := c.stopOnRateLimit(w); err != nil {
			return forge.Discovery{}, err
		}
		if w.cut() {
			d.End = forge.EndCut
		}
		repos := make([]forge.Repo, 0, len(rows))
		for i := range rows {
			repos = append(repos, repoOf(&rows[i]))
		}
		d.ByOwner[o] = repos
	}
	return d, nil
}

// OpenPRs lists the open pull requests under owner. A listing the forge
// answered partially is re-read once on a GraphQL partial answer and is
// otherwise forge.ErrSnapshotPartial: a cut snapshot is never answered.
func (c *Client) OpenPRs(ctx context.Context, owner string) ([]forge.PullRequest, error) {
	rows, err := snapshot(ctx, c, maxSnapshotPages, func(ctx context.Context, after forgeapi.Cursor) (forgeapi.Page[forgeapi.PullRequest], error) {
		return c.api.ListMyPRs(ctx, forgeapi.WithOwner(owner), forgeapi.WithAfter(after))
	})
	if err != nil {
		return nil, err
	}
	return c.prsOf(rows)
}

// OpenIssues lists the open issues under owner, on OpenPRs's terms.
func (c *Client) OpenIssues(ctx context.Context, owner string) ([]forge.Issue, error) {
	rows, err := snapshot(ctx, c, maxSnapshotPages, func(ctx context.Context, after forgeapi.Cursor) (forgeapi.Page[forgeapi.Issue], error) {
		return c.api.ListMyIssues(ctx, forgeapi.WithOwner(owner), forgeapi.WithAfter(after))
	})
	if err != nil {
		return nil, err
	}
	return issuesOf(rows), nil
}

// RepoOpenPRs lists one repository's open pull requests, on OpenPRs's terms.
func (c *Client) RepoOpenPRs(ctx context.Context, repo forge.Repo) ([]forge.PullRequest, error) {
	ref, err := repoRef(repo)
	if err != nil {
		return nil, err
	}
	rows, err := snapshot(ctx, c, maxRepoItemPages, func(ctx context.Context, after forgeapi.Cursor) (forgeapi.Page[forgeapi.PullRequest], error) {
		return c.api.ListPRs(ctx, ref, forgeapi.WithState(forgeapi.ListStateOpen), forgeapi.WithAfter(after))
	})
	if err != nil {
		return nil, err
	}
	return c.prsOf(rows)
}

// RepoOpenIssues lists one repository's open issues, on OpenPRs's terms.
func (c *Client) RepoOpenIssues(ctx context.Context, repo forge.Repo) ([]forge.Issue, error) {
	ref, err := repoRef(repo)
	if err != nil {
		return nil, err
	}
	rows, err := snapshot(ctx, c, maxRepoItemPages, func(ctx context.Context, after forgeapi.Cursor) (forgeapi.Page[forgeapi.Issue], error) {
		return c.api.ListIssues(ctx, ref, forgeapi.WithState(forgeapi.ListStateOpen), forgeapi.WithAfter(after))
	})
	if err != nil {
		return nil, err
	}
	return issuesOf(rows), nil
}

// RunsSince lists repo's runs created at or after since: the completed ones
// in Rows and the ones in a state forgeapi could not map in Unmapped, with
// the oldest creation time of any row and of a pending one. A listing cut
// short ends EndCut. A
// listing whose rows carry no creation time (Gitea before v28) is
// forge.ErrRunsUndated.
func (c *Client) RunsSince(ctx context.Context, repo forge.Repo, since time.Time) (forge.RunList, error) {
	ref, err := repoRef(repo)
	if err != nil {
		return forge.RunList{}, err
	}
	rows, w, err := walk(ctx, c, maxRunPages, func(ctx context.Context, after forgeapi.Cursor) (forgeapi.Page[forgeapi.Run], error) {
		return c.api.ListRuns(ctx, ref, forgeapi.WithSince(since), forgeapi.WithAfter(after))
	})
	if err != nil {
		if c.product == forge.ProductGitea && undated(err) {
			return forge.RunList{}, fmt.Errorf("%w: %v", forge.ErrRunsUndated, err)
		}
		return forge.RunList{}, mapErr(err)
	}
	if err := c.stopOnRateLimit(w); err != nil {
		return forge.RunList{}, err
	}
	list := forge.RunList{Listing: forge.Listing[forge.Run]{Rows: make([]forge.Run, 0, len(rows)), End: w.end()}}
	for i := range rows {
		r := &rows[i]
		earliest(&list.Oldest, r.CreatedAt)
		if r.State == forgeapi.CheckPending {
			earliest(&list.OldestPending, r.CreatedAt)
			continue
		}
		run, ok := runOf(r, repo.Path)
		if !ok {
			list.Unmapped = append(list.Unmapped, unstated(r, repo.Path))
			continue
		}
		list.Rows = append(list.Rows, run)
	}
	return list, nil
}

// earliest moves *oldest back to t when t is set and older.
func earliest(oldest *time.Time, t time.Time) {
	if !t.IsZero() && (oldest.IsZero() || t.Before(*oldest)) {
		*oldest = t
	}
}

// CommitChecks folds the checks of pr's head commit, CheckNone where a whole
// read found none. The caller asks only for a pull request whose HeadSHA is
// set.
func (c *Client) CommitChecks(ctx context.Context, pr *forge.PullRequest) (forge.CheckResult, error) {
	ref, ok := pr.RepoHandle().(forgeapi.RepoRef)
	if !ok {
		return forge.CheckResult{}, errors.New("pull request carries no repository address")
	}
	if err := c.admit(); err != nil {
		return forge.CheckResult{}, err
	}
	cc, err := c.api.CommitStatus(ctx, ref, pr.HeadSHA)
	if err != nil {
		return forge.CheckResult{}, mapErr(err)
	}
	if cc.Partial != nil && cc.Partial.Reason == forgeapi.PartialRateLimited {
		return forge.CheckResult{}, c.rateLimitError()
	}
	res := forge.CheckResult{State: checkStateOf(cc.State), End: forge.EndWhole}
	switch {
	case cc.Partial != nil:
		res.End = forge.EndCut
	case cc.State == forgeapi.CheckUnknown && cc.Total == 0:
		res.State = forge.CheckNone
	}
	return res, nil
}

// undated reports the refusal of a bounded run page whose rows carry no
// creation time, a departure forgeapi declares for Gitea before v28 alone.
func undated(err error) bool {
	var fe *forgeapi.Error
	return errors.As(err, &fe) && fe.Code == forgeapi.CodeRunUndated
}

// admit holds back a call the GitHub quota meter refuses; every other
// product admits all.
func (c *Client) admit() error {
	if c.meter == nil {
		return nil
	}
	return c.meter.Admit()
}

// stopOnRateLimit turns a walk that ended on a rate-limited page into the
// stop error.
func (c *Client) stopOnRateLimit(w walked) error {
	if w.partial != nil && w.partial.Reason == forgeapi.PartialRateLimited {
		return c.rateLimitError()
	}
	return nil
}

// rateLimitError tells a governor deferral from a forge refusal by the budget
// it left: a deferral holds back budget the forge still reports.
func (c *Client) rateLimitError() error {
	if c.api.BudgetState().Remaining > 0 {
		return forge.ErrReadDeferred
	}
	return forge.ErrRateLimited
}

// mapErr maps a forgeapi failure on its Kind and Code alone.
func mapErr(err error) error {
	var fe *forgeapi.Error
	if !errors.As(err, &fe) {
		return err
	}
	switch {
	case fe.Kind == forgeapi.KindRateLimited && fe.Status == 0 && fe.Code == "":
		return fmt.Errorf("%w: %v", forge.ErrReadDeferred, err)
	case fe.Kind == forgeapi.KindUnauthorized:
		return fmt.Errorf("%w: %v", forge.ErrTokenInvalid, err)
	case fe.Kind == forgeapi.KindRateLimited:
		return fmt.Errorf("%w: %v", forge.ErrRateLimited, err)
	case fe.Code == forgeapi.CodeOwnerUnresolved:
		return fmt.Errorf("%w: %v", forge.ErrOwnerUnresolved, err)
	case fe.Kind == forgeapi.KindForbidden:
		return fmt.Errorf("%w: %v", forge.ErrForbidden, err)
	default:
		return err
	}
}

// connectionError maps an open or identification failure: credential and
// budget failures keep their own sentinel, cancellation passes through, and
// the rest is forge.ErrConnection.
func connectionError(step string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	mapped := mapErr(err)
	if errors.Is(mapped, forge.ErrTokenInvalid) || errors.Is(mapped, forge.ErrRateLimited) || errors.Is(mapped, forge.ErrReadDeferred) {
		return fmt.Errorf("%s: %w", step, mapped)
	}
	return fmt.Errorf("%w: %s: %v", forge.ErrConnection, step, err)
}

// walked is how a list walk ended.
type walked struct {
	partial *forgeapi.Partial
	ceiling bool
}

// cut reports a walk that did not read the whole listing.
func (w walked) cut() bool { return w.ceiling || w.partial != nil }

// end is the walk's End.
func (w walked) end() forge.End {
	if w.cut() {
		return forge.EndCut
	}
	return forge.EndWhole
}

// walk follows Next across list calls, each admitted by c. A page cap marker
// with a cursor is the library's own per-call bound and the walk continues;
// any other partial marker ends the walk and is returned. Reaching maxPages
// with a cursor left sets ceiling.
func walk[T any](ctx context.Context, c *Client, maxPages int, fetch func(context.Context, forgeapi.Cursor) (forgeapi.Page[T], error)) ([]T, walked, error) {
	var rows []T
	var after forgeapi.Cursor
	for range maxPages {
		if err := c.admit(); err != nil {
			return nil, walked{}, err
		}
		page, err := fetch(ctx, after)
		if err != nil {
			return nil, walked{}, err
		}
		rows = append(rows, page.Items...)
		if p := page.Partial; p != nil && (p.Reason != forgeapi.PartialPaginationCap || page.Next == "") {
			return rows, walked{partial: p}, nil
		}
		if page.Next == "" {
			return rows, walked{}, nil
		}
		after = page.Next
	}
	return rows, walked{ceiling: true}, nil
}

// snapshot walks a listing that must be read whole.
func snapshot[T any](ctx context.Context, c *Client, maxPages int, fetch func(context.Context, forgeapi.Cursor) (forgeapi.Page[T], error)) ([]T, error) {
	for attempt := range 2 {
		rows, w, err := walk(ctx, c, maxPages, fetch)
		if err != nil {
			return nil, mapErr(err)
		}
		if err := c.stopOnRateLimit(w); err != nil {
			return nil, err
		}
		switch {
		case w.ceiling:
			return nil, fmt.Errorf("%w: the listing reached forge-scout's page ceiling", forge.ErrSnapshotPartial)
		case w.partial == nil:
			return rows, nil
		case w.partial.Reason == forgeapi.PartialGraphQLPartial && attempt == 0:
			continue
		default:
			return nil, fmt.Errorf("%w: %s", forge.ErrSnapshotPartial, w.partial.Reason)
		}
	}
	return nil, fmt.Errorf("%w: %s", forge.ErrSnapshotPartial, forgeapi.PartialGraphQLPartial)
}

func repoRef(repo forge.Repo) (forgeapi.RepoRef, error) {
	ref, ok := repo.Handle().(forgeapi.RepoRef)
	if !ok {
		return forgeapi.RepoRef{}, fmt.Errorf("repository %q carries no forge address", repo.Path)
	}
	return ref, nil
}

func repoOf(r *forgeapi.Repository) forge.Repo {
	return forge.Repo{
		Path: r.Ref.DisplayPath, DefaultBranch: r.Affordances.DefaultBranch, Archived: r.Archived, Fork: r.Fork, Private: r.Private,
	}.WithHandle(r.Ref)
}

// prsOf maps pull-request rows. On GitLab an item partial is a label list cut
// short, which may hide an excluded label, so the listing is partial.
func (c *Client) prsOf(rows []forgeapi.PullRequest) ([]forge.PullRequest, error) {
	out := make([]forge.PullRequest, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		if c.product == forge.ProductGitLab && r.Partial != nil && r.Partial.Reason == forgeapi.PartialPaginationCap {
			return nil, fmt.Errorf("%w: the labels of %s%s were cut short", forge.ErrSnapshotPartial, r.Repo.DisplayPath, refOf(r.Ref.Sigil, r.Ref.Number))
		}
		pr := forge.PullRequest{
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Repo: r.Repo.DisplayPath,
			Ref: refOf(r.Ref.Sigil, r.Ref.Number), Title: r.Title, Author: r.Author, URL: r.WebURL,
			HeadSHA: r.HeadSHA, Labels: labelNames(r.Labels), Number: r.Ref.Number, Draft: r.Draft,
		}
		pr.SetRepoHandle(r.Repo)
		out = append(out, pr)
	}
	return out, nil
}

func issuesOf(rows []forgeapi.Issue) []forge.Issue {
	out := make([]forge.Issue, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, forge.Issue{
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Repo: r.Repo.DisplayPath,
			Ref: refOf("#", r.Ref.Number), Title: r.Title, Author: r.Author, URL: r.WebURL,
			Labels: labelNames(r.Labels), Number: r.Ref.Number,
		})
	}
	return out
}

func refOf(sigil string, n int) string { return cmp.Or(sigil, "#") + strconv.Itoa(n) }

func labelNames(ls []forgeapi.Label) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

// runOf maps a completed run. A state other than passing, failing or
// neutral is one forgeapi could not map.
func runOf(r *forgeapi.Run, repo string) (forge.Run, bool) {
	var state forge.RunState
	switch r.State {
	case forgeapi.CheckPassing:
		state = forge.RunPassing
	case forgeapi.CheckFailing:
		state = forge.RunFailing
	case forgeapi.CheckNeutral:
		state = forge.RunNeutral
	default:
		return forge.Run{}, false
	}
	run := unstated(r, repo)
	run.State = state
	return run, true
}

// unstated is r with no State, as RunList.Unmapped carries it.
func unstated(r *forgeapi.Run, repo string) forge.Run {
	return forge.Run{
		CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, UpdatedAt: r.UpdatedAt, Repo: repo,
		Workflow: r.Name, Branch: r.Branch, Trigger: r.Trigger.String(), URL: r.WebURL,
		ID: r.Ref.ID, Number: r.Ref.Number,
	}
}

func checkStateOf(s forgeapi.CheckState) forge.CheckState {
	switch s {
	case forgeapi.CheckPassing:
		return forge.CheckPassing
	case forgeapi.CheckFailing:
		return forge.CheckFailing
	case forgeapi.CheckPending:
		return forge.CheckPending
	case forgeapi.CheckNeutral:
		return forge.CheckNeutral
	default:
		return forge.CheckUnknown
	}
}

func productOf(p forgeapi.Product) forge.Product {
	switch p {
	case forgeapi.ProductGitHub:
		return forge.ProductGitHub
	case forgeapi.ProductGitLab:
		return forge.ProductGitLab
	case forgeapi.ProductGitea:
		return forge.ProductGitea
	case forgeapi.ProductForgejo:
		return forge.ProductForgejo
	default:
		return forge.ProductUnknown
	}
}

// staticToken is a personal access token held for the life of the process.
type staticToken string

func (t staticToken) Token(context.Context) (string, error) { return string(t), nil }
func (staticToken) Kind() forgeapi.CredKind                 { return forgeapi.CredKindStaticPAT }
func (staticToken) State() forgeapi.CredState               { return forgeapi.CredValid }
