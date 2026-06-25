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
// Three run modes (matching the fleet's scheduled-app convention):
//   - scheduled    (SCAN_INTERVAL > 0): an internal jittered timer.
//   - resident-idle (SCAN_INTERVAL = off): no internal timer; sits healthy
//     and idle, awaiting external `github-scout trigger` execs (Ofelia).
//   - trigger      (`github-scout trigger`): one one-shot scan, then exit
//     0/1 — the target for an Ofelia job-exec or cron.
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
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cplieger/github-scout/internal/collect"
	"github.com/cplieger/github-scout/internal/config"
	"github.com/cplieger/github-scout/internal/github"
	"github.com/cplieger/github-scout/internal/urlsafe"
	"github.com/cplieger/health"
	"github.com/cplieger/httpx/v2"
)

// seenStatePath persists the run dedup set across one-shot `trigger`
// processes. It lives in /tmp alongside the health marker: /tmp is shared
// across `docker exec` triggers of the same running container, so each
// trigger reloads the previous one's set and re-emits nothing. A container
// recreate clears /tmp, which at worst re-emits the lookback window once
// (the documented cold-start behaviour). In scheduled/resident mode the
// long-lived process also persists here, so a plain restart no longer
// re-emits either.
const seenStatePath = "/tmp/seen-runs.json"

func main() {
	// Install the JSON handler before anything logs (including config.Load
	// warnings) so every line is JSON on stdout; setupLogging sets the level
	// once config is read.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout,
		&slog.HandlerOptions{Level: logLevel})))

	// CLI subcommands for the distroless image (no shell): `health` for the
	// Docker healthcheck (checks the marker file), `trigger` for a one-shot
	// scan driven by an external scheduler (Ofelia job-exec).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "health":
			health.RunProbe(health.DefaultPath)
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

	// Clear any stale marker from a previous crash so the probe reports
	// unhealthy until the first scan completes.
	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)
	defer marker.Cleanup()

	collector, httpClient := buildCollector(&cfg)
	defer httpx.Close(httpClient)

	if cfg.ScanInterval == 0 {
		// Resident-idle: no internal timer. Scans are driven externally by
		// `github-scout trigger` (Ofelia job-exec). Healthy while idle; the
		// trigger runs update the marker to reflect each scan's outcome.
		marker.Set(true)
		slog.Info("resident-idle mode", "reason", "SCAN_INTERVAL=off, awaiting external trigger")
		<-ctx.Done()
	} else {
		// First scan inline so the container reports healthy (or not) quickly.
		marker.Set(runScan(ctx, collector))
		slog.Info("scheduled mode", "interval", cfg.ScanInterval, "jitter", "±10%")
		runScheduled(ctx, cfg.ScanInterval, collector, marker)
	}

	slog.Info("shutdown complete")
}

// runTrigger executes a single scan and exits — the target for external
// schedulers (Ofelia job-exec, cron). os.Exit lives here, free of pending
// defers; doTrigger holds the defers and returns the exit code.
func runTrigger() {
	os.Exit(doTrigger())
}

// doTrigger loads config, runs one scan, and returns the process exit code
// (0 healthy, 1 unhealthy / misconfigured). Each trigger is an independent
// process, but the run dedup set is persisted to seenStatePath (in /tmp,
// shared across `docker exec` triggers of the same running container), so a
// trigger reloads the previous one's set and emits each completed run
// exactly once — no re-emission across triggers. A container recreate clears
// /tmp and at worst re-emits the lookback window once. Open PRs / issues /
// alerts remain pure snapshots (re-emitted every scan by design).
func doTrigger() int {
	cfg, valid := loadConfig()
	if !valid {
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No marker.Cleanup here: in resident-idle deployments the main process
	// owns /tmp/.healthy; this trigger runs as a separate `docker exec` and
	// only updates the marker to reflect the run's outcome — deleting it
	// would mark the resident container unhealthy.
	marker := health.NewMarker(health.DefaultPath)

	collector, httpClient := buildCollector(&cfg)
	defer httpx.Close(httpClient)

	ok := runScan(ctx, collector)
	marker.Set(ok)
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
// collect.Collector. Shared by the scheduled/resident main path and the
// one-shot trigger path. The caller owns httpx.Close on the returned client.
func buildCollector(cfg *config.Config) (*collect.Collector, *http.Client) {
	httpClient := httpx.NewClient(30 * time.Second)
	gh := github.NewClient(httpClient, cfg.Token, nil, slog.Default())
	collector := collect.New(&collect.Deps{
		Client:       gh,
		Logger:       slog.Default(),
		Owner:        cfg.Owner,
		Lookback:     cfg.Lookback,
		Exclude:      cfg.ExcludeRepos,
		PRExclude:    cfg.PRExclude,
		IssueExclude: cfg.IssueExclude,
		StatePath:    seenStatePath,
	})
	return collector, httpClient
}

// runScan executes one scan, recovering from a panic so a single bad
// cycle can't crash the long-lived poller. Returns the health flag.
func runScan(ctx context.Context, collector *collect.Collector) (healthy bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scan panicked", "panic", r)
			healthy = false
		}
	}()
	return collector.Scan(ctx)
}

// jitteredDelay returns the next poll delay: interval drawn uniformly from
// [interval-interval/10, interval+interval/10). Extracted from runScheduled
// so the ±10% band invariant can be property-tested in isolation.
func jitteredDelay(interval time.Duration) time.Duration {
	jitterMax := max(1, int(interval/5))
	jitter := time.Duration(rand.IntN(jitterMax)) //nolint:gosec // G404: scheduling jitter, not crypto
	return interval - interval/10 + jitter
}

// runScheduled scans on each tick of a ScanInterval timer with ±10% jitter
// until ctx is cancelled. Jitter avoids a predictable, synchronized hammer
// on the GitHub API across restarts.
func runScheduled(ctx context.Context, interval time.Duration, collector *collect.Collector, marker *health.Marker) {
	for {
		timer := time.NewTimer(jitteredDelay(interval))

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			marker.Set(runScan(ctx, collector))
		}
	}
}

// logLevel backs the JSON handler installed at the start of main(). The JSON
// handler (not the fleet-default text handler) is deliberate: the product IS
// structured events rendered as Grafana table columns, and workflow names /
// branches contain spaces and slashes that JSON encodes unambiguously where
// logfmt quoting is fragile (the homelab error-matching regex covers JSON, so
// github-scout's own error logs are still caught by the cross-fleet panels).
// Installing it at the start of main() — before config.Load runs — means even
// config-validation warnings emit as JSON on stdout, not text on stderr.
var logLevel = new(slog.LevelVar)

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
	mode := "resident-idle"
	if cfg.ScanInterval > 0 {
		mode = cfg.ScanInterval.String()
	}
	slog.Info("configuration loaded",
		"owner", cfg.Owner,
		"token_set", cfg.Token != "",
		"scan_interval", mode,
		"lookback", cfg.Lookback,
		"excluded_repos", len(cfg.ExcludeRepos))
}
