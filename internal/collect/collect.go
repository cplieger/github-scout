// Package collect is github-scout's scan orchestrator. One Collector
// instance lives for the process lifetime; each Scan discovers the
// owner's repos, lists their recent failed workflow runs, and emits each
// newly-seen failure exactly once as a structured log line for Alloy to
// ship to Loki.
//
// "Exactly once" is the whole design: emitting each RunID a single time
// means a plain LogQL count over the events equals the number of distinct
// failures, with no dedup gymnastics in the dashboard. The seen-set is
// in-memory (github-scout is stateless, like registry-stats) and pruned
// to the lookback window, so a process restart re-emits at most the
// failures still inside that window — rare, since failures are infrequent
// and restarts rarer.
package collect

import (
	"context"
	"log/slog"
	"time"

	"github.com/cplieger/github-scout/internal/model"
)

// Collector holds the cross-scan state: the GitHub client, the scan
// parameters, and the in-memory dedup set. Construct via New.
type Collector struct {
	client   apiClient
	logger   *slog.Logger
	now      func() time.Time
	seen     map[int64]time.Time // RunID -> run CreatedAt, pruned to lookback
	exclude  map[string]bool
	owner    string
	lookback time.Duration
}

// apiClient is the consumer-side view of the GitHub client the collector
// needs. *github.Client satisfies it in production; tests pass a fake so
// the dedup/emit/prune logic can be exercised without HTTP. Kept here
// (not in internal/github) per the Go idiom of defining interfaces where
// they are consumed.
type apiClient interface {
	ListRepos(ctx context.Context, owner string) ([]model.Repo, error)
	ListFailedRuns(ctx context.Context, repo model.Repo, since time.Time) ([]model.FailedRun, error)
}

// Deps are the constructor arguments for New. A nil Logger falls back to
// slog.Default; a nil Now falls back to time.Now (tests inject both).
type Deps struct {
	Client   apiClient
	Logger   *slog.Logger
	Now      func() time.Time
	Exclude  map[string]bool
	Owner    string
	Lookback time.Duration
}

// New constructs a Collector with an empty seen-set.
func New(d Deps) *Collector {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Collector{
		client:   d.Client,
		logger:   logger,
		now:      now,
		seen:     make(map[int64]time.Time),
		exclude:  d.Exclude,
		owner:    d.Owner,
		lookback: d.Lookback,
	}
}

// Scan runs one full discovery+collection cycle and returns whether the
// scan was healthy. Health contract:
//
//   - true  — repo discovery succeeded. Per-repo run-list failures are
//     tolerated (logged as warnings); a healthy scan can still have
//     surfaced zero new failures (the common, good case: nothing broke).
//   - false — repo discovery itself failed (bad token, rate limit, owner
//     typo). Without the repo list there is nothing to scan, so the
//     caller should flip its health marker and the operator should look.
//
// Newly-seen failures are emitted as INFO log lines (the failure is in the
// watched repo, not in github-scout — the scout is working correctly when
// it reports one). The seen-set is pruned to the lookback window at the
// end of every scan so memory stays bounded.
func (c *Collector) Scan(ctx context.Context) (healthy bool) {
	start := c.now()
	cutoff := start.Add(-c.lookback)

	repos, err := c.client.ListRepos(ctx, c.owner)
	if err != nil {
		c.logger.Error("repo discovery failed", "owner", c.owner, "error", err)
		return false
	}
	c.logger.Info("scanning repos", "owner", c.owner, "count", len(repos), "since", cutoff.Format(time.RFC3339))

	scanned, skipped, newFailures := 0, 0, 0
	for _, repo := range repos {
		if c.exclude[repo.Name] {
			skipped++
			continue
		}
		scanned++

		runs, err := c.client.ListFailedRuns(ctx, repo, cutoff)
		if err != nil {
			// Partial degradation: keep the runs we did get, keep going.
			c.logger.Warn("partial failure listing runs", "repo", repo.FullName(), "error", err)
		}
		for i := range runs {
			run := &runs[i]
			if c.markSeen(run) {
				c.emit(run)
				newFailures++
			}
		}
	}

	c.prune(cutoff)
	c.logger.Info("scan complete",
		"scanned", scanned, "skipped", skipped,
		"new_failures", newFailures, "tracked", len(c.seen),
		"duration", c.now().Sub(start).Round(time.Millisecond))
	return true
}

// markSeen records run as seen and reports whether it was new (i.e. not
// previously emitted). The stored timestamp drives lookback pruning.
func (c *Collector) markSeen(run *model.FailedRun) (isNew bool) {
	if _, ok := c.seen[run.RunID]; ok {
		return false
	}
	c.seen[run.RunID] = run.CreatedAt
	return true
}

// emit writes a single failed-run event. The message is a stable string
// the Grafana "Failed Actions" panel and any Loki ruler alert filter on;
// the fields are the actionable detail (repo, workflow, branch, the
// clickable run URL).
func (c *Collector) emit(run *model.FailedRun) {
	c.logger.Info("workflow run failed",
		"repo", run.Repo,
		"workflow", run.Workflow,
		"conclusion", run.Conclusion,
		"branch", run.Branch,
		"event", run.Event,
		"run_number", run.RunNumber,
		"run_id", run.RunID,
		"url", run.URL,
		"created_at", run.CreatedAt.UTC().Format(time.RFC3339),
	)
}

// prune drops seen entries older than cutoff. The GitHub query already
// filters to runs at or after cutoff, so an older entry can never be
// returned again and is safe to forget — this bounds the map to the
// failures inside the lookback window.
func (c *Collector) prune(cutoff time.Time) {
	for id, created := range c.seen {
		if created.Before(cutoff) {
			delete(c.seen, id)
		}
	}
}
