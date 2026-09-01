// Package main implements github-scout: scans a GitHub owner's repositories
// on a schedule and emits open PRs/issues, code-scanning alerts, and Actions
// runs as structured log lines for Loki.
//
// main.go is a pure composition root: config -> *http.Client -> github.Client
// -> collect.Collector -> health.Marker. All logic lives in internal/*.
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
	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/slogx"
)

// seenStatePath persists the run dedup set across process lifetimes via a
// flock'd merge-on-save slot (scheduler.SlotFile), so a concurrent writer
// pair cannot lose entries to a last-writer-wins overwrite. Best-effort:
// lives on /tmp, so a container recreate re-emits the lookback window once.
const seenStatePath = "/tmp/seen-runs.json"

// condCachePath persists the GitHub client's conditional-request cache
// (per-URL ETag/Last-Modified validators plus the validated item subset),
// so an unchanged resource revalidates as a free 304. Same best-effort
// /tmp contract as seenStatePath.
const condCachePath = "/tmp/cond-cache.json"

func main() {
	// JSON handler installed before anything logs, so config.Load warnings
	// are JSON too; setupLogging sets the real level once config is read.
	logLevel = slogx.Setup(slogx.Options{Format: slogx.JSON, Output: os.Stdout})

	// Distroless image has no shell, so subcommands are the CLI surface.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "health":
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
		// guard is a fallback since health is a separately versioned dep.
		os.Exit(0)
	}

	cfg, valid := loadConfig()
	if !valid {
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Marker is pure loop liveness, refreshed after every iteration
	// regardless of scan outcome; a bad token or rate limit is reported on
	// the log channel instead, since a restart cannot fix either.
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

// runTrigger executes a single scan and exits. os.Exit lives here, free of
// pending defers; doTrigger holds the defers and returns the exit code.
func runTrigger() {
	os.Exit(doTrigger())
}

// doTrigger loads config, runs one scan, and returns the process exit code.
// It deliberately never touches the /tmp/.healthy marker, which belongs to
// the scheduled daemon's loop-liveness probe.
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

// loadConfig loads config, installs the log level, logs the active config,
// then validates it. Returns the config and whether it is valid; on invalid
// config it logs the diagnostic and returns false, leaving the abort to the
// caller.
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
// collect.Collector. The caller owns CloseIdleConnections on the returned client.
func buildCollector(cfg *config.Config) (*collect.Collector, *http.Client) {
	httpClient := httpx.NewClient(30 * time.Second)
	gh := github.NewClient(github.Options{
		HTTP:          httpClient,
		Token:         cfg.Token,
		Logger:        slog.Default(),
		CondCachePath: condCachePath,
	})
	collector := collect.New(&collect.Deps{
		Client:                   gh,
		Logger:                   slog.Default(),
		Owner:                    cfg.Owner,
		Lookback:                 cfg.Lookback,
		Exclude:                  cfg.ExcludeRepos,
		CodeScanningExclude:      cfg.CodeScanningExcludeRepos,
		CodeScanningExcludeForks: cfg.CodeScanningExcludeForks,
		PRExclude:                cfg.PRExclude,
		IssueExclude:             cfg.IssueExclude,
		StatePath:                seenStatePath,
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
// (avoids a synchronized hammer on the GitHub API across restarts) until ctx
// is cancelled. FireOnStart runs the first scan immediately on boot.
//
// The marker refresh is unconditional: it asserts the loop completed an
// iteration, not that the scan found the data healthy. A failing scan
// refreshes it too — the failure is already on the log channel.
func runScheduled(ctx context.Context, interval time.Duration, collector *collect.Collector, marker *health.Marker) {
	scheduler.RunLoop(ctx, func(ctx context.Context) {
		runScan(ctx, collector)
		marker.Set(true)
	}, scheduler.LoopOptions{Interval: interval, FireOnStart: true, Jitter: 0.10})
}

// logLevel backs the JSON handler installed at the start of main(). JSON
// (not the shared text handler) because workflow names/branches contain
// spaces and slashes that JSON encodes unambiguously where logfmt quoting
// is fragile.
var logLevel *slog.LevelVar

// setupLogging sets the configured level on logLevel. Called once by
// loadConfig after LOG_LEVEL is read; until then the handler runs at the
// LevelVar default (Info).
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
