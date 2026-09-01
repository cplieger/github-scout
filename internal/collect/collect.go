// Package collect scans GitHub signals and emits structured log events.
package collect

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/cplieger/github-scout/internal/ghsignal"
	"github.com/cplieger/scheduler/v4"
)

// Collector holds the cross-scan state: the GitHub client, scan
// parameters, and the in-memory failed-run dedup set. Construct via New.
type Collector struct {
	client       apiClient
	logger       *slog.Logger
	now          func() time.Time
	seen         map[int64]time.Time
	exclude      map[string]bool
	csExclude    map[string]bool
	slot         *scheduler.SlotFile
	owner        string
	prExclude    string
	issueExclude string
	statePath    string
	lookback     time.Duration
	csSkipForks  bool
}

// apiClient is the consumer-side view of the GitHub client the collector
// needs. *github.Client satisfies it in production; tests pass a fake.
type apiClient interface {
	ListRepos(ctx context.Context, owner string) ([]ghsignal.Repo, error)
	ListRuns(ctx context.Context, repo ghsignal.Repo, since time.Time) ([]ghsignal.WorkflowRun, error)
	SearchOpenPRs(ctx context.Context, owner, exclude string) ([]ghsignal.PullRequest, error)
	SearchOpenIssues(ctx context.Context, owner, exclude string) ([]ghsignal.Issue, error)
	ListCodeScanningAlerts(ctx context.Context, repo ghsignal.Repo) ([]ghsignal.CodeScanningAlert, error)
}

// Deps are the constructor arguments for New. A nil Logger falls back to
// slog.Default; a nil Now falls back to time.Now.
type Deps struct {
	Client              apiClient
	Logger              *slog.Logger
	Now                 func() time.Time
	Exclude             map[string]bool
	CodeScanningExclude map[string]bool
	Owner               string
	PRExclude           string
	IssueExclude        string
	// StatePath, when non-empty, is where the run dedup set is persisted
	// (JSON in a flock'd scheduler.SlotFile, merged with any concurrent
	// writer's entries) at the end of each scan and reloaded by New.
	// Production (main.go) sets it wherever a scan runs -- the scheduled
	// daemon and every one-shot `trigger` process -- so the dedup set
	// survives a plain restart and each completed run is emitted once
	// regardless of process lifetime.
	// Leave empty only for in-memory-only dedup (used in tests).
	StatePath string
	Lookback  time.Duration
	// CodeScanningExcludeForks skips the code-scanning signal on every fork.
	// It is the batch form of CodeScanningExclude for the one repo property
	// that always warrants the skip: GitHub reports a fork's INHERITED alerts
	// as its own, so they are upstream's findings, not this owner's. The skip
	// is identical to the per-name one, so the fork keeps every other signal.
	CodeScanningExcludeForks bool
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
		csExclude:    d.CodeScanningExclude,
		csSkipForks:  d.CodeScanningExcludeForks,
		owner:        d.Owner,
		prExclude:    d.PRExclude,
		issueExclude: d.IssueExclude,
		statePath:    d.StatePath,
		lookback:     d.Lookback,
	}
	if c.statePath != "" {
		c.slot = scheduler.NewSlotFile(c.statePath)
		c.loadState()
	}
	return c
}

// Scan returns false only when discovery fails; unread signals are reported through scanIntegrity.
func (c *Collector) Scan(ctx context.Context) (healthy bool) {
	start := c.now()
	cutoff := start.Add(-c.lookback).Truncate(time.Second)

	repos, err := c.client.ListRepos(ctx, c.owner)
	if err != nil {
		if isShutdown(err) {
			c.logger.Debug("scan interrupted", "phase", "repo discovery")
			return true
		}
		c.logger.Error("repo discovery failed", "owner", c.owner, "error", err)
		return false
	}
	c.logger.Info("scanning", "owner", c.owner, "repos", len(repos), "since", cutoff.UTC().Format(time.RFC3339))

	var ig scanIntegrity
	ig.recordDiscovery(len(repos))
	openPRs, prErr := c.collectPRs(ctx)
	ig.recordPRs(prErr)
	openIssues, issErr := c.collectIssues(ctx)
	ig.recordIssues(issErr)

	scanned, skipped, newRuns, newFailures, alerts := 0, 0, 0, 0, 0
	for i := range repos {
		if ctx.Err() != nil {
			break
		}
		repo := &repos[i]
		if c.excludedName(repo.Name) {
			c.logger.Debug("skipping excluded repo", "repo", repo.FullName())
			skipped++
			continue
		}
		scanned++
		runs, failures, runErr := c.collectRuns(ctx, repo, cutoff)
		newRuns += runs
		newFailures += failures
		ig.recordRuns(runErr)
		if c.codeScanningExcluded(repo) {
			c.logger.Debug("skipping code scanning for excluded repo",
				"repo", repo.FullName(), "fork", repo.Fork)
		} else {
			a, alertErr := c.collectAlerts(ctx, repo)
			alerts += a
			ig.recordAlerts(alertErr)
		}
	}

	c.prune(cutoff)
	if c.statePath != "" {
		c.saveState()
	}

	ig.emit(c.logger)
	c.logger.Info("scan complete",
		"scanned", scanned, "skipped", skipped,
		"open_prs", openPRs, "open_issues", openIssues,
		"code_alerts", alerts, "new_runs", newRuns, "new_failures", newFailures,
		"tracked", len(c.seen),
		"errors", ig.errCount(), "degraded", ig.degraded(), "failed_signals", ig.failedSignals(),
		"duration", c.now().Sub(start).Round(time.Millisecond))
	return true
}

// collectPRs emits the current open-PR snapshot (cross-repo, one query) and
// returns the count surfaced plus any error from the search call, so Scan can
// fold a failed PR search into the scan's data-integrity verdict. Excluded
// repos are filtered client-side.
func (c *Collector) collectPRs(ctx context.Context) (int, error) {
	prs, err := c.client.SearchOpenPRs(ctx, c.owner, c.prExclude)
	if err != nil {
		if isShutdown(err) {
			c.logger.Debug("scan interrupted", "search", "open pull requests")
		} else {
			c.logger.Warn("open PR search failed", "error", err)
		}
		return 0, err
	}
	n := 0
	for i := range prs {
		pr := &prs[i]
		if !c.keep(pr.Repo) {
			continue
		}
		c.logger.Info("open pull request",
			"repo", pr.Repo, "number", pr.Number, "title", pr.Title,
			"author", pr.Author, "draft", pr.Draft, "url", pr.URL,
			"created_at", pr.CreatedAt.UTC().Format(time.RFC3339))
		n++
	}
	return n, nil
}

// collectIssues emits the current open-issue snapshot (cross-repo, one
// query) and returns the count surfaced plus any error from the search call.
func (c *Collector) collectIssues(ctx context.Context) (int, error) {
	issues, err := c.client.SearchOpenIssues(ctx, c.owner, c.issueExclude)
	if err != nil {
		if isShutdown(err) {
			c.logger.Debug("scan interrupted", "search", "open issues")
		} else {
			c.logger.Warn("open issue search failed", "error", err)
		}
		return 0, err
	}
	n := 0
	for i := range issues {
		is := &issues[i]
		if !c.keep(is.Repo) {
			continue
		}
		c.logger.Info("open issue",
			"repo", is.Repo, "number", is.Number, "title", is.Title,
			"author", is.Author, "labels", is.Labels, "url", is.URL,
			"created_at", is.CreatedAt.UTC().Format(time.RFC3339))
		n++
	}
	return n, nil
}

// collectRuns emits newly-seen completed runs for repo (event-once) and
// returns how many runs were new this scan, how many of those were failures,
// and any error from the list call (so Scan can fold a per-repo runs failure
// into the integrity verdict). Every completed run is emitted once as
// msg="workflow run" with its conclusion, so the dashboard filters that one
// stream for the failures view and computes the failure rate — no
// per-conclusion fan-out needed.
func (c *Collector) collectRuns(ctx context.Context, repo *ghsignal.Repo, cutoff time.Time) (newRuns, newFailures int, err error) {
	runs, err := c.client.ListRuns(ctx, *repo, cutoff)
	if err != nil {
		if isShutdown(err) {
			c.logger.Debug("scan interrupted", "repo", repo.FullName())
		} else {
			c.logger.Warn("partial failure listing runs", "repo", repo.FullName(), "error", err)
		}
	}
	for i := range runs {
		run := &runs[i]
		if _, ok := c.seen[run.RunID]; ok {
			c.logger.Debug("run already seen", "repo", run.Repo, "run_id", run.RunID)
			continue
		}
		c.seen[run.RunID] = run.CreatedAt
		c.logger.Info("workflow run",
			"repo", run.Repo, "workflow", run.Workflow, "conclusion", run.Conclusion,
			"branch", run.Branch, "event", run.Event, "run_number", run.RunNumber,
			"run_id", run.RunID, "url", run.URL,
			"created_at", run.CreatedAt.UTC().Format(time.RFC3339))
		newRuns++
		if ghsignal.IsFailureConclusion(run.Conclusion) {
			newFailures++
		}
	}
	return newRuns, newFailures, err
}

// collectAlerts emits the current code-scanning-alert snapshot for repo and
// returns the count plus any error. A repo that never ran code scanning yields
// ghsignal.ErrNoCodeScanning (GitHub's 404) — a benign "no data" outcome the
// collector stays silent on and Scan counts as neither readable nor blind. A
// 403 (Advanced Security off, a missing token scope, or a rate limit) is a real
// read failure surfaced as a warning, so Scan can fold a blind code-scanning
// read into the integrity verdict rather than reporting a false zero.
func (c *Collector) collectAlerts(ctx context.Context, repo *ghsignal.Repo) (int, error) {
	alerts, err := c.client.ListCodeScanningAlerts(ctx, *repo)
	if err != nil {
		switch {
		case errors.Is(err, ghsignal.ErrNoCodeScanning):
		case isShutdown(err):
			c.logger.Debug("scan interrupted", "repo", repo.FullName())
		default:
			c.logger.Warn("code scanning listing failed", "repo", repo.FullName(), "error", err)
		}
		return 0, err
	}
	for i := range alerts {
		a := &alerts[i]
		c.logger.Info("code scanning alert",
			"repo", a.Repo, "number", a.Number, "rule", a.Rule,
			"severity", a.Severity, "tool", a.Tool, "url", a.URL,
			"created_at", a.CreatedAt.UTC().Format(time.RFC3339))
	}
	return len(alerts), nil
}

// keep reports whether a cross-repo search result from repoFullName should be
// surfaced: it must belong to the configured owner (the Search API can return
// repos outside it) and not be in the operator's exclude set. The owner-prefix
// test is case-insensitive because GitHub's search can vary the owner's case.
func (c *Collector) keep(repoFullName string) bool {
	prefix := c.owner + "/"
	return strings.HasPrefix(strings.ToLower(repoFullName), strings.ToLower(prefix)) &&
		!c.excludedRepo(repoFullName)
}

// excludedRepo reports whether a "owner/name" full name is in the exclude
// set (which is keyed by bare repo name). The lookup is case-insensitive,
// mirroring keep()'s owner test and the lowercased exclude set. A name with
// no owner prefix is looked up whole.
func (c *Collector) excludedRepo(fullName string) bool {
	name := fullName
	if _, bare, found := strings.CutLast(fullName, "/"); found {
		name = bare
	}
	return c.excludedName(name)
}

// excludedName reports whether a bare repo name is in the exclude set. It
// lowercases its argument because the set (parseExcludes) is keyed by
// lowercased names — GitHub repo names are case-insensitive — so this is the
// single place the read-side normalization lives.
func (c *Collector) excludedName(name string) bool {
	return c.exclude[strings.ToLower(name)]
}

// isShutdown reports whether err is a context cancellation or deadline: a
// graceful shutdown (SIGTERM) rather than a data/read failure. Every
// per-signal collector and the integrity classifier treat this class the
// same way (Debug log, no degradation), so the invariant lives in one place.
func isShutdown(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// codeScanningExcluded skips only code-scanning reads, preserving other signals.
func (c *Collector) codeScanningExcluded(repo *ghsignal.Repo) bool {
	return (c.csSkipForks && repo.Fork) || c.csExclude[strings.ToLower(repo.Name)]
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

// loadState tolerates unreadable persisted state by starting cold.
func (c *Collector) loadState() {
	data, err := c.slot.Mutate(func(before []byte) []byte { return before })
	if err != nil {
		c.logger.Warn("dedup state unreadable; starting cold", "path", c.statePath, "error", err)
		return
	}
	if len(data) == 0 {
		return // first use: the slot was just created empty
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

// saveState merges persisted and in-memory run IDs under the slot lock.
func (c *Collector) saveState() {
	var marshalErr error
	if _, err := c.slot.Mutate(func(before []byte) []byte {
		data, err := json.Marshal(mergeSeen(before, c.seen))
		if err != nil {
			marshalErr = err
			return before // leave the slot untouched (the no-write idiom)
		}
		return data
	}); err != nil {
		c.logger.Warn("dedup state save failed", "path", c.statePath, "error", err)
		return
	}
	if marshalErr != nil {
		c.logger.Warn("dedup state marshal failed", "error", marshalErr)
	}
}

// mergeSeen unions the persisted dedup set in before with the in-memory set.
// Self-healing per the SlotFile contract: empty, torn, truncated, or garbage
// bytes parse as an empty set rather than erroring, so a corrupt slot degrades
// to "this process's view wins" instead of failing the save. Kept small
// (parse, union, marshal on the caller side) because it runs under the flock.
func mergeSeen(before []byte, seen map[int64]time.Time) map[int64]time.Time {
	var persisted map[int64]time.Time
	if len(before) > 0 {
		_ = json.Unmarshal(before, &persisted) // self-heal: garbage -> empty
	}
	merged := make(map[int64]time.Time, len(persisted)+len(seen))
	maps.Copy(merged, persisted)
	maps.Copy(merged, seen)
	return merged
}
