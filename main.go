package main

// github-scout scans all of a GitHub owner's repositories on a schedule
// and emits each newly-detected failed Actions run (build, release,
// scheduled job, ...) as a structured log line for Loki. It exists
// because the Grafana GitHub-datasource plugin cannot enumerate "all
// workflows across all repos", and private repos have no org-level alert
// endpoint — so a small poller is the only way to get a single cross-repo
// "what just broke" view.
//
// main.go is a pure composition root: it wires config -> *http.Client ->
// github.Client -> collect.Collector -> health.Marker, then runs the
// signal-driven poll loop. All logic lives in internal/*; this file holds
// no business rules.
//
// Output model is slog-to-stdout, not a /metrics endpoint: failed runs are
// high-cardinality events (unique run IDs/URLs), not numeric time-series,
// so they belong in Loki. There is no HTTP server and no listening port;
// health is a file marker checked by the `health` subcommand.

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cplieger/github-scout/internal/collect"
	"github.com/cplieger/github-scout/internal/config"
	"github.com/cplieger/github-scout/internal/github"
	"github.com/cplieger/health"
	"github.com/cplieger/httpx"
)

func main() {
	// CLI health probe for the Docker healthcheck (distroless has no
	// curl/wget). Checks the marker file — no port needed.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		health.RunProbe(health.DefaultPath)
	}

	cfg := config.Load()
	setupLogging(cfg.LogLevel)
	logConfig(cfg)

	if !cfg.Valid() {
		slog.Error("invalid configuration; need GITHUB_OWNER and GITHUB_TOKEN",
			"owner_set", cfg.Owner != "", "token_set", cfg.Token != "")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Clear any stale marker from a previous crash so the probe reports
	// unhealthy until the first scan completes.
	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)
	defer marker.Cleanup()

	httpClient := httpx.NewClient(30 * time.Second)
	gh := github.NewClient(httpClient, cfg.Token, nil, slog.Default())
	collector := collect.New(collect.Deps{
		Client:   gh,
		Logger:   slog.Default(),
		Owner:    cfg.Owner,
		Lookback: cfg.Lookback,
		Exclude:  cfg.ExcludeRepos,
	})

	// First scan inline so the container reports healthy (or not) quickly.
	marker.Set(runScan(ctx, collector))

	if cfg.PollInterval == 0 {
		slog.Info("one-shot mode; scan complete, idling until signal")
		<-ctx.Done()
	} else {
		slog.Info("scheduled mode", "interval", cfg.PollInterval, "jitter", "±10%")
		runScheduled(ctx, cfg.PollInterval, collector, marker)
	}

	marker.Cleanup()
	httpx.Close(httpClient)
	slog.Info("shutdown complete")
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

// runScheduled scans on each tick of a PollInterval timer with ±10%
// jitter until ctx is cancelled. Jitter avoids a predictable, synchronized
// hammer on the GitHub API across restarts.
func runScheduled(ctx context.Context, interval time.Duration, collector *collect.Collector, marker *health.Marker) {
	for {
		jitterMax := max(1, int(interval/5))
		jitter := time.Duration(rand.IntN(jitterMax)) //nolint:gosec // G404: scheduling jitter, not crypto
		delay := interval - interval/10 + jitter
		timer := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			marker.Set(runScan(ctx, collector))
		}
	}
}

// setupLogging configures slog. github-scout uses the JSON handler (not
// the fleet-default text handler) deliberately: its product IS structured
// events rendered as Grafana table columns, and workflow names / branches
// contain spaces and slashes that JSON encodes unambiguously where logfmt
// quoting is fragile. The homelab error-matching regex covers JSON, so
// github-scout's own error logs are still caught by the cross-fleet panels.
func setupLogging(level slog.Level) {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout,
		&slog.HandlerOptions{Level: level})))
}

// logConfig logs the active configuration at startup. The token is never
// logged — only whether one is present.
func logConfig(cfg config.Config) {
	slog.Info("configuration loaded",
		"owner", cfg.Owner,
		"token_set", cfg.Token != "",
		"poll_interval", cfg.PollInterval,
		"lookback", cfg.Lookback,
		"excluded_repos", len(cfg.ExcludeRepos))
}
