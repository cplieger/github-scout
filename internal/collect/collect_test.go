package collect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/config"
	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/github-scout/internal/ghquota"
	"github.com/cplieger/github-scout/internal/runstate"
	"github.com/cplieger/slogx/capture"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// listing is a fake repository's runs: completed, still pending and in a
// state the adapter could not map; Truncated cuts its listing short.
type listing struct {
	Runs      []forge.Run
	Pending   []forge.Run
	Unmapped  []forge.Run
	Truncated bool
}

// fakeConn answers every read from its fields; errs keys an error by
// "<op> <owner or repo>".
type fakeConn struct {
	errs        map[string]error
	repos       map[string][]forge.Repo
	prs         map[string][]forge.PullRequest
	issues      map[string][]forge.Issue
	runs        map[string]listing
	checks      map[string]forge.CheckResult
	login       string
	unresolved  []string
	budget      forge.Budget
	product     forge.Product
	truncated   bool
	calls       []string
	closed      bool
	budgetAfter map[string]forge.Budget
	// since is the bound each repository's last run listing asked for.
	since map[string]time.Time
	// ran is the workflow names each repository's run listings answered,
	// which a GitHub workflow listing defines unless the test says otherwise.
	ran map[string][]string
	// block, when set, runs before every read with the read's context and
	// its "<op> <key>"; a non-nil error is the read's answer.
	block func(ctx context.Context, call string) error
	// quota, when set, is the GitHub quota meter both clients share, which
	// every read is sent through.
	quota *fakeQuota
}

// fakeQuota sends each fake GitHub read through a real ghquota.Meter as the
// response it stands for: one request less of the shared budget, a 403 with
// no rate-limit header for a read answering forge.ErrForbidden.
type fakeQuota struct {
	meter *ghquota.Meter
	reset time.Time
	// remaining is the budget the next response reports, and least the
	// lowest any reported.
	remaining, least int
}

func newFakeQuota(clock func() time.Time, remaining int, reset time.Time) *fakeQuota {
	m := ghquota.New(clock)
	m.Arm()
	return &fakeQuota{meter: m, remaining: remaining, least: remaining, reset: reset}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// send reports one sent read's answer err to the meter and returns err.
func (q *fakeQuota) send(err error) error {
	if q == nil {
		return err
	}
	status := http.StatusOK
	if errors.Is(err, forge.ErrForbidden) {
		status = http.StatusForbidden
	}
	q.remaining--
	q.least = min(q.least, q.remaining)
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", strconv.Itoa(q.remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(q.reset.Unix(), 10))
	answer := roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: h, Body: http.NoBody}, nil
	})
	req, rerr := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.test/", http.NoBody)
	if rerr != nil {
		return rerr
	}
	if resp, rerr := q.meter.Observe(answer).RoundTrip(req); rerr == nil {
		resp.Body.Close()
	}
	return err
}

// admit is the meter's answer to a read about to be sent, nil without one.
func (q *fakeQuota) admit() error {
	if q == nil {
		return nil
	}
	return q.meter.Admit()
}

func (f *fakeConn) call(ctx context.Context, op, key string) error {
	if err := f.quota.admit(); err != nil {
		return err
	}
	f.calls = append(f.calls, op+" "+key)
	if f.block != nil {
		if err := f.block(ctx, op+" "+key); err != nil {
			return err
		}
	}
	if b, ok := f.budgetAfter[op+" "+key]; ok {
		f.budget = b
	}
	return f.quota.send(f.errs[op+" "+key])
}

func (f *fakeConn) Product() forge.Product { return f.product }
func (f *fakeConn) Login() string          { return f.login }
func (f *fakeConn) Budget() forge.Budget   { return f.budget }
func (f *fakeConn) Close()                 { f.closed = true }

func (f *fakeConn) Discover(ctx context.Context, owners []string) (forge.Discovery, error) {
	if err := f.call(ctx, "discover", strings.Join(owners, ",")); err != nil {
		return forge.Discovery{}, err
	}
	d := forge.Discovery{ByOwner: map[string][]forge.Repo{}, End: forge.EndWhole}
	if f.truncated {
		d.End = forge.EndCut
	}
	for _, o := range owners {
		if slices.Contains(f.unresolved, o) {
			d.Unresolved = append(d.Unresolved, o)
			continue
		}
		d.ByOwner[o] = f.repos[o]
	}
	return d, nil
}

func (f *fakeConn) OpenPRs(ctx context.Context, owner string) ([]forge.PullRequest, error) {
	if err := f.call(ctx, "prs", owner); err != nil {
		return nil, err
	}
	return f.prs[owner], nil
}

func (f *fakeConn) OpenIssues(ctx context.Context, owner string) ([]forge.Issue, error) {
	if err := f.call(ctx, "issues", owner); err != nil {
		return nil, err
	}
	return f.issues[owner], nil
}

func (f *fakeConn) RepoOpenPRs(ctx context.Context, repo forge.Repo) ([]forge.PullRequest, error) {
	if err := f.call(ctx, "repo-prs", repo.Path); err != nil {
		return nil, err
	}
	return f.prs[repo.Path], nil
}

func (f *fakeConn) RepoOpenIssues(ctx context.Context, repo forge.Repo) ([]forge.Issue, error) {
	if err := f.call(ctx, "repo-issues", repo.Path); err != nil {
		return nil, err
	}
	return f.issues[repo.Path], nil
}

// RunsSince answers the repository's runs created at or after since, as
// GitHub's and GitLab's server filter does, and records the bound asked. A
// truncated listing answers its rows as the newest-first prefix it is.
func (f *fakeConn) RunsSince(ctx context.Context, repo forge.Repo, since time.Time) (forge.RunList, error) {
	if err := f.call(ctx, "runs", repo.Path); err != nil {
		return forge.RunList{}, err
	}
	if f.since == nil {
		f.since = map[string]time.Time{}
	}
	f.since[repo.Path] = since
	all := f.runs[repo.Path]
	var list forge.RunList
	list.End = forge.EndWhole
	if all.Truncated {
		list.End = forge.EndCut
	}
	// A row with no creation time reaches the caller, which must refuse it.
	keep := func(rows []forge.Run) []forge.Run {
		var out []forge.Run
		for _, r := range rows {
			if r.CreatedAt.IsZero() || !r.CreatedAt.Before(since) {
				out = append(out, r)
				if !r.CreatedAt.IsZero() && (list.Oldest.IsZero() || r.CreatedAt.Before(list.Oldest)) {
					list.Oldest = r.CreatedAt
				}
			}
		}
		return out
	}
	list.Rows, list.Unmapped = keep(all.Runs), keep(all.Unmapped)
	for _, r := range keep(all.Pending) {
		if list.OldestPending.IsZero() || r.CreatedAt.Before(list.OldestPending) {
			list.OldestPending = r.CreatedAt
		}
	}
	if f.ran == nil {
		f.ran = map[string][]string{}
	}
	for _, r := range list.Rows {
		if !slices.Contains(f.ran[repo.Path], r.Workflow) {
			f.ran[repo.Path] = append(f.ran[repo.Path], r.Workflow)
		}
	}
	return list, nil
}

// CommitChecks answers the head's checks, whole unless the test set an End.
func (f *fakeConn) CommitChecks(ctx context.Context, pr *forge.PullRequest) (forge.CheckResult, error) {
	if err := f.call(ctx, "checks", pr.HeadSHA); err != nil {
		return forge.CheckResult{}, err
	}
	res := f.checks[pr.HeadSHA]
	if res.End == 0 {
		res.End = forge.EndWhole
	}
	return res, nil
}

// fakeGitHub answers the GitHub-only reads, each sent through the quota it
// shares with conn. A repository workflows does not key defines an active
// workflow for each name conn's run listings of it ever answered.
type fakeGitHub struct {
	conn      *fakeConn
	errs      map[string]error
	alerts    map[string][]forge.Alert
	workflows map[string][]forge.Workflow
	truncated map[string]bool
	// none is the repositories with no code-scanning analyses.
	none map[string]bool
	// block, when set, runs before every read with its context and its
	// "<op> <repo>"; a non-nil error is the read's answer.
	block         func(ctx context.Context, call string) error
	alertCalls    []string
	workflowCalls []string
	calls         int
}

func (g *fakeGitHub) BeginScan()           { g.conn.quota.meter.BeginScan() }
func (g *fakeGitHub) Budget() forge.Budget { return g.conn.quota.meter.Budget() }

// read sends one GitHub-only read answering the error errs keys by call,
// reporting whether it was sent.
func (g *fakeGitHub) read(ctx context.Context, call string) (sent bool, err error) {
	if g.block != nil {
		if err := g.block(ctx, call); err != nil {
			return false, err
		}
	}
	if err := g.conn.quota.admit(); err != nil {
		return false, err
	}
	g.calls++
	return true, g.conn.quota.send(g.errs[call])
}

func ended(truncated bool) forge.End {
	if truncated {
		return forge.EndCut
	}
	return forge.EndWhole
}

func (g *fakeGitHub) CodeScanningAlerts(ctx context.Context, repo forge.Repo) (forge.Listing[forge.Alert], error) {
	sent, err := g.read(ctx, "alerts "+repo.Path)
	if sent {
		g.alertCalls = append(g.alertCalls, repo.Path)
	}
	if err != nil {
		return forge.Listing[forge.Alert]{}, err
	}
	if g.none[repo.Path] {
		return forge.Listing[forge.Alert]{End: forge.EndNone}, nil
	}
	return forge.Listing[forge.Alert]{Rows: g.alerts[repo.Path], End: ended(g.truncated["alerts "+repo.Path])}, nil
}

func (g *fakeGitHub) Workflows(ctx context.Context, repo forge.Repo) (forge.Listing[forge.Workflow], error) {
	sent, err := g.read(ctx, "workflows "+repo.Path)
	if sent {
		g.workflowCalls = append(g.workflowCalls, repo.Path)
	}
	if err != nil {
		return forge.Listing[forge.Workflow]{}, err
	}
	defs, ok := g.workflows[repo.Path]
	if !ok && g.conn != nil {
		for _, name := range g.conn.ran[repo.Path] {
			defs = append(defs, forge.Workflow{Repo: repo.Path, Name: name, State: "active"})
		}
	}
	return forge.Listing[forge.Workflow]{Rows: defs, End: ended(g.truncated["workflows "+repo.Path])}, nil
}

// endpoint is one configured connection and the fakes behind it; rest, when
// set, replaces gh as the GitHub-only reader.
type endpoint struct {
	conn    *fakeConn
	gh      *fakeGitHub
	rest    GitHubReader
	openErr error
	// beforeOpen, when set, runs at the start of each open.
	beforeOpen func()
	cfg        config.Connection
}

func connCfg(name string, owners ...string) config.Connection {
	return config.Connection{
		Name: name, URL: "https://" + name + ".forge.test", Owners: owners, ExcludeRepos: map[string]bool{}, ExcludeAuthors: map[string]bool{},
		ExcludeLabels: map[string]bool{}, BotAuthors: map[string]bool{},
		Security: config.Security{Enabled: true, SkipForks: true, SkipRepos: map[string]bool{}},
	}
}

func githubEndpoint(name string) *endpoint {
	conn := &fakeConn{
		product: forge.ProductGitHub, login: "me", budget: forge.Budget{Remaining: 4000, Reset: now.Add(time.Hour)},
		repos: map[string][]forge.Repo{"o": {{Path: "o/r", DefaultBranch: "main"}}},
		quota: newFakeQuota(func() time.Time { return now }, 4000, now.Add(time.Hour)),
	}
	return &endpoint{
		cfg:  connCfg(name, "o"),
		conn: conn,
		gh:   &fakeGitHub{conn: conn, alerts: map[string][]forge.Alert{}},
	}
}

func giteaEndpoint(name string) *endpoint {
	return &endpoint{
		cfg: connCfg(name, "o"),
		conn: &fakeConn{
			product: forge.ProductGitea, login: "me", budget: forge.Budget{Remaining: -1},
			repos: map[string][]forge.Repo{"o": {{Path: "o/r", DefaultBranch: "main"}}},
		},
	}
}

type harness struct {
	c     *Collector
	store *runstate.Store
	rec   *capture.Recorder
	clock *time.Time
	dir   string
	eps   []*endpoint
	limit time.Duration
	// interval is the configured scan_interval, 15m unless a test sets it
	// before rebuilding.
	interval time.Duration
	// lookback is the configured lookback, 72h unless a test sets it
	// before rebuilding.
	lookback time.Duration
}

func newHarness(t *testing.T, eps ...*endpoint) *harness {
	t.Helper()
	return newLimitedHarness(t, 0, eps...)
}

// newLimitedHarness bounds each connection's scan at limit.
func newLimitedHarness(t *testing.T, limit time.Duration, eps ...*endpoint) *harness {
	t.Helper()
	h := &harness{dir: t.TempDir(), eps: eps, limit: limit, interval: 15 * time.Minute, lookback: 72 * time.Hour}
	clock := now
	h.clock = &clock
	h.build(t)
	return h
}

// openStore opens the run state store in dir, closed when the test ends.
func openStore(t *testing.T, dir string) *runstate.Store {
	t.Helper()
	s, err := runstate.Open(dir)
	if err != nil {
		t.Fatalf("Setup: open run state: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func (h *harness) build(t *testing.T) {
	t.Helper()
	if h.store != nil {
		h.store.Close()
	}
	h.store = openStore(t, h.dir)
	logger, rec := capture.New()
	h.rec = rec
	byName := map[string]*endpoint{}
	var cfgs []config.Connection
	for _, ep := range h.eps {
		byName[ep.cfg.Name] = ep
		cfgs = append(cfgs, ep.cfg)
		// A process start opens a fresh meter on the forge's budget.
		if q := ep.conn.quota; q != nil {
			q.meter = ghquota.New(func() time.Time { return *h.clock })
			q.meter.Arm()
		}
	}
	h.c = New(t.Context(), &Deps{
		Open: func(_ context.Context, c *config.Connection) (Conn, GitHubReader, error) {
			ep := byName[c.Name]
			if ep.beforeOpen != nil {
				ep.beforeOpen()
			}
			if ep.openErr != nil {
				return nil, nil, ep.openErr
			}
			// The account read an open makes is the meter's first answer.
			if err := ep.conn.quota.send(nil); err != nil {
				return nil, nil, err
			}
			if ep.rest != nil {
				return ep.conn, ep.rest, nil
			}
			if ep.gh == nil {
				return ep.conn, nil, nil
			}
			return ep.conn, ep.gh, nil
		},
		Store: h.store, Logger: logger, Now: func() time.Time { return *h.clock },
		Connections: cfgs, Lookback: h.lookback, ScanInterval: h.interval, ScanLimit: h.limit,
	})
}

// restart builds a fresh collector over the same state directory, as a
// container start does.
func (h *harness) restart(t *testing.T) { t.Helper(); h.build(t) }

func (h *harness) scan(t *testing.T) Outcome {
	t.Helper()
	h.rec = h.resetRecorder(t)
	return h.c.Scan(t.Context())
}

func (h *harness) resetRecorder(t *testing.T) *capture.Recorder {
	t.Helper()
	logger, rec := capture.New()
	h.c.logger = logger
	return rec
}

// lines returns the rendered attributes of every record logged as msg.
func lines(rec *capture.Recorder, msg string) []map[string]string {
	var out []map[string]string
	for _, r := range rec.Records() {
		if r.Message != msg {
			continue
		}
		m := map[string]string{"level": r.Level.String()}
		r.Attrs(func(a slog.Attr) bool {
			m[a.Key] = a.Value.Resolve().String()
			return true
		})
		out = append(out, m)
	}
	return out
}

func lineFor(t *testing.T, rec *capture.Recorder, msg, connection string) map[string]string {
	t.Helper()
	for _, l := range lines(rec, msg) {
		if l["connection"] == connection {
			return l
		}
	}
	t.Fatalf("no %q line for connection %q; messages %v", msg, connection, rec.Messages())
	return nil
}

func noLineFor(t *testing.T, rec *capture.Recorder, msg, connection string) {
	t.Helper()
	for _, l := range lines(rec, msg) {
		if l["connection"] == connection {
			t.Errorf("a %q line was logged for connection %q: %v", msg, connection, l)
		}
	}
}

func run(id int64, state forge.RunState, created time.Time) forge.Run {
	return forge.Run{
		ID: id, Number: id, Repo: "o/r", Workflow: "CI", Branch: "main", Trigger: "push", State: state,
		URL: "https://forge.test/o/r/runs/" + strconv.FormatInt(id, 10), CreatedAt: created,
		StartedAt: created.Add(10 * time.Second), UpdatedAt: created.Add(5 * time.Minute),
	}
}

func TestScan_ci_run_is_emitted_once_per_state_across_scans_and_restarts(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	h := newHarness(t, ep)
	h.scan(t)
	got := lines(h.rec, "ci run")
	if len(got) != 1 {
		t.Fatalf("first scan logged %d ci run line(s), want 1", len(got))
	}
	l := got[0]
	want := map[string]string{
		"forge": "github", "connection": "gh", "repo": "o/r", "state": "failing", "workflow": "CI", "branch": "main",
		"default_branch": "true", "trigger": "push", "run_id": "1", "run_number": "1", "url": "https://forge.test/o/r/runs/1",
		"created_at": "2026-10-07T11:00:00Z", "started_at": "2026-10-07T11:00:10Z", "updated_at": "2026-10-07T11:05:00Z",
		"duration_seconds": "290",
	}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("ci run %s = %q, want %q", k, l[k], v)
		}
	}
	if _, ok := l["previous_state"]; ok {
		t.Errorf("first observation carries previous_state %q, want none", l["previous_state"])
	}
	h.scan(t)
	if n := h.rec.CountExact("ci run"); n != 0 {
		t.Errorf("second scan re-emitted %d ci run line(s), want 0", n)
	}
	h.restart(t)
	h.scan(t)
	if n := h.rec.CountExact("ci run"); n != 0 {
		t.Errorf("a scan after a restart re-emitted %d ci run line(s), want 0: the state must persist", n)
	}
	rerun := run(1, forge.RunPassing, now.Add(-time.Hour))
	rerun.UpdatedAt = now
	ep.conn.runs["o/r"] = listing{Runs: []forge.Run{rerun}}
	h.scan(t)
	l = lineFor(t, h.rec, "ci run", "gh")
	if l["state"] != "passing" || l["previous_state"] != "failing" {
		t.Errorf("re-run line = state %q previous %q, want passing after failing", l["state"], l["previous_state"])
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	if c["changed_runs"] != "1" || c["new_runs"] != "0" {
		t.Errorf("scan complete runs = new %s changed %s, want 0 and 1", c["new_runs"], c["changed_runs"])
	}
}

func TestScan_unknown_duration_omits_start_and_duration(t *testing.T) {
	ep := githubEndpoint("gh")
	r := run(1, forge.RunPassing, now.Add(-time.Hour))
	r.StartedAt = time.Time{}
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{r}}}
	h := newHarness(t, ep)
	h.scan(t)
	l := lineFor(t, h.rec, "ci run", "gh")
	if _, ok := l["started_at"]; ok {
		t.Error("ci run carries started_at for a run with no start time")
	}
	if _, ok := l["duration_seconds"]; ok {
		t.Error("ci run carries duration_seconds for a run with no start time")
	}
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["run_durations_known"] != "0" {
		t.Errorf("run_durations_known = %s, want 0", c["run_durations_known"])
	}
	if n := h.rec.CountExact("slow workflow"); n != 0 {
		t.Errorf("slow workflow lines = %d, want 0 with no timed runs", n)
	}
}

func TestScan_failing_workflow_tracks_the_default_branch_streak(t *testing.T) {
	ep := githubEndpoint("gh")
	feature := run(3, forge.RunFailing, now.Add(-time.Hour))
	feature.Branch = "feature"
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{
		run(1, forge.RunFailing, now.Add(-3*time.Hour)), run(2, forge.RunFailing, now.Add(-2*time.Hour)), feature,
	}}}
	h := newHarness(t, ep)
	h.scan(t)
	l := lineFor(t, h.rec, "failing workflow", "gh")
	if l["consecutive_failures"] != "2" || l["failing_since"] != "2026-10-07T09:00:00Z" || l["failing_since_clipped"] != "true" ||
		l["run_id"] != "2" || l["last_run_at"] != "2026-10-07T10:00:00Z" || l["rank"] != "1" || l["workflow"] != "CI" {
		t.Errorf("failing workflow = %v, want run 2 created 10:00, streak 2 since 09:00, clipped on a cold store, rank 1", l)
	}
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["failing_workflows"] != "1" || c["failing_workflows_read"] != "complete" {
		t.Errorf("scan complete failing = %s read %s, want 1 complete: a cold store clips the streak and reads the window whole", c["failing_workflows"], c["failing_workflows_read"])
	}
	fixed := run(2, forge.RunPassing, now.Add(-2*time.Hour))
	fixed.UpdatedAt = now
	ep.conn.runs["o/r"] = listing{Runs: []forge.Run{fixed}}
	h.scan(t)
	if n := h.rec.CountExact("failing workflow"); n != 0 {
		t.Errorf("after the newest run passed on a re-run, %d failing workflow line(s), want 0", n)
	}
}

func TestScan_a_default_branch_change_reclassifies_the_runs_already_read(t *testing.T) {
	ep := githubEndpoint("gh")
	runs := []forge.Run{run(1, forge.RunFailing, now.Add(-5*time.Hour))}
	for i := range 3 {
		r := run(int64(i+2), forge.RunFailing, now.Add(-time.Duration(4-i)*time.Hour))
		r.Branch = "trunk"
		runs = append(runs, r)
	}
	ep.conn.runs = map[string]listing{"o/r": {Runs: runs}}
	h := newHarness(t, ep)
	h.scan(t)
	if l := lineFor(t, h.rec, "failing workflow", "gh"); l["branch"] != "main" || l["run_id"] != "1" {
		t.Fatalf("failing workflow on main = %v, want run 1", l)
	}
	noLineFor(t, h.rec, "slow workflow", "gh")
	ep.conn.repos["o"][0].DefaultBranch = "trunk"
	h.scan(t)
	if l := lineFor(t, h.rec, "failing workflow", "gh"); l["branch"] != "trunk" || l["run_id"] != "4" || l["consecutive_failures"] != "3" {
		t.Errorf("failing workflow after the default moved to trunk = %v, want run 4 on trunk, 3 consecutive failures", l)
	}
	if l := lineFor(t, h.rec, "slow workflow", "gh"); l["runs"] != "3" {
		t.Errorf("slow workflow after the default moved to trunk = %v, want the 3 trunk runs and not run 1 on main", l)
	}
	if n := h.rec.CountExact("ci run"); n != 0 {
		t.Errorf("ci run lines on the reclassifying scan = %d, want 0: no run changed", n)
	}
}

func TestScan_a_run_the_store_cannot_hold_fails_its_repositorys_listing(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.repos["o"] = append(ep.conn.repos["o"], forge.Repo{Path: "o/s", DefaultBranch: "main"})
	bad := run(2, forge.RunFailing, now.Add(-time.Hour))
	bad.UpdatedAt = time.Time{}
	other := run(3, forge.RunPassing, now.Add(-time.Hour))
	other.Repo = "o/s"
	ep.conn.runs = map[string]listing{
		"o/r": {Runs: []forge.Run{run(1, forge.RunPassing, now.Add(-2*time.Hour)), bad}},
		"o/s": {Runs: []forge.Run{other}},
	}
	h := newHarness(t, ep)
	h.scan(t)
	if l := lineFor(t, h.rec, "runs listing failed", "gh"); l["repo"] != "o/r" || l["error"] == "" {
		t.Errorf("runs listing failed = %v, want o/r and the reason", l)
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	want := map[string]string{
		"new_runs": "1", "tracked": "1", "runs_failed_repos": "1", "failing_workflows": "0",
		"failing_workflows_read": "partial", "degraded": "true", "failed_signals": "runs",
	}
	for k, v := range want {
		if c[k] != v {
			t.Errorf("scan complete %s = %q, want %q: a listing holding a row the store cannot hold is a failed read", k, c[k], v)
		}
	}
	for _, l := range lines(h.rec, "ci run") {
		if l["repo"] != "o/s" {
			t.Errorf("ci run %v from the failed listing, want only o/s's", l)
		}
	}
}

func TestScan_workflow_outside_the_lookback_stays_failing_from_the_store(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	h := newHarness(t, ep)
	h.scan(t)
	*h.clock = now.Add(7 * 24 * time.Hour)
	ep.conn.runs["o/r"] = listing{}
	h.restart(t)
	h.scan(t)
	if l := lineFor(t, h.rec, "failing workflow", "gh"); l["run_id"] != "1" {
		t.Errorf("a weekly workflow a week later = %v, want still failing at run 1", l)
	}
}

func TestScan_a_discovery_whose_owners_list_no_repository_is_complete_and_forgets_the_stored_ones(t *testing.T) {
	ep := githubEndpoint("gh")
	listed := ep.conn.repos["o"]
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	h := newHarness(t, ep)
	h.scan(t)
	*h.clock = now.Add(time.Hour)
	ep.conn.repos["o"] = nil
	ep.conn.runs["o/r"] = listing{}
	h.scan(t)
	noLineFor(t, h.rec, "scan degraded", "gh")
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["repos_read"] != "complete" || c["owners_empty"] != "o" || c["degraded"] != "false" {
		t.Errorf("scan with owner o listing no repository: repos_read %q owners_empty %q degraded %q, want complete, o, false", c["repos_read"], c["owners_empty"], c["degraded"])
	}
	*h.clock = now.Add(7 * 24 * time.Hour)
	ep.conn.repos["o"] = listed
	h.scan(t)
	noLineFor(t, h.rec, "failing workflow", "gh")
}

func TestScan_a_scan_whose_owners_do_not_resolve_forgets_no_stored_workflow(t *testing.T) {
	ep := githubEndpoint("gh")
	listed := ep.conn.repos["o"]
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	h := newHarness(t, ep)
	h.scan(t)
	*h.clock = now.Add(7 * 24 * time.Hour)
	ep.conn.unresolved = []string{"o"}
	ep.conn.runs["o/r"] = listing{}
	before := len(ep.conn.calls)
	h.scan(t)
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "no_repos_visible" {
		t.Fatalf("Setup: scan with no owner resolved degraded with cause %q, want no_repos_visible", l["cause"])
	}
	if calls := ep.conn.calls[before:]; !slices.Equal(calls, []string{"discover o"}) {
		t.Errorf("calls %v, want discovery alone: after a discovery that resolved no owner, every row would be dropped", calls)
	}
	*h.clock = h.clock.Add(time.Hour)
	ep.conn.unresolved = nil
	ep.conn.repos["o"] = listed
	h.scan(t)
	if l := lineFor(t, h.rec, "failing workflow", "gh"); l["run_id"] != "1" {
		t.Errorf("the weekly workflow after one scan that resolved no owner = %v, want still failing at run 1: a blind listing forgets nothing", l)
	}
}

func TestScan_failing_workflows_are_capped_with_the_full_count(t *testing.T) {
	ep := githubEndpoint("gh")
	var runs []forge.Run
	for i := range 30 {
		r := run(int64(i+1), forge.RunFailing, now.Add(-time.Duration(i+1)*time.Minute))
		r.Workflow = fmt.Sprintf("wf-%02d", i)
		runs = append(runs, r)
	}
	ep.conn.runs = map[string]listing{"o/r": {Runs: runs}}
	h := newHarness(t, ep)
	h.scan(t)
	if n := h.rec.CountExact("failing workflow"); n != 25 {
		t.Errorf("failing workflow lines = %d, want 25", n)
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	if c["failing_workflows"] != "30" || c["failing_workflows_listed"] != "25" {
		t.Errorf("scan complete failing = %s listed %s, want 30 and 25", c["failing_workflows"], c["failing_workflows_listed"])
	}
}

func TestScan_disabled_workflows_are_capped_with_the_full_count(t *testing.T) {
	ep := githubEndpoint("gh")
	var wfs []forge.Workflow
	for i := range 30 {
		wfs = append(wfs, forge.Workflow{Repo: "o/r", Name: fmt.Sprintf("wf-%02d", i), State: "disabled_manually"})
	}
	ep.gh.workflows = map[string][]forge.Workflow{"o/r": wfs}
	h := newHarness(t, ep)
	h.scan(t)
	if n := h.rec.CountExact("disabled workflow"); n != 25 {
		t.Errorf("disabled workflow lines = %d, want 25", n)
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	if c["disabled_workflows"] != "30" || c["disabled_workflows_listed"] != "25" {
		t.Errorf("scan complete disabled = %s listed %s, want 30 and 25", c["disabled_workflows"], c["disabled_workflows_listed"])
	}
}

func TestScan_slow_workflows_need_three_timed_runs(t *testing.T) {
	ep := githubEndpoint("gh")
	var runs []forge.Run
	for i, d := range []time.Duration{time.Minute, 2 * time.Minute, 10 * time.Minute} {
		r := run(int64(i+1), forge.RunPassing, now.Add(-time.Duration(i+1)*time.Hour))
		r.UpdatedAt = r.StartedAt.Add(d)
		runs = append(runs, r)
	}
	two := run(9, forge.RunPassing, now.Add(-time.Hour))
	two.Workflow = "rare"
	runs = append(runs, two)
	ep.conn.runs = map[string]listing{"o/r": {Runs: runs}}
	h := newHarness(t, ep)
	h.scan(t)
	got := lines(h.rec, "slow workflow")
	if len(got) != 1 {
		t.Fatalf("slow workflow lines = %d, want 1 (rare has one run)", len(got))
	}
	if got[0]["workflow"] != "CI" || got[0]["runs"] != "3" || got[0]["p50_seconds"] != "120" || got[0]["p95_seconds"] != "600" {
		t.Errorf("slow workflow = %v, want CI over 3 runs, p50 120 s, p95 600 s", got[0])
	}
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["run_durations_known"] != "4" || c["slow_workflows_listed"] != "1" {
		t.Errorf("scan complete durations = %s listed %s, want 4 and 1", c["run_durations_known"], c["slow_workflows_listed"])
	}
}

func TestScan_open_work_carries_origin_age_and_checks(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.cfg.BotAuthors = map[string]bool{"helper": true}
	pr := func(n int, author, head string, updated time.Time) forge.PullRequest {
		return forge.PullRequest{
			Repo: "o/r", Number: n, Ref: "#" + strconv.Itoa(n), Title: "t", Author: author, HeadSHA: head,
			CreatedAt: now.Add(-40 * 24 * time.Hour), UpdatedAt: updated, Labels: []string{"a", "b"},
		}
	}
	ep.conn.prs = map[string][]forge.PullRequest{"o": {
		pr(1, "Me", "h1", now.Add(-time.Hour)), pr(2, "dependabot[bot]", "h2", now.Add(-31*24*time.Hour)),
		pr(3, "Helper", "", now.Add(-2*time.Hour)), pr(4, "stranger", "h4", now.Add(-3*time.Hour)),
	}}
	ep.conn.checks = map[string]forge.CheckResult{"h1": {State: "failing"}, "h2": {State: "passing", End: forge.EndCut}}
	ep.conn.errs = map[string]error{"checks h4": errors.New("boom")}
	h := newHarness(t, ep)
	h.scan(t)
	got := map[string]map[string]string{}
	for _, l := range lines(h.rec, "open pull request") {
		got[l["number"]] = l
	}
	if got["1"]["from"] != "you" || got["2"]["from"] != "bot" || got["3"]["from"] != "bot" || got["4"]["from"] != "someone_else" {
		t.Errorf("from = %s %s %s %s, want you bot bot someone_else", got["1"]["from"], got["2"]["from"], got["3"]["from"], got["4"]["from"])
	}
	if got["1"]["checks"] != "failing" || got["2"]["checks"] != "unknown" || got["4"]["checks"] != "unknown" || got["3"]["checks"] != "unknown" {
		t.Errorf("checks = %q %q %q %q, want failing, unknown on a partial read, unknown on a failed read, unknown on a row naming no head",
			got["1"]["checks"], got["2"]["checks"], got["4"]["checks"], got["3"]["checks"])
	}
	if got["2"]["rank"] != "1" || got["2"]["idle_days"] != "31" || got["2"]["age_days"] != "40" || got["1"]["labels"] != "a,b" {
		t.Errorf("row 2 = %v, want rank 1 (idlest), idle 31, age 40; row 1 labels %q", got["2"], got["1"]["labels"])
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	want := map[string]string{
		"open_prs": "4", "from_others_prs": "1", "failing_checks_prs": "1", "checks_unread": "3",
		"open_age_30_90d": "4", "idle_30d": "1",
	}
	for k, v := range want {
		if c[k] != v {
			t.Errorf("scan complete %s = %q, want %q", k, c[k], v)
		}
	}
	if !strings.Contains(c["failed_signals"], "pr_checks") || h.rec.CountExact("scan degraded") != 0 {
		t.Errorf("failed_signals %q degraded lines %d, want pr_checks named and no escalation", c["failed_signals"], h.rec.CountExact("scan degraded"))
	}
}

func TestScan_age_buckets(t *testing.T) {
	ep := githubEndpoint("gh")
	issue := func(n int, age time.Duration) forge.Issue {
		return forge.Issue{Repo: "o/r", Number: n, Author: "x", CreatedAt: now.Add(-age), UpdatedAt: now}
	}
	ep.conn.issues = map[string][]forge.Issue{"o": {
		issue(1, 2*24*time.Hour), issue(2, 10*24*time.Hour), issue(3, 45*24*time.Hour), issue(4, 200*24*time.Hour),
		issue(5, 30*24*time.Hour),
	}}
	idle := issue(6, 3*24*time.Hour)
	idle.UpdatedAt = now.Add(-30 * 24 * time.Hour)
	ep.conn.issues["o"] = append(ep.conn.issues["o"], idle)
	h := newHarness(t, ep)
	h.scan(t)
	c := lineFor(t, h.rec, "scan complete", "gh")
	for k, v := range map[string]string{"open_age_lt7d": "2", "open_age_7_30d": "1", "open_age_30_90d": "2", "open_age_90d_plus": "1", "idle_30d": "1", "open_issues": "6"} {
		if c[k] != v {
			t.Errorf("scan complete %s = %q, want %q", k, c[k], v)
		}
	}
}

func TestScan_pr_checks_unsupported_where_rows_carry_no_head(t *testing.T) {
	for name, prs := range map[string][]forge.PullRequest{
		"one_open_pr": {{Repo: "o/r", Number: 1, Author: "x", UpdatedAt: now}},
		"no_open_pr":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			ep := giteaEndpoint("gt")
			ep.conn.prs = map[string][]forge.PullRequest{"o": prs}
			h := newHarness(t, ep)
			h.scan(t)
			c := lineFor(t, h.rec, "scan complete", "gt")
			if !strings.Contains(c["unsupported_signals"], "pr_checks") {
				t.Errorf("unsupported_signals = %q, want pr_checks on a product whose rows name no head", c["unsupported_signals"])
			}
			for _, k := range []string{"failing_checks_prs", "checks_unread"} {
				if _, ok := c[k]; ok {
					t.Errorf("scan complete carries %s = %s where checks are unsupported, want it absent", k, c[k])
				}
			}
		})
	}
}

func TestScan_pr_checks_read_through_a_partial_pr_population_publish_no_count(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.prs = map[string][]forge.PullRequest{"o": {
		{Repo: "o/r", Number: 1, Ref: "#1", Author: "x", HeadSHA: "h1", CreatedAt: now, UpdatedAt: now},
		{Repo: "o/unlisted", Number: 2, Ref: "#2", Author: "x", HeadSHA: "h2", CreatedAt: now, UpdatedAt: now},
	}}
	ep.conn.checks = map[string]forge.CheckResult{"h1": {State: "passing"}, "h2": {State: "failing"}}
	h := newHarness(t, ep)
	h.scan(t)
	c := lineFor(t, h.rec, "scan complete", "gh")
	if c["open_prs_read"] != "partial" || c["pr_checks_read"] != "partial" {
		t.Errorf("scan with o/unlisted's pull request dropped: open_prs_read %q pr_checks_read %q, want partial and partial", c["open_prs_read"], c["pr_checks_read"])
	}
	if v, ok := c["failing_checks_prs"]; ok {
		t.Errorf("scan with o/unlisted's pull request dropped: failing_checks_prs = %s, want no count: the dropped one may be failing", v)
	}
	if l := lineFor(t, h.rec, "open pull request", "gh"); l["number"] != "1" || l["checks"] != "passing" {
		t.Errorf("open pull request = %v, want #1 with its own checks passing", l)
	}
}

func TestScan_pr_checks_supported_with_no_open_pr_count_zero(t *testing.T) {
	ep := githubEndpoint("gh")
	h := newHarness(t, ep)
	h.scan(t)
	c := lineFor(t, h.rec, "scan complete", "gh")
	if c["failing_checks_prs"] != "0" || strings.Contains(c["unsupported_signals"], "pr_checks") {
		t.Errorf("scan complete failing_checks_prs %q unsupported %q, want a read 0 and pr_checks supported", c["failing_checks_prs"], c["unsupported_signals"])
	}
}

func TestScan_a_head_with_no_checks_is_a_whole_read(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 1, Author: "x", HeadSHA: "h1"}}}
	ep.conn.checks = map[string]forge.CheckResult{"h1": {State: forge.CheckNone}}
	h := newHarness(t, ep)
	h.scan(t)
	if l := lineFor(t, h.rec, "open pull request", "gh"); l["checks"] != "none" {
		t.Errorf("open pull request checks = %q, want none", l["checks"])
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	if c["pr_checks_read"] != "complete" || c["checks_unread"] != "0" || c["failing_checks_prs"] != "0" {
		t.Errorf("scan complete pr_checks_read %q checks_unread %q failing_checks_prs %q, want complete, 0, 0",
			c["pr_checks_read"], c["checks_unread"], c["failing_checks_prs"])
	}
}

func TestScan_pr_checks_unread_on_every_headless_pr_publish_no_count(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.prs = map[string][]forge.PullRequest{"o": {
		{Repo: "o/r", Number: 1, Author: "x"}, {Repo: "o/r", Number: 2, Author: "x"},
	}}
	h := newHarness(t, ep)
	h.scan(t)
	c := lineFor(t, h.rec, "scan complete", "gh")
	if v, ok := c["failing_checks_prs"]; ok || c["checks_unread"] != "2" {
		t.Errorf("scan complete failing_checks_prs %q (present %t) checks_unread %q, want it absent beside 2 unread", v, ok, c["checks_unread"])
	}
}

func TestScan_noise_exclusion_and_archived_intersection(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.repos = map[string][]forge.Repo{"o": {
		{Path: "o/r", DefaultBranch: "main"}, {Path: "o/old", Archived: true}, {Path: "o/noisy", DefaultBranch: "main"},
	}}
	ep.cfg.ExcludeRepos = map[string]bool{"o/noisy": true}
	ep.cfg.ExcludeAuthors = map[string]bool{"renovate[bot]": true}
	ep.cfg.ExcludeLabels = map[string]bool{"skip": true}
	ep.conn.prs = map[string][]forge.PullRequest{"o": {
		{Repo: "O/R", Number: 1, Author: "x"},
		{Repo: "o/old", Number: 2, Author: "x"},
		{Repo: "o/noisy", Number: 3, Author: "x"},
		{Repo: "o/r", Number: 4, Author: "Renovate[bot]"},
		{Repo: "o/r", Number: 5, Author: "x", Labels: []string{"Skip"}},
		{Repo: "other/r", Number: 6, Author: "x"},
	}}
	h := newHarness(t, ep)
	h.scan(t)
	got := lines(h.rec, "open pull request")
	if len(got) != 1 || got[0]["number"] != "1" {
		t.Errorf("open pull request lines = %v, want only #1 (archived, excluded repo, excluded author and label, foreign repo dropped)", got)
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	if c["excluded_prs"] != "2" || c["skipped"] != "1" || c["repos_discovered"] != "1" {
		t.Errorf("scan complete = excluded %s skipped %s repos %s, want 2 1 1", c["excluded_prs"], c["skipped"], c["repos_discovered"])
	}
	for _, call := range ep.conn.calls {
		if strings.HasSuffix(call, "o/noisy") || strings.HasSuffix(call, "o/old") {
			t.Errorf("an excluded or archived repo was read: %s", call)
		}
	}
}

func TestScan_owner_unresolved_listing_falls_back_per_repository(t *testing.T) {
	ep := githubEndpoint("gl")
	ep.conn.product = forge.ProductGitLab
	ep.gh = nil
	ep.conn.errs = map[string]error{"prs o": forge.ErrOwnerUnresolved, "issues o": forge.ErrOwnerUnresolved}
	ep.conn.prs = map[string][]forge.PullRequest{"o/r": {{Repo: "o/r", Number: 1, Ref: "!1", Author: "x"}}}
	ep.conn.issues = map[string][]forge.Issue{"o/r": {{Repo: "o/r", Number: 2, Author: "x"}}}
	h := newHarness(t, ep)
	h.scan(t)
	if l := lineFor(t, h.rec, "open pull request", "gl"); l["ref"] != "!1" {
		t.Errorf("fallback pull request = %v, want !1", l)
	}
	if n := h.rec.CountExact("open issue"); n != 1 {
		t.Errorf("fallback issue lines = %d, want 1", n)
	}
	if h.rec.CountExact("scan degraded") != 0 {
		t.Errorf("a resolved fallback escalated: %v", lines(h.rec, "scan degraded"))
	}
}

func TestScan_owner_unresolved_at_discovery_escalates(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.cfg.Owners = []string{"o", "ghost"}
	ep.conn.unresolved = []string{"ghost"}
	h := newHarness(t, ep)
	h.scan(t)
	l := lineFor(t, h.rec, "scan degraded", "gh")
	if l["cause"] != "owner_unresolved" || !strings.Contains(l["reason"], "ghost") {
		t.Errorf("scan degraded = %v, want owner_unresolved naming ghost", l)
	}
	ep.cfg.Owners = []string{"ghost"}
	h2 := newHarness(t, ep)
	h2.scan(t)
	if l := lineFor(t, h2.rec, "scan degraded", "gh"); l["cause"] != "no_repos_visible" {
		t.Errorf("every owner unresolved: cause = %q, want no_repos_visible", l["cause"])
	}
}

func TestScan_an_owner_answering_no_usable_repository(t *testing.T) {
	tests := []struct {
		name  string
		repos map[string][]forge.Repo
		empty string
	}{
		{"one_owner_empty_beside_one_with_repositories", map[string][]forge.Repo{"o": {{Path: "o/r", DefaultBranch: "main"}}, "p": {}}, "p"},
		{"every_owner_empty", map[string][]forge.Repo{"o": {}, "p": {}}, "o,p"},
		{"every_repository_archived", map[string][]forge.Repo{"o": {{Path: "o/old", Archived: true}}, "p": {}}, "p"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := githubEndpoint("gh")
			ep.cfg.Owners = []string{"o", "p"}
			ep.conn.repos = tc.repos
			h := newHarness(t, ep)
			h.scan(t)
			c := lineFor(t, h.rec, "scan complete", "gh")
			if c["owners_empty"] != tc.empty {
				t.Errorf("scan complete owners_empty = %q, want %q", c["owners_empty"], tc.empty)
			}
			noLineFor(t, h.rec, "scan degraded", "gh")
			if c["degraded"] != "false" || c["errors"] != "0" || c["repos_read"] != "complete" {
				t.Errorf("scan complete degraded %q errors %q repos_read %q, want a clean, complete scan: every owner resolved and was listed whole", c["degraded"], c["errors"], c["repos_read"])
			}
		})
	}
}

// TestScan_an_owner_listing_no_repository_is_named_on_scan_complete keeps a
// complete read, as the owner resolved and holds nothing to read, and names
// the owner: loudly the first scan it is empty, quietly while it stays so.
func TestScan_an_owner_listing_no_repository_is_named_on_scan_complete(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.cfg.Owners = []string{"o", "p", "u"}
	ep.conn.unresolved = []string{"u"}
	ep.conn.repos = map[string][]forge.Repo{"o": {{Path: "o/r", DefaultBranch: "main"}}, "p": {}}
	h := newHarness(t, ep)
	scanEmpty := func(step, wantLevel string) {
		t.Helper()
		h.scan(t)
		if c := lineFor(t, h.rec, "scan complete", "gh"); c["owners_empty"] != "p" {
			t.Errorf("%s: scan complete owners_empty = %q, want %q: u is unresolved, not empty", step, c["owners_empty"], "p")
		}
		if l := lineFor(t, h.rec, "owner lists no repository", "gh"); l["level"] != wantLevel || l["owners"] != "p" {
			t.Errorf("%s: owner lists no repository = %v, want level %s owners p", step, l, wantLevel)
		}
	}
	scanEmpty("first scan", "WARN")
	scanEmpty("second scan", "DEBUG")
	ep.conn.repos["p"] = []forge.Repo{{Path: "p/r", DefaultBranch: "main"}}
	h.scan(t)
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["owners_empty"] != "" {
		t.Errorf("p answering a repository: owners_empty = %q, want empty", c["owners_empty"])
	}
	noLineFor(t, h.rec, "owner lists no repository", "gh")
	ep.conn.repos["p"] = nil
	scanEmpty("p empty again", "WARN")
}

func TestScan_open_work_with_no_reported_time_is_unknown_not_new(t *testing.T) {
	ep := githubEndpoint("gh")
	known := forge.PullRequest{Repo: "o/r", Number: 1, Author: "x", CreatedAt: now.Add(-40 * 24 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
	undated := forge.PullRequest{Repo: "o/r", Number: 2, Author: "x"}
	ep.conn.prs = map[string][]forge.PullRequest{"o": {undated, known}}
	ep.conn.issues = map[string][]forge.Issue{"o": {
		{Repo: "o/r", Number: 3, Author: "x", UpdatedAt: now.Add(-31 * 24 * time.Hour)},
		{Repo: "o/r", Number: 4, Author: "x", CreatedAt: now.Add(-2 * 24 * time.Hour)},
	}}
	h := newHarness(t, ep)
	h.scan(t)
	got := map[string]map[string]string{}
	for _, msg := range []string{"open pull request", "open issue"} {
		for _, l := range lines(h.rec, msg) {
			got[l["number"]] = l
		}
	}
	absent := map[string][]string{
		"2": {"created_at", "age_days", "updated_at", "idle_days"},
		"3": {"created_at", "age_days"},
		"4": {"updated_at", "idle_days"},
	}
	for number, keys := range absent {
		for _, k := range keys {
			if v, ok := got[number][k]; ok {
				t.Errorf("item #%s carries %s = %q for a time the forge did not report, want it absent", number, k, v)
			}
		}
	}
	if got["3"]["idle_days"] != "31" || got["4"]["age_days"] != "2" {
		t.Errorf("item #3 idle_days %q, #4 age_days %q, want the known times' 31 and 2", got["3"]["idle_days"], got["4"]["age_days"])
	}
	if got["1"]["rank"] != "1" || got["2"]["rank"] != "2" {
		t.Errorf("pull request ranks #1 %q #2 %q, want the known activity first and the unknown after it", got["1"]["rank"], got["2"]["rank"])
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	want := map[string]string{
		"open_age_lt7d": "1", "open_age_30_90d": "1", "open_age_unknown": "2", "idle_30d": "1", "idle_unknown": "2",
	}
	for k, v := range want {
		if c[k] != v {
			t.Errorf("scan complete %s = %q, want %q", k, c[k], v)
		}
	}
}

func TestScan_a_blind_pr_family_emits_no_rows(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.cfg.Owners = []string{"o", "p"}
	ep.conn.repos["p"] = []forge.Repo{{Path: "p/r", DefaultBranch: "main"}}
	ep.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 1, Author: "x", HeadSHA: "h1"}}, "p": {{Repo: "p/r", Number: 2, Author: "x"}}}
	ep.conn.issues = map[string][]forge.Issue{"o": {{Repo: "o/r", Number: 3, Author: "x"}}}
	ep.conn.errs = map[string]error{"prs p": fmt.Errorf("%w: result_window", forge.ErrSnapshotPartial)}
	h := newHarness(t, ep)
	h.scan(t)
	if got := lines(h.rec, "open pull request"); len(got) != 0 {
		t.Errorf("open pull request lines = %v, want none: a family one owner could not read whole emits nothing", got)
	}
	if n := h.rec.CountExact("open issue"); n != 1 {
		t.Errorf("open issue lines = %d, want the readable issue family's 1", n)
	}
	if slices.Contains(ep.conn.calls, "checks h1") {
		t.Error("the head checks of a blind pull-request family were read")
	}
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "signal_blind" {
		t.Errorf("scan degraded cause = %q, want signal_blind", l["cause"])
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	if !strings.Contains(c["failed_signals"], "open_prs") {
		t.Errorf("failed_signals = %q, want open_prs", c["failed_signals"])
	}
	for _, k := range []string{"open_prs", "open_age_lt7d", "failing_checks_prs"} {
		if _, ok := c[k]; ok {
			t.Errorf("scan complete carries %s = %s for a blind family, want it absent", k, c[k])
		}
	}
	if c["open_issues"] != "1" {
		t.Errorf("scan complete open_issues = %q, want 1", c["open_issues"])
	}
}

func TestScan_a_blind_family_on_one_connection_leaves_the_others_rows(t *testing.T) {
	a, b := githubEndpoint("a"), giteaEndpoint("b")
	a.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 1, Author: "x", UpdatedAt: now.Add(-3 * time.Hour)}}}
	a.cfg.Owners = []string{"o", "p"}
	a.conn.repos["p"] = []forge.Repo{{Path: "p/r", DefaultBranch: "main"}}
	a.conn.errs = map[string]error{"prs p": errors.New("upstream 502")}
	b.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 2, Author: "x", UpdatedAt: now.Add(-time.Hour)}}}
	h := newHarness(t, a, b)
	h.scan(t)
	got := lines(h.rec, "open pull request")
	if len(got) != 1 || got[0]["connection"] != "b" || got[0]["rank"] != "1" {
		t.Errorf("open pull request lines = %v, want only b's row at rank 1 under the shared scan_id", got)
	}
	if c := lineFor(t, h.rec, "scan complete", "b"); c["open_prs"] != "1" || c["degraded"] != "false" {
		t.Errorf("b scan complete = %v, want its clean pull request", c)
	}
}

func TestScan_fallback_project_failure_makes_the_family_blind(t *testing.T) {
	ep := githubEndpoint("gl")
	ep.conn.product = forge.ProductGitLab
	ep.gh = nil
	ep.conn.repos["o"] = append(ep.conn.repos["o"], forge.Repo{Path: "o/b", DefaultBranch: "main"})
	ep.conn.errs = map[string]error{
		"prs o": forge.ErrOwnerUnresolved, "issues o": forge.ErrOwnerUnresolved,
		"repo-prs o/b": fmt.Errorf("%w: labels cut short", forge.ErrSnapshotPartial),
	}
	ep.conn.prs = map[string][]forge.PullRequest{"o/r": {{Repo: "o/r", Number: 1, Ref: "!1", Author: "x"}}}
	ep.conn.issues = map[string][]forge.Issue{"o/r": {{Repo: "o/r", Number: 2, Author: "x"}}}
	h := newHarness(t, ep)
	h.scan(t)
	if n := h.rec.CountExact("open pull request"); n != 0 {
		t.Errorf("open pull request lines = %d, want none: one unreadable project leaves the owner's snapshot incomplete", n)
	}
	if n := h.rec.CountExact("open issue"); n != 1 {
		t.Errorf("open issue lines = %d, want the issue family's 1", n)
	}
	if l := lineFor(t, h.rec, "scan degraded", "gl"); l["cause"] != "signal_blind" {
		t.Errorf("scan degraded cause = %q, want signal_blind", l["cause"])
	}
	if c := lineFor(t, h.rec, "scan complete", "gl"); c["open_issues"] != "1" {
		t.Errorf("scan complete = %v, want open_prs absent and open_issues 1", c)
	} else if _, ok := c["open_prs"]; ok {
		t.Errorf("scan complete open_prs = %s, want it absent", c["open_prs"])
	}
}

func TestScan_overlapping_owners_keep_each_item_once(t *testing.T) {
	ep := githubEndpoint("gl")
	ep.conn.product = forge.ProductGitLab
	ep.gh = nil
	ep.cfg.Owners = []string{"group", "group/sub"}
	sub := forge.Repo{Path: "group/sub/r", DefaultBranch: "main"}
	ep.conn.repos = map[string][]forge.Repo{"group": {sub}, "group/sub": {sub}}
	pr := forge.PullRequest{Repo: "group/sub/r", Number: 1, Ref: "!1", Author: "x"}
	issue := forge.Issue{Repo: "group/sub/r", Number: 1, Author: "x"}
	ep.conn.prs = map[string][]forge.PullRequest{"group": {pr}, "group/sub": {pr}}
	ep.conn.issues = map[string][]forge.Issue{"group": {issue}, "group/sub": {issue}}
	h := newHarness(t, ep)
	h.scan(t)
	if n := h.rec.CountExact("open pull request"); n != 1 {
		t.Errorf("open pull request lines = %d, want 1 for a merge request both a group and its subgroup list", n)
	}
	if n := h.rec.CountExact("open issue"); n != 1 {
		t.Errorf("open issue lines = %d, want 1", n)
	}
	if c := lineFor(t, h.rec, "scan complete", "gl"); c["open_prs"] != "1" || c["open_issues"] != "1" {
		t.Errorf("scan complete open_prs %s open_issues %s, want 1 and 1", c["open_prs"], c["open_issues"])
	}
}

func TestScan_overlapping_owners_count_each_skipped_repository_once(t *testing.T) {
	ep := githubEndpoint("gl")
	ep.conn.product = forge.ProductGitLab
	ep.gh = nil
	ep.cfg.Owners = []string{"group", "group/sub"}
	ep.cfg.ExcludeRepos = map[string]bool{"group/sub/x": true}
	kept := forge.Repo{Path: "group/sub/r", DefaultBranch: "main"}
	excluded := forge.Repo{Path: "group/sub/x", DefaultBranch: "main"}
	ep.conn.repos = map[string][]forge.Repo{"group": {kept, excluded}, "group/sub": {kept, excluded}}
	ep.conn.errs = map[string]error{"prs group/sub": forge.ErrOwnerUnresolved, "issues group/sub": forge.ErrOwnerUnresolved}
	h := newHarness(t, ep)
	h.scan(t)
	c := lineFor(t, h.rec, "scan complete", "gl")
	if c["repos_discovered"] != "1" || c["skipped"] != "1" {
		t.Errorf("scan complete repos_discovered %q skipped %q, want 1 and 1: two owners listing one excluded repository skip it once",
			c["repos_discovered"], c["skipped"])
	}
	if !slices.Contains(ep.conn.calls, "repo-prs group/sub/r") || slices.Contains(ep.conn.calls, "repo-prs group/sub/x") {
		t.Errorf("calls %v, want the subgroup's per-project fallback over its kept repository alone", ep.conn.calls)
	}
}

func TestScan_every_run_read_failing_emits_no_state_derived_rows(t *testing.T) {
	ep := githubEndpoint("gh")
	r := run(1, forge.RunFailing, now.Add(-time.Hour))
	var timedRuns []forge.Run
	for i := range 3 {
		tr := run(int64(10+i), forge.RunPassing, now.Add(-time.Duration(i+2)*time.Hour))
		tr.Workflow = "build"
		timedRuns = append(timedRuns, tr)
	}
	ep.conn.runs = map[string]listing{"o/r": {Runs: append([]forge.Run{r}, timedRuns...)}}
	h := newHarness(t, ep)
	h.scan(t)
	lineFor(t, h.rec, "failing workflow", "gh")
	lineFor(t, h.rec, "slow workflow", "gh")
	ep.conn.errs = map[string]error{"runs o/r": errors.New("upstream 502")}
	h.scan(t)
	for _, msg := range []string{"failing workflow", "slow workflow"} {
		if n := h.rec.CountExact(msg); n != 0 {
			t.Errorf("%q lines with every run read failed = %d, want none", msg, n)
		}
	}
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "runs_blind" {
		t.Errorf("scan degraded cause = %q, want runs_blind", l["cause"])
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	for _, k := range []string{"failing_workflows", "failing_workflows_listed", "slow_workflows_listed", "run_durations_known"} {
		if _, ok := c[k]; ok {
			t.Errorf("scan complete carries %s = %s with the run family blind, want it absent", k, c[k])
		}
	}
	if c["runs_read"] != "blind" || c["failing_workflows_read"] != "blind" {
		t.Errorf("scan complete runs_read %q failing_workflows_read %q, want blind and blind", c["runs_read"], c["failing_workflows_read"])
	}
}

func TestScan_capped_families_keep_each_forges_first_rows(t *testing.T) {
	gh := githubEndpoint("gh")
	var runs []forge.Run
	for i := range 30 {
		r := run(int64(i+1), forge.RunFailing, now.Add(-time.Duration(60-i)*time.Hour))
		r.Workflow = fmt.Sprintf("wf-%02d", i)
		runs = append(runs, r)
	}
	gh.conn.runs = map[string]listing{"o/r": {Runs: runs}}
	eps := []*endpoint{gh}
	for _, p := range []forge.Product{forge.ProductGitLab, forge.ProductGitea, forge.ProductForgejo} {
		ep := giteaEndpoint(p.String())
		ep.conn.product = p
		ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
		eps = append(eps, ep)
	}
	h := newHarness(t, eps...)
	h.scan(t)
	byForge := map[string][]map[string]string{}
	for _, l := range lines(h.rec, "failing workflow") {
		byForge[l["forge"]] = append(byForge[l["forge"]], l)
	}
	if n := len(byForge["github"]); n != maxFailingWorkflows {
		t.Errorf("github failing workflow lines = %d, want the %d it ranks first", n, maxFailingWorkflows)
	}
	for _, f := range []string{"gitlab", "gitea", "forgejo"} {
		got := byForge[f]
		if len(got) != 1 || got[0]["forge_rank"] != "1" {
			t.Errorf("%s failing workflow lines = %v, want its one row at forge_rank 1 beside a full global cap", f, got)
			continue
		}
		if rk, _ := strconv.Atoi(got[0]["rank"]); rk <= maxFailingWorkflows {
			t.Errorf("%s row rank = %d, want its true global rank past the cap of %d", f, rk, maxFailingWorkflows)
		}
	}
	if c := lineFor(t, h.rec, "scan complete", "gitea"); c["failing_workflows_listed"] != "1" {
		t.Errorf("gitea failing_workflows_listed = %s, want 1", c["failing_workflows_listed"])
	}
}

func TestScan_truncated_runs_and_discovery_degrade_without_escalating(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.truncated = true
	ep.conn.runs = map[string]listing{"o/r": {Truncated: true}}
	h := newHarness(t, ep)
	h.scan(t)
	if h.rec.CountExact("scan degraded") != 0 {
		t.Errorf("truncation escalated: %v", lines(h.rec, "scan degraded"))
	}
	if h.rec.CountExact("runs listing truncated") != 1 {
		t.Errorf("runs listing truncated lines = %d, want 1", h.rec.CountExact("runs listing truncated"))
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	if c["degraded"] != "true" || c["repos_truncated"] != "true" || c["runs_truncated_repos"] != "1" || c["failing_workflows_read"] != "partial" {
		t.Errorf("scan complete = %v, want degraded, repos truncated, 1 truncated runs repo, failing workflows partial", c)
	}
}

func TestScan_a_cut_listing_above_a_run_the_last_listing_returned_pending_reports_a_coverage_gap(t *testing.T) {
	tests := []struct {
		name    string
		oldest  time.Time
		wantGap int
	}{
		{"cut_above_the_pending_run", now.Add(-50 * time.Minute), 1},
		{"cut_reaching_the_pending_run", now.Add(-11 * time.Hour), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := githubEndpoint("gh")
			pending := run(8, forge.RunPassing, now.Add(-10*time.Hour))
			ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunPassing, now.Add(-20*time.Hour))}, Pending: []forge.Run{pending}}}
			h := newHarness(t, ep)
			h.scan(t)
			*h.clock = now.Add(44 * time.Hour)
			ep.conn.runs["o/r"] = listing{Runs: []forge.Run{run(9, forge.RunFailing, tc.oldest)}, Truncated: true}
			h.scan(t)
			got := lines(h.rec, "runs coverage gap")
			if len(got) != tc.wantGap {
				t.Fatalf("runs coverage gap lines after a cut with its oldest row at %s, run 8 pending at %s = %v, want %d", tc.oldest, pending.CreatedAt, got, tc.wantGap)
			}
			if tc.wantGap == 1 && (got[0]["repo"] != "o/r" || got[0]["pending_since"] != stamp(pending.CreatedAt) || got[0]["last_listed"] != stamp(now)) {
				t.Errorf("runs coverage gap = %v, want repo o/r, pending_since %s and last_listed %s", got[0], stamp(pending.CreatedAt), stamp(now))
			}
			*h.clock = now.Add(88 * time.Hour)
			ep.conn.runs["o/r"] = listing{}
			h.scan(t)
			if n := len(lines(h.rec, "runs coverage gap")); n != 0 {
				t.Errorf("runs coverage gap lines on the whole listing after = %d, want 0: the gap is reported once", n)
			}
		})
	}
}

func TestScan_truncated_discovery_does_not_forget_workflows(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.repos["o"] = append(ep.conn.repos["o"], forge.Repo{Path: "o/b", DefaultBranch: "main"})
	b := run(5, forge.RunFailing, now.Add(-time.Hour))
	b.Repo = "o/b"
	ep.conn.runs = map[string]listing{"o/b": {Runs: []forge.Run{b}}}
	h := newHarness(t, ep)
	h.scan(t)
	ep.conn.repos["o"] = ep.conn.repos["o"][:1]
	ep.conn.truncated = true
	h.scan(t)
	h.restart(t)
	ep.conn.repos["o"] = append(ep.conn.repos["o"], forge.Repo{Path: "o/b", DefaultBranch: "main"})
	ep.conn.truncated = false
	ep.conn.runs = map[string]listing{}
	h.scan(t)
	if l := lineFor(t, h.rec, "failing workflow", "gh"); l["repo"] != "o/b" {
		t.Errorf("after a truncated discovery missed o/b, failing workflow = %v, want o/b still known", l)
	}
	ep.conn.repos["o"] = ep.conn.repos["o"][:1]
	h.scan(t)
	h.scan(t)
	if n := h.rec.CountExact("failing workflow"); n != 0 {
		t.Errorf("after a complete discovery without o/b, %d failing workflow line(s), want 0", n)
	}
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["tracked"] != "1" {
		t.Errorf("tracked after o/b left a complete discovery = %s, want 1: its delivered run stays until it leaves the window, so a return reports nothing", c["tracked"])
	}
}

func TestScan_a_cut_discovery_with_no_usable_row_is_partial_not_no_repos(t *testing.T) {
	tests := []struct {
		name  string
		repos []forge.Repo
	}{
		{"empty_prefix", nil},
		{"archived_prefix", []forge.Repo{{Path: "o/old", DefaultBranch: "main", Archived: true}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := githubEndpoint("gh")
			ep.conn.repos["o"] = tc.repos
			ep.conn.truncated = true
			h := newHarness(t, ep)
			h.scan(t)
			if got := lines(h.rec, "scan degraded"); len(got) != 0 {
				t.Errorf("cut discovery with no kept row (%s) logged scan degraded %v, want the truncation alone", tc.name, got)
			}
			c := lineFor(t, h.rec, "scan complete", "gh")
			if c["degraded"] != "true" || c["repos_truncated"] != "true" || c["failed_signals"] != "repos" {
				t.Errorf("scan complete (%s) = %v, want degraded, repos truncated, failed_signals repos", tc.name, c)
			}
		})
	}
}

func TestScan_undated_runs_on_every_repository_are_unsupported(t *testing.T) {
	ep := giteaEndpoint("gt")
	ep.conn.repos["o"] = append(ep.conn.repos["o"], forge.Repo{Path: "o/b", DefaultBranch: "main"})
	ep.conn.errs = map[string]error{"runs o/r": forge.ErrRunsUndated, "runs o/b": forge.ErrRunsUndated}
	h := newHarness(t, ep)
	h.scan(t)
	c := lineFor(t, h.rec, "scan complete", "gt")
	if !strings.Contains(c["unsupported_signals"], "runs") || c["degraded"] != "false" {
		t.Errorf("scan complete = unsupported %q degraded %s, want runs unsupported and not degraded", c["unsupported_signals"], c["degraded"])
	}
	if _, ok := c["new_runs"]; ok {
		t.Error("scan complete carries run counts where runs are unsupported")
	}
	if h.rec.CountExact("run listing carries no creation time; runs not read") != 1 {
		t.Error("no warning names the undated run listing")
	}
}

func TestScan_undated_runs_beside_repositories_with_no_run_are_unsupported(t *testing.T) {
	ep := giteaEndpoint("gt")
	ep.conn.repos["o"] = append(ep.conn.repos["o"], forge.Repo{Path: "o/b", DefaultBranch: "main"})
	ep.conn.errs = map[string]error{"runs o/r": forge.ErrRunsUndated}
	h := newHarness(t, ep)
	h.scan(t)
	c := lineFor(t, h.rec, "scan complete", "gt")
	if !strings.Contains(c["unsupported_signals"], "runs") || c["runs_read"] != "unsupported" || c["degraded"] != "false" {
		t.Errorf("an undated repository beside one with no run: scan complete = %v, want runs unsupported and not degraded: an empty list dates nothing", c)
	}
	if h.rec.CountExact("run listing carries no creation time; runs not read") != 1 {
		t.Error("no warning names the undated run listing")
	}
}

func TestScan_a_failed_run_listing_beside_undated_ones_degrades_nothing(t *testing.T) {
	ep := giteaEndpoint("gt")
	ep.conn.repos["o"] = append(ep.conn.repos["o"], forge.Repo{Path: "o/b", DefaultBranch: "main"})
	ep.conn.errs = map[string]error{"runs o/r": forge.ErrRunsUndated, "runs o/b": errors.New("server error")}
	h := newHarness(t, ep)
	h.scan(t)
	c := lineFor(t, h.rec, "scan complete", "gt")
	if c["runs_read"] != "unsupported" || c["degraded"] != "false" || c["errors"] != "0" || c["failed_signals"] != "" {
		t.Errorf("a failed run listing on a product with undated runs: scan complete = %v, want runs unsupported, nothing degraded or failed", c)
	}
	if got := lines(h.rec, "scan degraded"); len(got) != 0 {
		t.Errorf("a failed run listing on a product with undated runs logged scan degraded %v, want none: the product has no run signals", got)
	}
}

func TestScan_undated_runs_beside_a_dated_run_are_a_failed_read(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending_%t", pending), func(t *testing.T) {
			ep := giteaEndpoint("gt")
			ep.conn.repos["o"] = append(ep.conn.repos["o"], forge.Repo{Path: "o/b", DefaultBranch: "main"})
			ep.conn.errs = map[string]error{"runs o/r": forge.ErrRunsUndated}
			r := run(1, forge.RunPassing, now.Add(-time.Hour))
			r.Repo = "o/b"
			dated := listing{Runs: []forge.Run{r}}
			if pending {
				dated = listing{Pending: []forge.Run{r}}
			}
			ep.conn.runs = map[string]listing{"o/b": dated}
			h := newHarness(t, ep)
			h.scan(t)
			c := lineFor(t, h.rec, "scan complete", "gt")
			if c["runs_failed_repos"] != "1" || c["runs_read"] != "partial" || strings.Contains(c["unsupported_signals"], "runs") {
				t.Errorf("an undated repository beside a dated run (pending %t): scan complete = %v, want 1 failed repo, runs partial and supported", pending, c)
			}
			if got := lines(h.rec, "runs listing failed"); len(got) != 1 || got[0]["repo"] != "o/r" {
				t.Errorf("an undated repository beside a dated run (pending %t): runs listing failed lines = %v, want one naming o/r", pending, got)
			}
		})
	}
}

func TestScan_github_only_signals(t *testing.T) {
	gh := githubEndpoint("gh")
	gh.conn.repos["o"] = append(gh.conn.repos["o"],
		forge.Repo{Path: "o/fork", Fork: true}, forge.Repo{Path: "o/none"}, forge.Repo{Path: "o/private", Private: true}, forge.Repo{Path: "o/skip"})
	gh.cfg.Security.SkipRepos = map[string]bool{"o/skip": true}
	gh.gh.alerts = map[string][]forge.Alert{"o/r": {
		{Repo: "o/r", Number: 1, Severity: "high", Tool: "CodeQL", Source: "code_scanning"},
		{Repo: "o/r", Number: 2, Severity: "critical", Tool: "CodeQL", Source: "code_scanning"},
		{Repo: "o/r", Number: 3, Tool: "Trivy", Source: "code_scanning"},
	}, "o/private": {{Repo: "o/private", Number: 9, Severity: "high", Tool: "CodeQL", Source: "code_scanning"}}}
	gh.gh.none = map[string]bool{"o/none": true}
	gh.gh.workflows = map[string][]forge.Workflow{"o/r": {{Repo: "o/r", Name: "nightly", State: "disabled_inactivity"}}}
	gt := giteaEndpoint("gt")
	h := newHarness(t, gh, gt)
	h.scan(t)
	c := lineFor(t, h.rec, "scan complete", "gh")
	want := map[string]string{
		"security_alerts": "3", "security_alerts_critical": "1", "security_alerts_high": "1", "security_alerts_unrated": "1",
		"security_repos_read": "1", "security_skipped": "3", "security_unavailable": "1", "security_unreadable": "0",
		"disabled_workflows": "1", "security_alerts_read": "complete", "disabled_workflows_read": "complete",
	}
	for k, v := range want {
		if c[k] != v {
			t.Errorf("github scan complete %s = %q, want %q", k, c[k], v)
		}
	}
	if top := lines(h.rec, "top security alert"); len(top) != 3 || top[0]["number"] != "2" || top[0]["rank"] != "1" {
		t.Errorf("top security alert = %v, want the critical alert first of 3", top)
	}
	if h.rec.CountExact("security alert") != 3 || h.rec.CountExact("disabled workflow") != 1 {
		t.Errorf("security alert %d disabled workflow %d, want 3 and 1", h.rec.CountExact("security alert"), h.rec.CountExact("disabled workflow"))
	}
	if slices.Contains(gh.gh.alertCalls, "o/private") || !slices.Contains(gh.gh.workflowCalls, "o/private") {
		t.Errorf("alert reads %v workflow reads %v, want o/private's code scanning skipped by default and its workflows read", gh.gh.alertCalls, gh.gh.workflowCalls)
	}
	if l := lines(h.rec, "code scanning unreadable"); len(l) != 0 {
		t.Errorf("code scanning unreadable = %v, want none: o/none's 404 is no analyses and o/private is skipped", l)
	}
	g := lineFor(t, h.rec, "scan complete", "gt")
	if !strings.Contains(g["unsupported_signals"], "security_alerts") || !strings.Contains(g["unsupported_signals"], "disabled_workflows") {
		t.Errorf("gitea unsupported_signals = %q, want security_alerts and disabled_workflows", g["unsupported_signals"])
	}
	if _, ok := g["security_alerts"]; ok {
		t.Error("gitea scan complete carries security_alerts, want it absent, never 0")
	}
}

func TestScan_include_private_reads_code_scanning_of_a_private_repository(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.repos["o"] = []forge.Repo{{Path: "o/p", DefaultBranch: "main", Private: true}}
	ep.cfg.Security.IncludePrivate = true
	ep.gh.alerts = map[string][]forge.Alert{"o/p": {{Repo: "o/p", Number: 1, Severity: "high", Tool: "CodeQL", Source: "code_scanning"}}}
	h := newHarness(t, ep)
	h.scan(t)
	c := lineFor(t, h.rec, "scan complete", "gh")
	if !slices.Equal(ep.gh.alertCalls, []string{"o/p"}) || c["security_alerts"] != "1" || c["security_skipped"] != "0" {
		t.Errorf("include_private: alert reads %v, security_alerts %q, security_skipped %q, want o/p read with its 1 alert and nothing skipped",
			ep.gh.alertCalls, c["security_alerts"], c["security_skipped"])
	}
}

func TestScan_a_github_403_that_is_no_throttle_stops_later_github_only_reads(t *testing.T) {
	tests := []struct {
		name, call, warn, hint string
		calls                  int
	}{
		{name: "alerts", call: "alerts o/a", warn: "code scanning unreadable", hint: "security.skip_repos", calls: 1},
		{name: "workflows", call: "workflows o/a", warn: "workflows listing failed", hint: "Actions", calls: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := githubEndpoint("gh")
			ep.conn.repos["o"] = []forge.Repo{{Path: "o/a", DefaultBranch: "main"}, {Path: "o/b", DefaultBranch: "main"}}
			ep.gh.errs = map[string]error{tc.call: forge.ErrForbidden}
			h := newHarness(t, ep)
			h.scan(t)
			if ep.gh.calls != tc.calls {
				t.Errorf("GitHub-only requests after a 403 on %s = %d (alerts %v workflows %v), want %d: nothing after the refusal",
					tc.call, ep.gh.calls, ep.gh.alertCalls, ep.gh.workflowCalls, tc.calls)
			}
			w := lineFor(t, h.rec, tc.warn, "gh")
			if w["level"] != "WARN" || w["repo"] != "o/a" || !strings.Contains(w["hint"], tc.hint) {
				t.Errorf("%s = %v, want a WARN naming o/a with a hint naming %s", tc.warn, w, tc.hint)
			}
			c := lineFor(t, h.rec, "scan complete", "gh")
			if c["degraded"] != "true" || c["security_alerts_read"] == "complete" || c["disabled_workflows_read"] == "complete" {
				t.Errorf("scan complete after the refusal = degraded %q security_alerts_read %q disabled_workflows_read %q, want degraded and neither read complete",
					c["degraded"], c["security_alerts_read"], c["disabled_workflows_read"])
			}
			if d := lineFor(t, h.rec, "scan degraded", "gh"); d["cause"] != "github_refused" {
				t.Errorf("scan degraded cause = %q, want github_refused, never rate_limited", d["cause"])
			}
			noLineFor(t, h.rec, "scan stopped", "gh")
		})
	}
}

func TestScan_a_github_403_on_a_forgeapi_read_sends_no_further_request_of_either_client(t *testing.T) {
	tests := []struct {
		name, call, warn, hint string
		// notHint is a remedy the hint must not name.
		notHint string
		sent    []string
		unread  map[string]string
	}{
		{
			name: "runs", call: "runs o/a", warn: "runs listing failed", hint: "Actions",
			sent:   []string{"discover o", "prs o", "issues o", "runs o/a"},
			unread: map[string]string{"runs_unread_repos": "1", "security_unread_repos": "2", "workflows_unread_repos": "2"},
		},
		{
			name: "checks", call: "checks h1", warn: "pull request checks unreadable", hint: "a classic token with repo", notHint: "Checks read",
			sent:   []string{"discover o", "prs o", "issues o", "runs o/a", "runs o/b", "checks h1"},
			unread: map[string]string{"checks_unread": "2", "security_unread_repos": "2", "workflows_unread_repos": "2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := githubEndpoint("gh")
			ep.conn.repos["o"] = []forge.Repo{{Path: "o/a", DefaultBranch: "main"}, {Path: "o/b", DefaultBranch: "main"}}
			ep.conn.prs = map[string][]forge.PullRequest{"o": {
				{Repo: "o/a", Number: 1, Author: "x", HeadSHA: "h1"}, {Repo: "o/b", Number: 2, Author: "x", HeadSHA: "h2"},
			}}
			ep.conn.errs = map[string]error{tc.call: forge.ErrForbidden}
			h := newHarness(t, ep)
			h.scan(t)
			if !slices.Equal(ep.conn.calls, tc.sent) || ep.gh.calls != 0 {
				t.Errorf("requests after a 403 on %s = %v and %d GitHub-only, want %v and none", tc.call, ep.conn.calls, ep.gh.calls, tc.sent)
			}
			w := lineFor(t, h.rec, tc.warn, "gh")
			if w["level"] != "WARN" || !strings.Contains(w["hint"], tc.hint) || !strings.Contains(w["hint"], "no further request") {
				t.Errorf("%s = %v, want a WARN whose hint names %s and the requests stopped", tc.warn, w, tc.hint)
			}
			if tc.notHint != "" && strings.Contains(w["hint"], tc.notHint) {
				t.Errorf("%s hint = %q, want no %q: a fine-grained GitHub token has no such permission", tc.warn, w["hint"], tc.notHint)
			}
			c := lineFor(t, h.rec, "scan complete", "gh")
			for k, want := range tc.unread {
				if c[k] != want {
					t.Errorf("scan complete %s = %q, want %s: every read the refusal held back is counted", k, c[k], want)
				}
			}
			if c["degraded"] != "true" {
				t.Errorf("scan complete degraded = %q, want true", c["degraded"])
			}
			if d := lineFor(t, h.rec, "scan degraded", "gh"); d["cause"] != "github_refused" {
				t.Errorf("scan degraded cause = %q, want github_refused", d["cause"])
			}
			h.scan(t)
			if !slices.Contains(ep.conn.calls[len(tc.sent):], "discover o") {
				t.Errorf("the next scan sent %v, want it to read again: the refusal holds one scan", ep.conn.calls[len(tc.sent):])
			}
		})
	}
}

func TestScan_code_scanning_blind_escalates(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.gh.errs = map[string]error{"alerts o/r": errors.New("upstream 502")}
	h := newHarness(t, ep)
	h.scan(t)
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "code_scanning_blind" {
		t.Errorf("cause = %q, want code_scanning_blind", l["cause"])
	}
}

func TestScan_an_alert_with_no_reported_time_ranks_after_the_dated_ones(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.gh.alerts = map[string][]forge.Alert{"o/r": {
		{Repo: "o/r", Number: 1, Severity: "high", Tool: "t"},
		{Repo: "o/r", Number: 2, Severity: "high", Tool: "t", CreatedAt: now.Add(-time.Hour)},
	}}
	h := newHarness(t, ep)
	h.scan(t)
	got := map[string]map[string]string{}
	for _, l := range lines(h.rec, "security alert") {
		got[l["number"]] = l
	}
	if v, ok := got["1"]["created_at"]; ok {
		t.Errorf("alert #1 carries created_at = %q for a time the forge did not report, want it absent", v)
	}
	if got["2"]["rank"] != "1" || got["1"]["rank"] != "2" {
		t.Errorf("alert ranks #2 %q #1 %q, want the dated alert first and the undated one after it", got["2"]["rank"], got["1"]["rank"])
	}
}

func TestScan_alerts_and_tools_are_capped(t *testing.T) {
	var eps []*endpoint
	for i := range 3 {
		ep := githubEndpoint("gh" + strconv.Itoa(i))
		var alerts []forge.Alert
		for n := range 400 {
			alerts = append(alerts, forge.Alert{Repo: "o/r", Number: int64(n), Severity: "low", Tool: "tool-" + strconv.Itoa(n%13)})
		}
		ep.gh.alerts = map[string][]forge.Alert{"o/r": alerts}
		eps = append(eps, ep)
	}
	h := newHarness(t, eps...)
	h.scan(t)
	if n := h.rec.CountExact("top security alert"); n != 25 {
		t.Errorf("top security alert lines = %d, want 25", n)
	}
	got := lines(h.rec, "security tool")
	if len(got) != 11 {
		t.Fatalf("security tool lines in one scan of three connections = %d, want 11 (10 and the rollup)", len(got))
	}
	total := 0
	for i, l := range got {
		n, _ := strconv.Atoi(l["alerts"])
		total += n
		if l["forge"] != "github" || l["connection"] != "" || l["rank"] != strconv.Itoa(i+1) {
			t.Errorf("security tool line %d = %v, want forge github, no connection, rank %d", i, l, i+1)
		}
		if (l["rollup"] == "true") != (i == 10) {
			t.Errorf("security tool line %d rollup = %s, want only the 11th line marked", i, l["rollup"])
		}
	}
	if total != 1200 {
		t.Errorf("security tool alerts sum = %d, want all 1200 alerts of the scan", total)
	}
}

func TestScan_security_tools_rank_across_connections_and_mark_the_rollup(t *testing.T) {
	a, b := githubEndpoint("a"), githubEndpoint("b")
	var alerts []forge.Alert
	add := func(tool string, n int) {
		for range n {
			alerts = append(alerts, forge.Alert{Repo: "o/r", Number: int64(len(alerts) + 1), Severity: "low", Tool: tool})
		}
	}
	add("other", 50)
	for i := range 10 {
		add("tool-"+strconv.Itoa(i), 20-i)
	}
	a.gh.alerts = map[string][]forge.Alert{"o/r": alerts}
	b.gh.alerts = map[string][]forge.Alert{"o/r": {
		{Repo: "o/r", Number: 1, Severity: "low", Tool: "tool-9"}, {Repo: "o/r", Number: 2, Severity: "low", Tool: "tool-9"},
	}}
	h := newHarness(t, a, b)
	h.scan(t)
	got := lines(h.rec, "security tool")
	if len(got) != 11 {
		t.Fatalf("security tool lines = %d, want 11 for eleven tool names over two connections", len(got))
	}
	if got[0]["tool"] != "other" || got[0]["alerts"] != "50" || got[0]["rollup"] != "false" {
		t.Errorf("first security tool line = %v, want the real tool named other, 50 alerts, not the rollup", got[0])
	}
	rollups, nine := 0, false
	for _, l := range got {
		nine = nine || (l["tool"] == "tool-9" && l["alerts"] == "13")
		if l["rollup"] == "true" {
			rollups++
			if _, named := l["tool"]; named || l["alerts"] != "12" {
				t.Errorf("rollup line = %v, want no tool name and tool-8's 12 alerts, last of the scan-wide ranking", l)
			}
		}
	}
	if !nine {
		t.Errorf("security tool lines = %v, want tool-9 listed with 13 alerts summed over both connections", got)
	}
	if rollups != 1 {
		t.Errorf("rollup lines = %d, want exactly 1", rollups)
	}
}

func TestScan_rank_and_scan_id_span_connections_and_the_burst_follows_the_last_read(t *testing.T) {
	a, b := githubEndpoint("a"), giteaEndpoint("b")
	a.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 1, Author: "x", UpdatedAt: now.Add(-time.Hour)}}}
	b.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 2, Author: "x", UpdatedAt: now.Add(-2 * time.Hour)}}}
	h := newHarness(t, a, b)
	h.scan(t)
	got := lines(h.rec, "open pull request")
	if len(got) != 2 || got[0]["connection"] != "b" || got[0]["rank"] != "1" || got[1]["rank"] != "2" ||
		got[0]["forge_rank"] != "1" || got[1]["forge_rank"] != "1" || got[0]["scan_id"] != got[1]["scan_id"] {
		t.Errorf("open pull request = %v, want b's idler row rank 1, a's rank 2, each forge_rank 1, one scan_id", got)
	}
	if got[0]["scan_id"] != strconv.FormatInt(now.UnixMilli(), 10) {
		t.Errorf("open pull request scan_id = %q, want the scan start in Unix milliseconds, %d", got[0]["scan_id"], now.UnixMilli())
	}
	msgs := h.rec.Messages()
	lastScanning, firstPR := -1, -1
	for i, m := range msgs {
		if m == "scanning" {
			lastScanning = i
		}
		if m == "open pull request" && firstPR < 0 {
			firstPR = i
		}
	}
	if firstPR < lastScanning {
		t.Errorf("a snapshot line came before the last connection's reads began: %v", msgs)
	}
	for _, c := range lines(h.rec, "scan complete") {
		if c["scan_id"] != got[0]["scan_id"] {
			t.Errorf("scan complete scan_id %s, want %s", c["scan_id"], got[0]["scan_id"])
		}
	}
}

func TestScan_cancelled_scan_emits_no_burst(t *testing.T) {
	a := githubEndpoint("a")
	a.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 1, Author: "x"}}}
	a.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	h := newHarness(t, a)
	h.rec = h.resetRecorder(t)
	ctx, cancel := context.WithCancel(t.Context())
	a.conn.errs = map[string]error{"checks ": nil}
	a.conn.budgetAfter = nil
	cancelAfterRuns := &cancelling{fakeConn: a.conn, cancel: cancel}
	h.c.conns[0].conn = cancelAfterRuns
	h.c.conns[0].gh = a.gh
	h.c.Scan(ctx)
	for _, msg := range []string{"open pull request", "scan complete", "failing workflow"} {
		if n := h.rec.CountExact(msg); n != 0 {
			t.Errorf("a cancelled scan logged %d %q line(s), want none", n, msg)
		}
	}
	if n := h.rec.CountExact("ci run"); n != 1 {
		t.Errorf("ci run lines = %d, want the 1 run read before the cancel", n)
	}
	h.restart(t)
	h.scan(t)
	if n := h.rec.CountExact("ci run"); n != 0 {
		t.Errorf("the run read before the cancel was re-emitted %d time(s): the state must be saved", n)
	}
}

// cancelling cancels the scan's context once runs are read.
type cancelling struct {
	*fakeConn
	cancel context.CancelFunc
}

func (c *cancelling) RunsSince(ctx context.Context, repo forge.Repo, since time.Time) (forge.RunList, error) {
	defer c.cancel()
	return c.fakeConn.RunsSince(ctx, repo, since)
}

func (*cancelling) CommitChecks(ctx context.Context, _ *forge.PullRequest) (forge.CheckResult, error) {
	return forge.CheckResult{}, ctx.Err()
}

func TestScan_read_reserve_stops_one_connection(t *testing.T) {
	a, b := githubEndpoint("a"), githubEndpoint("b")
	a.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 1, Author: "x"}}}
	b.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 2, Author: "x"}}}
	a.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	a.conn.errs = map[string]error{"runs o/r": nil, "issues o": forge.ErrReadDeferred}
	h := newHarness(t, a, b)
	h.scan(t)
	noLineFor(t, h.rec, "scan complete", "a")
	noLineFor(t, h.rec, "open pull request", "a")
	s := lineFor(t, h.rec, "scan stopped", "a")
	if s["reason"] != "read_reserve" || s["phase"] != "open issues" || s["level"] != "WARN" || s["budget_remaining"] != "3996" {
		t.Errorf("scan stopped = %v, want read_reserve in open issues at WARN with the budget", s)
	}
	lineFor(t, h.rec, "scan complete", "b")
	lineFor(t, h.rec, "open pull request", "b")
	noLineFor(t, h.rec, "scan degraded", "a")
	h.scan(t)
	if s := lineFor(t, h.rec, "scan stopped", "a"); s["level"] != "INFO" {
		t.Errorf("a second stop in the same budget window logged at %s, want INFO, still the stall alert's heartbeat", s["level"])
	}
}

func TestScan_rate_limited_stops_and_escalates(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.errs = map[string]error{"runs o/r": forge.ErrRateLimited}
	h := newHarness(t, ep)
	h.scan(t)
	if s := lineFor(t, h.rec, "scan stopped", "gh"); s["reason"] != "rate_limited" {
		t.Errorf("scan stopped reason = %q, want rate_limited", s["reason"])
	}
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "rate_limited" {
		t.Errorf("scan degraded cause = %q, want rate_limited", l["cause"])
	}
	noLineFor(t, h.rec, "scan complete", "gh")
}

func TestScan_a_shared_quota_at_the_reserve_holds_every_read_of_both_clients(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.quota.remaining = forge.ReadReserve + 1
	h := newHarness(t, ep)
	h.scan(t)
	if s := lineFor(t, h.rec, "scan stopped", "gh"); s["reason"] != "read_reserve" || s["phase"] != "discovery" {
		t.Errorf("scan stopped = %v, want read_reserve in discovery: the open left the shared budget at the reserve", s)
	}
	if len(ep.conn.calls) != 0 || ep.gh.calls != 0 {
		t.Errorf("reads sent at the reserve = %v and %d GitHub-only, want none", ep.conn.calls, ep.gh.calls)
	}
}

func TestScan_one_shared_quota_scan_never_crosses_the_reserve(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.repos["o"] = []forge.Repo{{Path: "o/a", DefaultBranch: "main"}, {Path: "o/b", DefaultBranch: "main"}, {Path: "o/c", DefaultBranch: "main"}}
	for start := forge.ReadReserve + 1; start <= forge.ReadReserve+12; start++ {
		ep.conn.quota.remaining, ep.conn.quota.least = start, start
		ep.conn.calls, ep.gh.calls = nil, 0
		h := newHarness(t, ep)
		h.scan(t)
		if q := ep.conn.quota; q.least < forge.ReadReserve {
			t.Errorf("a scan from %d remaining took the shared budget to %d, want it never under the %d reserve", start, q.least, forge.ReadReserve)
		}
		if s := lineFor(t, h.rec, "scan stopped", "gh"); s["reason"] != "read_reserve" || s["budget_remaining"] != strconv.Itoa(forge.ReadReserve) {
			t.Errorf("a scan from %d remaining stopped = %v, want read_reserve at %d", start, s, forge.ReadReserve)
		}
	}
}

func TestScan_at_the_reserve_with_no_github_only_read_due_the_scan_completes(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.repos["o"] = []forge.Repo{{Path: "o/fork", DefaultBranch: "main", Fork: true}}
	// The open, discovery, both snapshots and the run listing leave it at the reserve.
	ep.conn.quota.remaining = forge.ReadReserve + 5
	h := newHarness(t, ep)
	h.scan(t)
	lineFor(t, h.rec, "scan complete", "gh")
	noLineFor(t, h.rec, "scan stopped", "gh")
	if ep.gh.calls != 0 {
		t.Errorf("GitHub-only requests for a skipped fork = %d, want 0", ep.gh.calls)
	}
}

func TestScan_pervasive_401_is_token_invalid_and_a_sparse_one_is_not(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.openErr = &forge.OpenError{Product: forge.ProductGitLab, Err: fmt.Errorf("identify account: %w", forge.ErrTokenInvalid)}
	h := newHarness(t, ep)
	if got := h.scan(t); got != Incomplete {
		t.Errorf("Scan with a connection that did not open = %s, want incomplete", got)
	}
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "token_invalid" || l["forge"] != "gitlab" {
		t.Errorf("scan degraded = %v, want token_invalid under the product the connection named", l)
	}
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["degraded"] != "true" || c["forge"] != "gitlab" {
		t.Errorf("scan complete = %v, want degraded under the product the connection named", c)
	}
	if l := lineFor(t, h.rec, "repo discovery failed", "gh"); l["forge"] != "gitlab" {
		t.Errorf("repo discovery failed forge = %q, want gitlab", l["forge"])
	}
	undetected := githubEndpoint("gh")
	undetected.openErr = fmt.Errorf("open: %w", forge.ErrTokenInvalid)
	h1 := newHarness(t, undetected)
	h1.scan(t)
	if c := lineFor(t, h1.rec, "scan complete", "gh"); c["forge"] != "unknown" {
		t.Errorf("scan complete of a connection that named no product: forge = %q, want unknown", c["forge"])
	}
	sparse := githubEndpoint("gh")
	sparse.conn.repos["o"] = append(sparse.conn.repos["o"], forge.Repo{Path: "o/b"})
	sparse.conn.errs = map[string]error{"runs o/b": forge.ErrTokenInvalid}
	h2 := newHarness(t, sparse)
	h2.scan(t)
	if n := h2.rec.CountExact("scan degraded"); n != 0 {
		t.Errorf("a sparse 401 beside successful reads escalated %d time(s): %v", n, lines(h2.rec, "scan degraded"))
	}
}

func TestScan_a_token_revoked_on_an_open_connection_is_token_invalid(t *testing.T) {
	ep := githubEndpoint("gh")
	h := newHarness(t, ep)
	h.scan(t)
	noLineFor(t, h.rec, "scan degraded", "gh")
	ep.conn.errs = map[string]error{"discover o": fmt.Errorf("list repositories: %w", forge.ErrTokenInvalid)}
	h.scan(t)
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "token_invalid" {
		t.Errorf("scan degraded cause = %q, want token_invalid: the reused connection read nothing this scan", l["cause"])
	}
	if l := lineFor(t, h.rec, "repo discovery failed", "gh"); l["cause"] != "token_invalid" {
		t.Errorf("repo discovery failed cause = %q, want token_invalid", l["cause"])
	}
}

func TestScan_a_discovery_401_right_after_a_fresh_open_is_connection_failed(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.errs = map[string]error{"discover o": fmt.Errorf("list repositories: %w", forge.ErrTokenInvalid)}
	h := newHarness(t, ep)
	if got := h.scan(t); got != Incomplete {
		t.Errorf("Scan with repository discovery rejected = %s, want incomplete", got)
	}
	if n := h.rec.CountExact("scan degraded"); n != 1 {
		t.Fatalf("scan degraded lines = %d, want 1: discovery read nothing", n)
	}
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "connection_failed" {
		t.Errorf("scan degraded cause = %q, want connection_failed: the fresh open proved the token, the listing still failed", l["cause"])
	}
	if l := lineFor(t, h.rec, "repo discovery failed", "gh"); l["cause"] != "token_invalid" {
		t.Errorf("repo discovery failed cause = %q, want token_invalid: the listing itself answered 401", l["cause"])
	}
}

func TestScan_github_only_reads_stop_at_the_read_reserve(t *testing.T) {
	ep := githubEndpoint("gh")
	// The open, discovery, both snapshots, the run listing and the alerts
	// read leave the workflows read at the reserve.
	ep.conn.quota.remaining = forge.ReadReserve + 6
	h := newHarness(t, ep)
	h.scan(t)
	if ep.gh.calls != 1 {
		t.Errorf("GitHub-only requests = %d, want 1", ep.gh.calls)
	}
	s := lineFor(t, h.rec, "scan stopped", "gh")
	if s["reason"] != "read_reserve" || s["phase"] != "workflows" || s["budget_remaining"] != strconv.Itoa(forge.ReadReserve) {
		t.Errorf("scan stopped = %v, want read_reserve in workflows with %d remaining", s, forge.ReadReserve)
	}
	noLineFor(t, h.rec, "scan complete", "gh")
}

func TestScan_a_shared_budget_at_the_reserve_holds_the_next_scan_until_it_renews(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.cfg.Security.Enabled = false
	ep.conn.quota.remaining = forge.ReadReserve + 6
	h := newHarness(t, ep)
	h.scan(t)
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["budget_remaining"] != strconv.Itoa(forge.ReadReserve) {
		t.Fatalf("first scan budget_remaining = %q, want %d: the workflows read spent the last request above the reserve", c["budget_remaining"], forge.ReadReserve)
	}
	ep.conn.calls = nil
	h.scan(t)
	if len(ep.conn.calls) != 0 || ep.gh.calls != 1 {
		t.Errorf("scan after the budget reached the reserve sent forgeapi reads %v and %d GitHub-only reads in total, want none beyond the first scan's 1", ep.conn.calls, ep.gh.calls)
	}
	s := lineFor(t, h.rec, "scan stopped", "gh")
	if s["reason"] != "read_reserve" || s["phase"] != "discovery" || s["budget_remaining"] != strconv.Itoa(forge.ReadReserve) {
		t.Errorf("scan stopped = %v, want read_reserve in discovery with %d remaining", s, forge.ReadReserve)
	}
	noLineFor(t, h.rec, "scan complete", "gh")
	noLineFor(t, h.rec, "scan degraded", "gh")
	*h.clock = now.Add(time.Hour + time.Minute)
	ep.conn.quota.remaining, ep.conn.quota.reset = 4000, now.Add(2*time.Hour)
	h.scan(t)
	if !slices.Contains(ep.conn.calls, "discover o") || ep.gh.calls != 2 {
		t.Errorf("scan after the window renewed sent forgeapi reads %v and %d GitHub-only reads in total, want discovery and 2", ep.conn.calls, ep.gh.calls)
	}
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["budget_remaining"] != "3995" {
		t.Errorf("scan complete after the window renewed budget_remaining = %q, want 3995", c["budget_remaining"])
	}
}

func TestScan_dead_connection_leaves_the_other_intact(t *testing.T) {
	dead, live := giteaEndpoint("dead"), githubEndpoint("live")
	dead.openErr = fmt.Errorf("%w: no family answered", forge.ErrConnection)
	live.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 1, Author: "x", HeadSHA: "h1"}}}
	h := newHarness(t, dead, live)
	h.scan(t)
	if l := lineFor(t, h.rec, "scan degraded", "dead"); l["cause"] != "connection_failed" {
		t.Errorf("dead cause = %q, want connection_failed", l["cause"])
	}
	if c := lineFor(t, h.rec, "scan complete", "dead"); c["degraded"] != "true" {
		t.Errorf("dead scan complete = %v, want degraded", c)
	}
	if c := lineFor(t, h.rec, "scan complete", "live"); c["degraded"] != "false" || c["open_prs"] != "1" {
		t.Errorf("live scan complete = %v, want clean with its pull request", c)
	}
	noLineFor(t, h.rec, "scan degraded", "live")
	dead.openErr = nil
	h.scan(t)
	noLineFor(t, h.rec, "scan degraded", "dead")
}

// TestScan_a_second_connection_on_one_account_fails_to_open opens b before
// a, so the connection refused is the one configured later, not the one
// that opened later. b's url names a's forge in another spelling.
func TestScan_a_second_connection_on_one_account_fails_to_open(t *testing.T) {
	tests := []struct{ name, urlB string }{
		{"host_case", "https://GitHub.test"},
		{"default_port", "https://github.test:443"},
		{"scheme_case_and_root_path", "HTTPS://github.test/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := githubEndpoint("a"), githubEndpoint("b")
			a.cfg.URL, b.cfg.URL = "https://github.test", tc.urlB
			b.conn.login = "Me"
			bOpened := make(chan struct{})
			b.beforeOpen = func() { close(bOpened) }
			a.beforeOpen = func() { <-bOpened }
			h := newHarness(t, a, b)
			h.scan(t)
			want := `connection "b" uses the same account as "a" on ` + tc.urlB + `; list both owners in one connection`
			if l := lineFor(t, h.rec, "repo discovery failed", "b"); l["cause"] != "connection_failed" || !strings.Contains(l["error"], want) {
				t.Errorf("b at %s: repo discovery failed = %v, want cause connection_failed and error %q", tc.urlB, l, want)
			}
			if l := lineFor(t, h.rec, "scan degraded", "b"); l["cause"] != "connection_failed" {
				t.Errorf("b at %s: scan degraded cause = %q, want connection_failed", tc.urlB, l["cause"])
			}
			if c := lineFor(t, h.rec, "scan complete", "b"); c["repos_read"] != "blind" || c["forge"] != "github" {
				t.Errorf("b at %s: scan complete = %v, want its repositories blind, as on any failed open, under forge github", tc.urlB, c)
			}
			if len(b.conn.calls) != 0 || b.gh.calls != 0 || !b.conn.closed {
				t.Errorf("b at %s: reads %v and %d GitHub-only, closed %t, want none made and the connection closed",
					tc.urlB, b.conn.calls, b.gh.calls, b.conn.closed)
			}
			noLineFor(t, h.rec, "scan degraded", "a")
			if c := lineFor(t, h.rec, "scan complete", "a"); c["degraded"] != "false" {
				t.Errorf("a scan complete = %v, want a clean scan", c)
			}
		})
	}
}

func TestScan_connections_on_other_accounts_or_forges_all_read(t *testing.T) {
	tests := []struct {
		name         string
		urlB, loginB string
	}{
		{"same_forge_other_account", "https://github.test", "you"},
		{"other_forge_same_login", "https://git.example.test", "me"},
		{"other_port_same_login", "https://github.test:8443", "me"},
		{"other_path_same_login", "https://github.test/forge", "me"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := githubEndpoint("a"), githubEndpoint("b")
			a.cfg.URL, b.cfg.URL, b.conn.login = "https://github.test", tc.urlB, tc.loginB
			h := newHarness(t, a, b)
			h.scan(t)
			for _, name := range []string{"a", "b"} {
				noLineFor(t, h.rec, "scan degraded", name)
			}
		})
	}
}

func TestScan_discovery_failure_is_connection_failed(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.errs = map[string]error{"discover o": errors.New("upstream 502")}
	h := newHarness(t, ep)
	if got := h.scan(t); got != Incomplete {
		t.Errorf("Scan after a failed discovery = %s, want incomplete", got)
	}
	if l := lineFor(t, h.rec, "repo discovery failed", "gh"); l["level"] != "ERROR" || l["cause"] != "connection_failed" {
		t.Errorf("repo discovery failed = %v, want ERROR with cause connection_failed", l)
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	for _, k := range []string{"open_prs", "repos_discovered"} {
		if _, ok := c[k]; ok {
			t.Errorf("scan complete after a failed discovery carries %s, want no unread count", k)
		}
	}
}

func TestScan_budget_fields_omitted_when_unknown(t *testing.T) {
	gt := giteaEndpoint("gt")
	h := newHarness(t, gt)
	h.scan(t)
	c := lineFor(t, h.rec, "scan complete", "gt")
	if _, ok := c["budget_remaining"]; ok {
		t.Error("scan complete carries budget_remaining where the instance reports none")
	}
	if _, ok := c["budget_reset"]; ok {
		t.Error("scan complete carries budget_reset where the instance reports none")
	}
	gh := githubEndpoint("gh")
	h2 := newHarness(t, gh)
	h2.scan(t)
	if c := lineFor(t, h2.rec, "scan complete", "gh"); c["budget_remaining"] != "3993" || c["budget_reset"] != "2026-10-07T13:00:00Z" {
		t.Errorf("scan complete budget = %s %s, want 3993 (4000 less the seven requests the open and the scan sent, through both clients) and 13:00",
			c["budget_remaining"], c["budget_reset"])
	}
}

func TestScan_snapshots_re_emit_every_scan(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.issues = map[string][]forge.Issue{"o": {{Repo: "o/r", Number: 1, Author: "x"}}}
	h := newHarness(t, ep)
	h.scan(t)
	h.scan(t)
	if n := h.rec.CountExact("open issue"); n != 1 {
		t.Errorf("second scan open issue lines = %d, want 1: a snapshot re-emits", n)
	}
}

func TestScan_security_settings_on_another_product_warn(t *testing.T) {
	ep := giteaEndpoint("gt")
	ep.cfg.SecuritySet = true
	h := newHarness(t, ep)
	h.scan(t)
	if l := lineFor(t, h.rec, "security settings ignored: code scanning is GitHub-only", "gt"); l["level"] != "WARN" {
		t.Errorf("security settings line = %v, want WARN", l)
	}
}

// blockStateWrites makes every run state write in dir fail, whoever runs the
// test: the rename onto a non-empty directory fails.
func blockStateWrites(t *testing.T, dir string) (unblock func()) {
	t.Helper()
	path := filepath.Join(dir, "state.json")
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o700); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	return func() {
		if err := os.RemoveAll(path); err != nil {
			t.Fatalf("Setup: %v", err)
		}
	}
}

func TestScan_a_failed_state_save_degrades_the_scan_and_logs_its_runs_once(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	h := newHarness(t, ep)
	unblock := blockStateWrites(t, h.dir)
	h.scan(t)
	if l := lineFor(t, h.rec, "ci run", "gh"); l["run_id"] != "1" || l["default_branch"] != "true" {
		t.Errorf("ci run of the scan whose save failed = %v, want run 1 on the default branch: it is logged before the write", l)
	}
	if l := lineFor(t, h.rec, "run state save failed", "gh"); l["level"] != "ERROR" || l["error"] == "" {
		t.Errorf("save failure line = %v, want ERROR with the error", l)
	}
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "state_unwritable" || l["failed_signals"] != "run_state" ||
		l["reason"] != "The run state could not be saved. This scan's CI run lines were logged, and a restart before a save succeeds logs them again." {
		t.Errorf("scan degraded = %v, want cause state_unwritable, failed_signals run_state and a reason saying the run lines were logged", l)
	}
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["degraded"] != "true" || c["failed_signals"] != "run_state" || c["new_failures"] != "1" {
		t.Errorf("scan complete = %v, want degraded with run_state failed and the logged failure counted", c)
	}
	unblock()
	h.scan(t)
	if n := h.rec.CountExact("ci run"); n != 0 {
		t.Errorf("scan after the save recovered logged %d ci run line(s), want 0: run 1 was logged", n)
	}
	noLineFor(t, h.rec, "scan degraded", "gh")
	h.restart(t)
	h.scan(t)
	if n := h.rec.CountExact("ci run"); n != 0 {
		t.Errorf("scan after a restart logged %d ci run line(s), want 0: the recovered save stored run 1", n)
	}
}

// crashBeforeWrite makes conn's save stop between logging its run changes
// and its write, once each of after has saved, as a process dying there
// does: the file never holds what conn logged.
func crashBeforeWrite(t *testing.T, conn string, after ...string) {
	t.Helper()
	var mu sync.Mutex
	saved := map[string]bool{}
	peersSaved := make(chan struct{})
	commitRuns = func(s *runstate.Store, ctx context.Context, name string, r *runstate.Read, deliver func([]runstate.Change) bool) error {
		if name != conn {
			err := s.Commit(ctx, name, r, deliver)
			mu.Lock()
			if slices.Contains(after, name) && !saved[name] {
				saved[name] = true
				if len(saved) == len(after) {
					close(peersSaved)
				}
			}
			mu.Unlock()
			return err
		}
		<-peersSaved
		dead, kill := context.WithCancel(ctx)
		kill()
		return s.Commit(dead, name, r, deliver)
	}
	t.Cleanup(func() { commitRuns = (*runstate.Store).Commit })
}

func TestScan_a_crash_between_a_connections_run_lines_and_its_write_repeats_them_after_a_restart(t *testing.T) {
	gh := githubEndpoint("gh")
	gh.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	peer := giteaEndpoint("gt")
	peer.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(2, forge.RunFailing, now.Add(-time.Hour))}}}
	h := newHarness(t, gh, peer)
	crashBeforeWrite(t, "gt", "gh")
	h.scan(t)
	for _, conn := range []string{"gh", "gt"} {
		if l := lineFor(t, h.rec, "ci run", conn); l["run_id"] == "" {
			t.Errorf("ci run for %s before the crash = %v, want its run logged", conn, l)
		}
	}
	commitRuns = (*runstate.Store).Commit
	h.restart(t)
	h.scan(t)
	noLineFor(t, h.rec, "ci run", "gh")
	if l := lineFor(t, h.rec, "ci run", "gt"); l["run_id"] != "2" {
		t.Errorf("gt's ci run after the restart = %v, want run 2 logged again: the crash came before its write", l)
	}
	h.scan(t)
	if n := h.rec.CountExact("ci run"); n != 0 {
		t.Errorf("next scan logged %d ci run line(s), want 0: both runs are stored now", n)
	}
}

// TestScan_a_connection_logs_its_runs_before_a_peer_finishes holds the peer's
// open until gh's ci run line is logged: a process dying while a slow peer
// still reads must not take with it a run gh already stored.
func TestScan_a_connection_logs_its_runs_before_a_peer_finishes(t *testing.T) {
	gh := githubEndpoint("gh")
	gh.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	peer := giteaEndpoint("gt")
	h := newHarness(t, gh, peer)
	logged := false
	peer.beforeOpen = func() {
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if len(lines(h.rec, "ci run")) > 0 {
				logged = true
				return
			}
		}
	}
	h.scan(t)
	if !logged {
		t.Errorf("gh's ci run line for run 1 was not logged while its peer was still opening, want it logged once gh's state is saved: messages %v", h.rec.Messages())
	}
}

func TestScan_a_failed_state_save_escalates_beside_a_budget_stop(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.errs = map[string]error{"runs o/r": forge.ErrReadDeferred}
	h := newHarness(t, ep)
	blockStateWrites(t, h.dir)
	h.scan(t)
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "state_unwritable" {
		t.Errorf("scan degraded = %v, want state_unwritable beside the read-reserve stop", l)
	}
}

// commitNotDurable makes every run state write land on disk without being
// made durable, as on a file system that refuses a directory sync.
func commitNotDurable(t *testing.T) {
	t.Helper()
	commitRuns = func(s *runstate.Store, ctx context.Context, conn string, r *runstate.Read, deliver func([]runstate.Change) bool) error {
		if err := s.Commit(ctx, conn, r, deliver); err != nil {
			return err
		}
		return runstate.ErrNotDurable
	}
	t.Cleanup(func() { commitRuns = (*runstate.Store).Commit })
}

func TestScan_a_save_that_is_never_durable_logs_each_run_once_and_degrades_the_scan(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	h := newHarness(t, ep)
	commitNotDurable(t)
	h.scan(t)
	if l := lineFor(t, h.rec, "ci run", "gh"); l["run_id"] != "1" {
		t.Errorf("ci run = %v, want run 1: the file holds it", l)
	}
	if l := lineFor(t, h.rec, "run state not durable", "gh"); l["level"] != "ERROR" || l["error"] == "" || l["path"] == "" {
		t.Errorf("not durable line = %v, want ERROR with the path and the error", l)
	}
	noLineFor(t, h.rec, "run state save failed", "gh")
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "state_not_durable" || l["failed_signals"] != "run_state" {
		t.Errorf("scan degraded = %v, want cause state_not_durable and failed_signals run_state", l)
	}
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["degraded"] != "true" || c["new_failures"] != "1" {
		t.Errorf("scan complete = %v, want degraded with the failure counted", c)
	}
	ep.conn.runs["o/r"] = listing{Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour)), run(2, forge.RunFailing, now.Add(-time.Hour))}}
	h.scan(t)
	if got := lines(h.rec, "ci run"); len(got) != 1 || got[0]["run_id"] != "2" {
		t.Errorf("second scan ci run lines = %v, want only run 2: run 1 was logged once", got)
	}
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "state_not_durable" {
		t.Errorf("second scan degraded = %v, want state_not_durable", l)
	}
}

func TestScan_a_save_that_is_not_durable_escalates_beside_a_budget_stop(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.errs = map[string]error{"runs o/r": forge.ErrReadDeferred}
	h := newHarness(t, ep)
	commitNotDurable(t)
	h.scan(t)
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "state_not_durable" {
		t.Errorf("scan degraded = %v, want state_not_durable beside the read-reserve stop", l)
	}
}

func TestScan_an_unresolved_owner_keeps_its_stored_runs_and_verdicts(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.cfg.Owners = []string{"o", "p"}
	ep.conn.repos["p"] = []forge.Repo{{Path: "p/r", DefaultBranch: "main"}}
	pRun := run(2, forge.RunFailing, now.Add(-time.Hour))
	pRun.Repo = "p/r"
	ep.conn.runs = map[string]listing{
		"o/r": {Runs: []forge.Run{run(1, forge.RunPassing, now.Add(-time.Hour))}},
		"p/r": {Runs: []forge.Run{pRun}},
	}
	h := newHarness(t, ep)
	h.scan(t)
	if l := lineFor(t, h.rec, "failing workflow", "gh"); l["repo"] != "p/r" {
		t.Fatalf("Setup: failing workflow = %v, want p/r", l)
	}
	ep.conn.unresolved = []string{"p"}
	// Past the lookback, p/r's verdict lives only in the store's fold, which
	// a forgotten repository loses.
	*h.clock = now.Add(7 * 24 * time.Hour)
	ep.conn.runs = map[string]listing{"o/r": {}, "p/r": {}}
	h.scan(t)
	ep.conn.unresolved = nil
	*h.clock = h.clock.Add(time.Hour)
	h.scan(t)
	if l := lineFor(t, h.rec, "failing workflow", "gh"); l["repo"] != "p/r" || l["consecutive_failures"] != "1" {
		t.Errorf("failing workflow after p resolved again = %v, want p/r's stored verdict", l)
	}
	if n := h.rec.CountExact("ci run"); n != 0 {
		t.Errorf("scan after p resolved again logged %d ci run line(s), want 0: its unchanged run is still stored", n)
	}
}

func TestNew_corrupt_state_is_set_aside_with_a_warning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := writeFile(path, "{garbage"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	logger, rec := capture.New()
	New(t.Context(), &Deps{Store: openStore(t, dir), Logger: logger})
	l := lineFor(t, rec, "run state unreadable; starting cold", "")
	if l["level"] != "WARN" || l["path"] != path || l["error"] == "" || !strings.HasPrefix(l["set_aside"], path+".corrupt-") {
		t.Errorf("cold-start warning = %v, want WARN with the path, the reason and where the file was set aside", l)
	}
	if body, err := os.ReadFile(l["set_aside"]); err != nil || string(body) != "{garbage" {
		t.Errorf("set-aside file = %q (%v), want the original bytes", body, err)
	}
}

func TestClose_closes_open_connections(t *testing.T) {
	ep := githubEndpoint("gh")
	h := newHarness(t, ep)
	h.scan(t)
	h.c.Close()
	if !ep.conn.closed {
		t.Error("Close left the connection open")
	}
}

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }

func TestScan_open_deferred_by_the_reserve_is_a_stop_and_reopens(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.openErr = fmt.Errorf("identify account: %w", forge.ErrReadDeferred)
	h := newHarness(t, ep)
	h.scan(t)
	if s := lineFor(t, h.rec, "scan stopped", "gh"); s["phase"] != "open" || s["reason"] != "read_reserve" {
		t.Errorf("scan stopped = %v, want read_reserve at open", s)
	} else if _, ok := s["budget_remaining"]; ok {
		t.Errorf("scan stopped at open carries budget_remaining %s, want it absent: no budget was reported", s["budget_remaining"])
	}
	noLineFor(t, h.rec, "scan complete", "gh")
	noLineFor(t, h.rec, "scan degraded", "gh")
	h.scan(t)
	if s := lineFor(t, h.rec, "scan stopped", "gh"); s["level"] != "WARN" {
		t.Errorf("a second open stop with no budget window known logged at %s, want WARN", s["level"])
	}
	ep.openErr = nil
	h.scan(t)
	lineFor(t, h.rec, "scan complete", "gh")
}

func TestScan_a_blind_github_only_family_logs_no_count(t *testing.T) {
	tests := []struct {
		setup  func(ep *endpoint)
		name   string
		absent []string
		kept   []string
	}{
		{
			name:  "security",
			setup: func(ep *endpoint) { ep.gh.errs = map[string]error{"alerts o/r": forge.ErrForbidden} },
			absent: []string{
				"security_alerts", "security_alerts_listed", "security_alerts_critical", "security_alerts_high",
				"security_alerts_medium", "security_alerts_low", "security_alerts_unrated",
			},
			kept: []string{"security_alerts_read", "security_unreadable", "security_repos_read"},
		},
		{
			name:   "workflows",
			setup:  func(ep *endpoint) { ep.gh.errs = map[string]error{"workflows o/r": forge.ErrForbidden} },
			absent: []string{"disabled_workflows"},
			kept:   []string{"disabled_workflows_read", "workflows_failed_repos"},
		},
		{
			name: "pr_checks",
			setup: func(ep *endpoint) {
				ep.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 1, Author: "x", HeadSHA: "h1"}}}
				ep.conn.errs = map[string]error{"checks h1": errors.New("upstream 502")}
			},
			absent: []string{"failing_checks_prs"},
			kept:   []string{"checks_unread", "open_prs"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := githubEndpoint("gh")
			tc.setup(ep)
			h := newHarness(t, ep)
			h.scan(t)
			c := lineFor(t, h.rec, "scan complete", "gh")
			for _, k := range tc.absent {
				if v, ok := c[k]; ok {
					t.Errorf("scan complete with every %s read failed carries %s = %s, want it absent, never 0", tc.name, k, v)
				}
			}
			for _, k := range tc.kept {
				if _, ok := c[k]; !ok {
					t.Errorf("scan complete with every %s read failed lacks %s, want the coverage kept", tc.name, k)
				}
			}
		})
	}
}

func TestScan_security_lines_rank_across_connections(t *testing.T) {
	a, b := githubEndpoint("a"), githubEndpoint("b")
	a.gh.alerts = map[string][]forge.Alert{"o/r": {
		{Repo: "o/r", Number: 1, Severity: "low", Tool: "Trivy"}, {Repo: "o/r", Number: 2, Severity: "low", Tool: "Trivy"},
	}}
	b.gh.alerts = map[string][]forge.Alert{"o/r": {{Repo: "o/r", Number: 3, Severity: "critical", Tool: "CodeQL"}}}
	h := newHarness(t, a, b)
	h.scan(t)
	for _, msg := range []string{"security alert", "top security alert", "security tool"} {
		got := lines(h.rec, msg)
		ranks := map[string]bool{}
		for _, l := range got {
			if l["scan_id"] == "" || l["rank"] == "" || l["forge_rank"] == "" {
				t.Errorf("%s line = %v, want scan_id, rank and forge_rank", msg, l)
			}
			ranks[l["rank"]] = true
		}
		for i := 1; i <= len(got); i++ {
			if !ranks[strconv.Itoa(i)] {
				t.Errorf("%s ranks = %v, want 1..%d dense across both connections", msg, ranks, len(got))
			}
		}
	}
	if l := lines(h.rec, "security alert"); len(l) != 3 || l[0]["connection"] != "b" || l[0]["rank"] != "1" {
		t.Errorf("security alert lines = %v, want b's critical alert first of 3", l)
	}
	if l := lines(h.rec, "security tool"); len(l) != 2 || l[0]["tool"] != "Trivy" || l[0]["rank"] != "1" || l[1]["rank"] != "2" {
		t.Errorf("security tool lines = %v, want a's Trivy (2 alerts) at rank 1 and b's CodeQL at rank 2", l)
	}
}

func TestScan_a_rate_limited_open_is_a_stop(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.openErr = fmt.Errorf("identify account: %w", forge.ErrRateLimited)
	h := newHarness(t, ep)
	h.scan(t)
	if s := lineFor(t, h.rec, "scan stopped", "gh"); s["reason"] != "rate_limited" || s["phase"] != "open" {
		t.Errorf("scan stopped = %v, want rate_limited at open", s)
	}
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "rate_limited" {
		t.Errorf("scan degraded cause = %q, want rate_limited", l["cause"])
	}
	noLineFor(t, h.rec, "repo discovery failed", "gh")
	noLineFor(t, h.rec, "scan complete", "gh")
}
