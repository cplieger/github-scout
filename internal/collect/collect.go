// Package collect is github-scout's scan orchestrator. One Collector
// instance lives for the process lifetime; each Scan gathers the four
// actionable GitHub signals across all of an owner's repos and emits them
// as structured log lines for Alloy to ship to Loki.
//
// Two emission models (see internal/model):
//
//   - Event-once (Actions runs): each completed run ID is emitted a single
//     time as msg="workflow run" with its conclusion, so a plain LogQL
//     count equals the number of distinct runs and the dashboard filters
//     that stream for failures and computes the failure rate. The dedup set
//     is in-memory, pruned to the lookback window.
//   - Snapshot (open PRs, open issues, code-scanning alerts): the full
//     current set is emitted every scan. A closed/merged/fixed item simply
//     stops appearing in later snapshots, so the dashboard reads the most
//     recent scan as "what is open right now" — no dedup state needed.
//
// The only cross-scan state is the run dedup set. In the long-lived
// scheduled/resident process it lives in memory across scans. Under an
// external scheduler (Ofelia job-exec) each `trigger` is a fresh process,
// so the set is persisted to a small JSON file (Deps.StatePath, e.g.
// /tmp/seen-runs.json, which is shared across `docker exec` triggers of the
// same running container) and reloaded at the start of the next trigger.
// Either way a cold start — the first run, or a container recreate that
// clears /tmp — at worst re-emits runs still inside the lookback window
// (the dashboard dedups run counts by run_id), so persistence is a
// best-effort optimization, never a correctness dependency.
package collect

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"time"

	"github.com/cplieger/atomicfile"
	"github.com/cplieger/github-scout/internal/model"
)

// maxStateBytes caps the dedup state file read. The set is bounded to the
// lookback window (~hundreds–thousands of run IDs, tens of KB of JSON), so
// 8 MiB is an ample guard against a corrupt/oversized file.
const maxStateBytes = 8 << 20

// Collector holds the cross-scan state: the GitHub client, scan
// parameters, and the in-memory failed-run dedup set. Construct via New.
type Collector struct {
	client       apiClient
	logger       *slog.Logger
	now          func() time.Time
	seen         map[int64]time.Time // run RunID -> CreatedAt, pruned to lookback
	exclude      map[string]bool     // bare repo names to skip across all signals
	owner        string
	prExclude    string // raw search qualifiers to filter PR noise (e.g. Renovate)
	issueExclude string // raw search qualifiers to filter issue noise (Renovate, auto-generated)
	statePath    string // optional path to persist `seen` across trigger processes ("" = in-memory only)
	lookback     time.Duration
}

// apiClient is the consumer-side view of the GitHub client the collector
// needs. *github.Client satisfies it in production; tests pass a fake.
type apiClient interface {
	ListRepos(ctx context.Context, owner string) ([]model.Repo, error)
	ListRuns(ctx context.Context, repo model.Repo, since time.Time) ([]model.WorkflowRun, error)
	SearchOpenPRs(ctx context.Context, owner, exclude string) ([]model.PullRequest, error)
	SearchOpenIssues(ctx context.Context, owner, exclude string) ([]model.Issue, error)
	ListCodeScanningAlerts(ctx context.Context, repo model.Repo) ([]model.CodeScanningAlert, error)
}

// Deps are the constructor arguments for New. A nil Logger falls back to
// slog.Default; a nil Now falls back to time.Now.
type Deps struct {
	Client       apiClient
	Logger       *slog.Logger
	Now          func() time.Time
	Exclude      map[string]bool
	Owner        string
	PRExclude    string
	IssueExclude string
	// StatePath, when non-empty, is where the run dedup set is persisted
	// (atomic JSON) at the end of each scan and reloaded by New. Set it for
	// trigger-mode deployments (each trigger is a fresh process) to a path
	// shared across execs of the same container, e.g. /tmp/seen-runs.json.
	// Leave empty for the long-lived scheduled/resident process (in-memory
	// dedup) and in tests.
	StatePath string
	Lookback  time.Duration
}

// New constructs a Collector. When Deps.StatePath is set, the persisted run
// dedup set is loaded from it (a missing or corrupt file simply starts the
// set empty); otherwise the set starts empty. Takes *Deps to avoid copying
// the (large) struct.
func New(d *Deps) *Collector {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	c := &Collector{
		client:       d.Client,
		logger:       logger,
		now:          now,
		seen:         make(map[int64]time.Time),
		exclude:      d.Exclude,
		owner:        d.Owner,
		prExclude:    d.PRExclude,
		issueExclude: d.IssueExclude,
		statePath:    d.StatePath,
		lookback:     d.Lookback,
	}
	if c.statePath != "" {
		c.loadState()
	}
	return c
}

// Scan runs one full cycle and returns whether it was healthy. Health
// contract: true if repo discovery succeeded (the per-signal collectors
// tolerate partial failure, logging a warning and continuing); false only
// if discovery itself failed (bad token, rate limit), since without the
// repo list there is nothing to scan.
func (c *Collector) Scan(ctx context.Context) (healthy bool) {
	start := c.now()
	cutoff := start.Add(-c.lookback)

	repos, err := c.client.ListRepos(ctx, c.owner)
	if err != nil {
		c.logger.Error("repo discovery failed", "owner", c.owner, "error", err)
		return false
	}
	c.logger.Info("scanning", "owner", c.owner, "repos", len(repos), "since", cutoff.Format(time.RFC3339))

	openPRs := c.collectPRs(ctx)
	openIssues := c.collectIssues(ctx)

	scanned, skipped, newRuns, newFailures, alerts := 0, 0, 0, 0, 0
	for i := range repos {
		repo := &repos[i]
		if c.exclude[repo.Name] {
			skipped++
			continue
		}
		scanned++
		runs, failures := c.collectRuns(ctx, repo, cutoff)
		newRuns += runs
		newFailures += failures
		alerts += c.collectAlerts(ctx, repo)
	}

	c.prune(cutoff)
	if c.statePath != "" {
		c.saveState()
	}
	c.logger.Info("scan complete",
		"scanned", scanned, "skipped", skipped,
		"open_prs", openPRs, "open_issues", openIssues,
		"code_alerts", alerts, "new_runs", newRuns, "new_failures", newFailures,
		"tracked", len(c.seen),
		"duration", c.now().Sub(start).Round(time.Millisecond))
	return true
}

// collectPRs emits the current open-PR snapshot (cross-repo, one query) and
// returns the count surfaced. Excluded repos are filtered client-side.
func (c *Collector) collectPRs(ctx context.Context) int {
	prs, err := c.client.SearchOpenPRs(ctx, c.owner, c.prExclude)
	if err != nil {
		c.logger.Warn("open PR search failed", "error", err)
		return 0
	}
	n := 0
	for i := range prs {
		pr := &prs[i]
		if c.excludedRepo(pr.Repo) {
			continue
		}
		c.logger.Info("open pull request",
			"repo", pr.Repo, "number", pr.Number, "title", pr.Title,
			"author", pr.Author, "draft", pr.Draft, "url", pr.URL,
			"created_at", pr.CreatedAt.UTC().Format(time.RFC3339))
		n++
	}
	return n
}

// collectIssues emits the current open-issue snapshot (cross-repo, one
// query) and returns the count surfaced.
func (c *Collector) collectIssues(ctx context.Context) int {
	issues, err := c.client.SearchOpenIssues(ctx, c.owner, c.issueExclude)
	if err != nil {
		c.logger.Warn("open issue search failed", "error", err)
		return 0
	}
	n := 0
	for i := range issues {
		is := &issues[i]
		if c.excludedRepo(is.Repo) {
			continue
		}
		c.logger.Info("open issue",
			"repo", is.Repo, "number", is.Number, "title", is.Title,
			"author", is.Author, "labels", is.Labels, "url", is.URL,
			"created_at", is.CreatedAt.UTC().Format(time.RFC3339))
		n++
	}
	return n
}

// collectRuns emits newly-seen completed runs for repo (event-once) and
// returns how many runs were new this scan and how many of those were
// failures. Every completed run is emitted once as msg="workflow run" with
// its conclusion, so the dashboard filters that one stream for the failures
// view and computes the failure rate — no per-conclusion fan-out needed.
func (c *Collector) collectRuns(ctx context.Context, repo *model.Repo, cutoff time.Time) (newRuns, newFailures int) {
	runs, err := c.client.ListRuns(ctx, *repo, cutoff)
	if err != nil {
		c.logger.Warn("partial failure listing runs", "repo", repo.FullName(), "error", err)
	}
	for i := range runs {
		run := &runs[i]
		if _, ok := c.seen[run.RunID]; ok {
			continue
		}
		c.seen[run.RunID] = run.CreatedAt
		c.logger.Info("workflow run",
			"repo", run.Repo, "workflow", run.Workflow, "conclusion", run.Conclusion,
			"branch", run.Branch, "event", run.Event, "run_number", run.RunNumber,
			"run_id", run.RunID, "url", run.URL,
			"created_at", run.CreatedAt.UTC().Format(time.RFC3339))
		newRuns++
		if model.IsFailureConclusion(run.Conclusion) {
			newFailures++
		}
	}
	return newRuns, newFailures
}

// collectAlerts emits the current code-scanning-alert snapshot for repo and
// returns the count. Repos without code scanning return no alerts (the
// client maps 403/404 to empty), so this is silent for them.
func (c *Collector) collectAlerts(ctx context.Context, repo *model.Repo) int {
	alerts, err := c.client.ListCodeScanningAlerts(ctx, *repo)
	if err != nil {
		c.logger.Warn("code scanning listing failed", "repo", repo.FullName(), "error", err)
		return 0
	}
	for i := range alerts {
		a := &alerts[i]
		c.logger.Info("code scanning alert",
			"repo", a.Repo, "number", a.Number, "rule", a.Rule,
			"severity", a.Severity, "tool", a.Tool, "url", a.URL,
			"created_at", a.CreatedAt.UTC().Format(time.RFC3339))
	}
	return len(alerts)
}

// excludedRepo reports whether a "owner/name" full name is in the exclude
// set (which is keyed by bare repo name).
func (c *Collector) excludedRepo(fullName string) bool {
	if c.exclude == nil {
		return false
	}
	name := fullName
	if i := lastSlash(fullName); i != -1 {
		name = fullName[i+1:]
	}
	return c.exclude[name]
}

// lastSlash returns the index of the last '/' in s, or -1.
func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

// prune drops run dedup entries older than cutoff. The Actions query
// already filters to runs at or after cutoff, so older entries can never
// recur and are safe to forget — bounding the map to the lookback window.
func (c *Collector) prune(cutoff time.Time) {
	for id, created := range c.seen {
		if created.Before(cutoff) {
			delete(c.seen, id)
		}
	}
}

// loadState reads the persisted dedup set from statePath into c.seen. A
// missing file is the normal cold start; an unreadable or corrupt file is
// tolerated with a warning, since the only cost of a cold set is re-emitting
// runs still inside the lookback window. Called by New only when statePath
// is set.
func (c *Collector) loadState() {
	data, err := atomicfile.ReadBounded(context.Background(), c.statePath, maxStateBytes)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.logger.Warn("dedup state unreadable; starting cold", "path", c.statePath, "error", err)
		}
		return
	}
	var seen map[int64]time.Time
	if err := json.Unmarshal(data, &seen); err != nil {
		c.logger.Warn("dedup state corrupt; starting cold", "path", c.statePath, "error", err)
		return
	}
	if seen != nil {
		c.seen = seen
	}
}

// saveState atomically persists c.seen to statePath. Best-effort: a marshal
// or write failure is logged but never fails the scan (the next trigger just
// re-emits a few runs). Called at the end of Scan only when statePath is set,
// after prune has bounded the set to the lookback window.
func (c *Collector) saveState() {
	data, err := json.Marshal(c.seen)
	if err != nil {
		c.logger.Warn("dedup state marshal failed", "error", err)
		return
	}
	if err := atomicfile.WriteFile(context.Background(), c.statePath, data); err != nil {
		c.logger.Warn("dedup state save failed", "path", c.statePath, "error", err)
	}
}
