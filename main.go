// Package main implements github-scout, which scans all of a GitHub
// owner's repositories on a schedule and emits the four actionable
// signals — failed Actions runs, open pull requests, open issues, and
// code-scanning alerts — as structured log lines for Loki. It is the
// single source for a cross-repo GitHub dashboard, replacing the Grafana
// GitHub-datasource plugin (which cannot enumerate "all workflows across
// all repos" and has no cross-repo view).
//
// main.go is a pure composition root: it wires config -> *http.Client ->
// github.Client -> collect.Collector -> health.Marker. All logic lives in
// internal/*; this file holds no business rules.
//
// Two run modes (the containerized daemon always self-schedules):
//   - scheduled (the default, 15m): an internal jittered timer drives
//     scans in the resident process. Because stdout of PID 1 IS the
//     product transport (Alloy ships it to Loki under the container name),
//     scan execution never leaves this process — there is no externally-
//     scheduled container mode (a scan run via `docker exec` would write
//     to the exec session, not the container stream, silently blinding
//     the bundled dashboard and alerts).
//   - trigger (`github-scout trigger`): one one-shot scan, then exit 0/1 —
//     the dev loop (`go run . trigger`), cron on a bare host, CI. Here the
//     invoking context's stdout is exactly where the output belongs.
//
// Output model is slog-to-stdout, not a /metrics endpoint: these signals
// are high-cardinality events/records (run IDs, PR/issue numbers, URLs),
// not numeric time-series, so they belong in Loki. There is no HTTP server
// and no listening port; health is a file marker checked by the `health`
// subcommand.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/cplieger/github-scout/internal/collect"
	"github.com/cplieger/github-scout/internal/config"
	"github.com/cplieger/github-scout/internal/github"
	"github.com/cplieger/github-scout/internal/urlsafe"
	"github.com/cplieger/health"
	"github.com/cplieger/httpx/v3"
	"github.com/cplieger/scheduler/v2"
	"github.com/cplieger/slogx"
)

// seenStatePath persists the run dedup set across process lifetimes. The
// scheduled daemon saves after each scan, so a plain container restart
// re-emits nothing; repeated one-shot `trigger` processes on the same host
// (cron, the dev loop) reload each other's set the same way. A container
// recreate clears /tmp, which at worst re-emits the lookback window once
// (the documented cold-start behaviour). Saves go through a flock'd
// merge-on-save slot (scheduler.SlotFile), so even an out-of-contract
// writer pair — someone hand-execing a `trigger` inside the scheduled
// container, or two overlapping cron triggers — cannot lose entries to a
// last-writer-wins overwrite.
const seenStatePath = "/tmp/seen-runs.json"

// condCachePath persists the GitHub client's conditional-request cache:
// per-URL ETag/Last-Modified validators plus the item subset they validate,
// for the endpoints whose URLs are stable across scans (the repo listing and
// per-repo code-scanning alerts). An unchanged resource then revalidates as
// a 304 — which GitHub serves without charging the primary rate limit — and
// the snapshot is re-emitted from the cached items. Same best-effort /tmp
// contract as seenStatePath (flock'd SlotFile, cold start on recreate),
// shared by the daemon and one-shot trigger processes.
const condCachePath = "/tmp/cond-cache.json"

func main() {
	// Install the JSON handler before anything logs (including config.Load
	// warnings) so every line is JSON on stdout; setupLogging sets the level
	// once config is read.
	logLevel = slogx.Setup(slogx.Options{Format: slogx.JSON, Output: os.Stdout})

	// CLI subcommands for the distroless image (no shell): `health` for the
	// Docker healthcheck (checks the marker file), `trigger` for a one-shot
	// scan driven by an external scheduler (Ofelia job-exec).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "health":
			// The probe arms a freshness deadline: the daemon refreshes the
			// marker after every loop iteration, so a marker older than 3
			// intervals means a wedged scan loop and a restart fixes it (the
			// 40m GithubScoutScanStalled alert pages first at the deployed
			// 15m interval; this restarts at 45m). This is the ONLY failure
			// class a restart repairs — data outcomes (bad token, rate
			// limit) live on the log channel, not the marker (see
			// runScheduled).
			health.RunProbe(health.DefaultPath,
				health.WithMaxAge(3*config.ScanInterval()))
		case "trigger":
			runTrigger()
		default:
			slog.Error("unknown subcommand", "arg", os.Args[1],
				"valid", "health, trigger, or no argument for daemon")
			os.Exit(2)
		}
		// health.RunProbe and runTrigger both terminate via os.Exit; this
		// guard makes the invariant explicit instead of depending on those
		// callees never returning (health is a separately versioned dependency).
		os.Exit(0)
	}

	cfg, valid := loadConfig()
	if !valid {
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The marker is pure LOOP LIVENESS: healthy on boot (a slow first scan
	// never gates startup health), refreshed after every loop iteration
	// regardless of the scan's outcome, and judged stale by the health
	// probe's max-age when the loop wedges. Scan outcomes — a bad token, a
	// rate limit, a blind signal — are deliberately NOT health: a restart
	// cannot fix any of them (the loop already retries next tick), and a
	// restart storm on a revoked token is noise. Those live on the log
	// channel ("repo discovery failed", "scan degraded", the absence of
	// "scan complete") where the bundled Loki rules page on them.
	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)
	defer marker.Cleanup()

	collector, httpClient := buildCollector(&cfg)
	defer httpClient.CloseIdleConnections()
	marker.Set(true)
	slog.Info("scheduled mode", "interval", cfg.ScanInterval, "jitter", "±10%")
	runScheduled(ctx, cfg.ScanInterval, collector, marker)

	slog.Info("shutdown complete", "cause", context.Cause(ctx))
}

// runTrigger executes a single scan and exits — the dev loop, cron on a
// bare host, CI. os.Exit lives here, free of pending defers; doTrigger
// holds the defers and returns the exit code.
func runTrigger() {
	os.Exit(doTrigger())
}

// doTrigger loads config, runs one scan, and returns the process exit code
// (0 healthy, 1 unhealthy / misconfigured). The exit code and the process's
// own stdout are the trigger's entire contract: it deliberately never
// touches the /tmp/.healthy marker, which belongs to the scheduled daemon's
// loop-liveness probe (a stray `docker exec … trigger` inside the scheduled
// container must not be able to refresh — or clear — the daemon's liveness
// signal). Repeated triggers still share the run dedup set at seenStatePath,
// so each completed run is emitted exactly once across one-shot processes;
// open PRs / issues / alerts remain pure snapshots (re-emitted every scan
// by design).
func doTrigger() int {
	cfg, valid := loadConfig()
	if !valid {
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	collector, httpClient := buildCollector(&cfg)
	defer httpClient.CloseIdleConnections()

	ok := runScan(ctx, collector)
	slog.Info("trigger scan complete", "healthy", ok)
	if !ok {
		return 1
	}
	return 0
}

// loadConfig runs the startup preamble shared by the daemon (main) and the
// one-shot trigger (doTrigger): load config, install the log level, log the
// active config, then validate. It returns the loaded config and whether it
// is valid; on invalid config it logs the diagnostic error and returns false,
// leaving the abort (os.Exit vs return) to the caller.
func loadConfig() (config.Config, bool) {
	cfg := config.Load()
	setupLogging(cfg.LogLevel)
	logConfig(&cfg)
	if !cfg.Valid() {
		slog.Error("invalid configuration; need GITHUB_OWNER and GITHUB_TOKEN",
			"owner_set", cfg.Owner != "", "token_set", cfg.Token != "",
			"owner_safe", cfg.Owner == "" || urlsafe.IsSafeURLSegment(cfg.Owner))
		return cfg, false
	}
	return cfg, true
}

// buildCollector wires config -> *http.Client -> github.Client ->
// collect.Collector. Shared by the scheduled daemon path and the one-shot
// trigger path. The caller owns CloseIdleConnections on the returned client.
func buildCollector(cfg *config.Config) (*collect.Collector, *http.Client) {
	httpClient := httpx.NewClient(30 * time.Second)
	gh := github.NewClient(httpClient, cfg.Token, nil, slog.Default(), condCachePath)
	collector := collect.New(&collect.Deps{
		Client:              gh,
		Logger:              slog.Default(),
		Owner:               cfg.Owner,
		Lookback:            cfg.Lookback,
		Exclude:             cfg.ExcludeRepos,
		CodeScanningExclude: cfg.CodeScanningExcludeRepos,
		PRExclude:           cfg.PRExclude,
		IssueExclude:        cfg.IssueExclude,
		StatePath:           seenStatePath,
	})
	return collector, httpClient
}

// runScan executes one scan, recovering from a panic so a single bad
// cycle can't crash the long-lived poller. Returns the health flag.
func runScan(ctx context.Context, collector *collect.Collector) (healthy bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scan panicked", "panic", r, "stack", string(debug.Stack()))
			healthy = false
		}
	}()
	return collector.Scan(ctx)
}

// runScheduled scans on each tick of a ScanInterval timer with ±10% jitter
// until ctx is cancelled, via scheduler.RunLoop. Jitter avoids a predictable,
// synchronized hammer on the GitHub API across restarts. FireOnStart runs the
// first scan immediately on boot (not after a full interval).
//
// The marker refresh is unconditional: it asserts "the scan loop completed
// an iteration", not "the scan found the data healthy". A failing scan
// (bad token, rate limit, blind signal) refreshes it too — the loop is
// alive and will retry next tick, which is everything a restart could
// achieve; the failure itself is already on the log channel where the
// bundled alerts consume it. Only a wedged loop stops refreshing, and the
// health probe's max-age converts that staleness into a restart.
func runScheduled(ctx context.Context, interval time.Duration, collector *collect.Collector, marker *health.Marker) {
	scheduler.RunLoop(ctx, func(ctx context.Context) {
		runScan(ctx, collector)
		marker.Set(true)
	}, scheduler.LoopOptions{Interval: interval, FireOnStart: true, Jitter: 0.10})
}

// logLevel backs the JSON handler installed at the start of main(). The JSON
// handler (not the shared default text handler) is deliberate: the product IS
// structured events rendered as Grafana table columns, and workflow names /
// branches contain spaces and slashes that JSON encodes unambiguously where
// logfmt quoting is fragile (the shared error-matching regex covers JSON, so
// github-scout's own error logs are still caught by the shared error panels).
// Installing it at the start of main() — before config.Load runs — means even
// config-validation warnings emit as JSON on stdout, not text on stderr.
var logLevel *slog.LevelVar

// setupLogging sets the configured level on logLevel, the LevelVar backing the
// handler installed in main(). Called once by loadConfig after LOG_LEVEL is
// read; until then the handler runs at the LevelVar default (Info), so early
// config.Load() warnings still emit.
func setupLogging(level slog.Level) {
	logLevel.Set(level)
}

// logConfig logs the active configuration at startup. The token is never
// logged — only whether one is present.
func logConfig(cfg *config.Config) {
	slog.Info("configuration loaded",
		"owner", cfg.Owner,
		"token_set", cfg.Token != "",
		"scan_interval", cfg.ScanInterval.String(),
		"lookback", cfg.Lookback,
		"excluded_repos", len(cfg.ExcludeRepos),
		"code_scanning_excluded_repos", len(cfg.CodeScanningExcludeRepos))
}
