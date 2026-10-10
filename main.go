// Package main is forge-scout: on a schedule it scans the repositories of the
// configured owners on GitHub, GitLab, Gitea and Forgejo instances and emits
// open pull requests, open issues, CI runs and GitHub code-scanning alerts as
// structured JSON log lines.
//
// main is the composition root: config, the forge connections, the run state
// store, the collector and the health marker. All logic lives in internal/*.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cplieger/forgeapi/github"
	"github.com/cplieger/github-scout/internal/collect"
	"github.com/cplieger/github-scout/internal/config"
	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/github-scout/internal/forgeconn"
	"github.com/cplieger/github-scout/internal/ghquota"
	"github.com/cplieger/github-scout/internal/githubrest"
	"github.com/cplieger/github-scout/internal/runstate"
	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/slogx"
)

// stateDir is the volume the run state lives on.
const stateDir = "/data"

func main() {
	// The JSON handler is installed before anything logs, so config warnings
	// are JSON too; the configured level is set after they are emitted.
	logLevel := slogx.Setup(slogx.Options{Format: slogx.JSON, Output: os.Stdout})
	base := scopeDefault()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "health":
			health.RunProbe(health.DefaultPath, health.WithMaxAge(healthLease(config.ScanIntervalFromFile(config.Path())).Duration()))
		case "trigger":
			os.Exit(runTrigger(logLevel, base, stateDir))
		default:
			slog.Error("unknown subcommand", "arg", os.Args[1], "valid", "health, trigger, or no argument for daemon")
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(runDaemon(logLevel, base, stateDir))
}

// healthLease is how stale the marker may grow: three intervals, plus one
// scan at its limit between two refreshes.
func healthLease(interval time.Duration) health.Lease {
	return health.Lease{Interval: interval, Cycles: 3, Timeout: config.ScanLimit(interval), Attempts: 1}
}

// scopeDefault scopes slog.Default as a line about no single connection, for
// the libraries that log through it, and returns the unscoped logger.
func scopeDefault() (base *slog.Logger) {
	base = slog.Default()
	slog.SetDefault(collect.Scope(base, forge.ProductUnknown, ""))
	return base
}

// Every function below takes base, the logger with neither forge nor
// connection, and scopes each line it logs with collect.Scope.

func runDaemon(logLevel *slog.LevelVar, base *slog.Logger, dir string) int {
	cfg, ok := loadConfig(logLevel, base, config.Path())
	if !ok {
		return 1
	}
	store, ok := openStore(base, dir)
	if !ok {
		return 1
	}
	defer store.Close()
	log := collect.Scope(base, forge.ProductUnknown, "")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The marker is loop liveness only: a bad token or a blind signal is
	// reported on the log channel, since a restart fixes neither.
	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)
	defer marker.Cleanup()

	collector := newCollector(ctx, &cfg, base, store)
	defer collector.Close()
	marker.Set(true)
	log.Info("scheduled mode", "interval", cfg.ScanInterval.String(), "jitter", fmt.Sprintf("±%d%%", config.ScanJitterPercent))
	scheduler.RunLoop(ctx, func(ctx context.Context) {
		collector.Scan(ctx)
		marker.Set(true)
	}, scheduler.LoopOptions{Interval: cfg.ScanInterval, FireOnStart: true, Jitter: config.ScanJitter})
	log.Info("shutdown complete", "cause", context.Cause(ctx))
	return 0
}

// runTrigger runs one scan and returns the exit code. It never touches the
// health marker, which belongs to the daemon's loop.
func runTrigger(logLevel *slog.LevelVar, base *slog.Logger, dir string) int {
	cfg, ok := loadConfig(logLevel, base, config.Path())
	if !ok {
		return 1
	}
	store, ok := openStore(base, dir)
	if !ok {
		return 1
	}
	defer store.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	collector := newCollector(ctx, &cfg, base, store)
	defer collector.Close()
	return trigger(ctx, collector, base)
}

// trigger runs the one-shot scan and returns its exit code: 0 only for a
// scan that read every signal of every connection whole.
func trigger(ctx context.Context, collector *collect.Collector, base *slog.Logger) int {
	log := collect.Scope(base, forge.ProductUnknown, "")
	outcome := collector.Scan(ctx)
	if outcome == collect.Interrupted {
		log.Warn("trigger scan interrupted", "cause", context.Cause(ctx))
		return 1
	}
	log.Info("trigger scan complete", "outcome", outcome.String())
	if outcome != collect.Complete {
		return 1
	}
	return 0
}

func loadConfig(logLevel *slog.LevelVar, base *slog.Logger, path string) (config.Config, bool) {
	log := collect.Scope(base, forge.ProductUnknown, "")
	cfg, warns, err := config.Load(path)
	for _, w := range warns {
		log.LogAttrs(context.Background(), slog.LevelWarn, w.Msg, w.Attrs...)
	}
	if err != nil {
		log.Error("invalid configuration", "path", path, "error", err)
		return cfg, false
	}
	logLevel.Set(cfg.LogLevel)
	for i := range cfg.Connections {
		c := &cfg.Connections[i]
		collect.Scope(base, forge.ProductUnknown, c.Name).Info("connection configured",
			"url", c.URL, "owners", len(c.Owners), "token_set", c.Token != "")
	}
	log.Info("configuration loaded", "path", path, "connections", len(cfg.Connections),
		"scan_interval", cfg.ScanInterval.String(), "lookback", cfg.Lookback.String())
	return cfg, true
}

// openStore takes the run state directory for this process. A directory
// another forge-scout process holds, or one it cannot lock, is refused before
// any forge is read.
func openStore(base *slog.Logger, dir string) (*runstate.Store, bool) {
	store, err := runstate.Open(dir)
	if err == nil {
		return store, true
	}
	log := collect.Scope(base, forge.ProductUnknown, "")
	if errors.Is(err, runstate.ErrInUse) {
		log.Error(runstate.ErrInUse.Error(), "path", dir)
	} else {
		log.Error("run state directory unusable", "path", dir, "error", err)
	}
	return nil, false
}

func newCollector(ctx context.Context, cfg *config.Config, base *slog.Logger, store *runstate.Store) *collect.Collector {
	return collect.New(ctx, &collect.Deps{
		Open:         opener(base),
		Store:        store,
		Logger:       base,
		Connections:  cfg.Connections,
		Lookback:     cfg.Lookback,
		ScanInterval: cfg.ScanInterval,
		ScanLimit:    config.ScanLimit(cfg.ScanInterval),
	})
}

// opener opens one forge connection, and on GitHub its REST client for the
// routes forgeapi does not model, the two sharing one quota meter.
func opener(base *slog.Logger) collect.Opener {
	return func(ctx context.Context, c *config.Connection) (collect.Conn, collect.GitHubReader, error) {
		meter := ghquota.New(nil)
		conn, err := forgeconn.Open(ctx, c, base.With("connection", c.Name), meter)
		if err != nil {
			return nil, nil, err
		}
		if conn.Product() != forge.ProductGitHub {
			return conn, nil, nil
		}
		api := githubrest.APIBase(c.URL, c.APIURL)
		logger := collect.Scope(base, forge.ProductGitHub, c.Name)
		gh := githubrest.New(githubrest.HTTPClient(api, c.PrivateAddresses, c.AllowPlaintext, logger), api, c.Token, github.APIVersion,
			meter, logger)
		return conn, gh, nil
	}
}
