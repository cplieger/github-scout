// Package config parses github-scout configuration from environment
// variables. The env var names (GITHUB_TOKEN, GITHUB_OWNER,
// POLL_INTERVAL_MINUTES, LOOKBACK_HOURS, LOG_LEVEL, EXCLUDE_REPOS) are an
// inviolate compose-file contract — the in-memory shape may evolve, but
// the names and parsing semantics must stay stable.
package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/github-scout/internal/urlsafe"
)

// Defaults for env-var-backed fields. Exported for test assertions.
const (
	// DefaultPollMinutes is the gap between scans. 15 minutes keeps the
	// "what just broke" latency low while staying far under GitHub's
	// authenticated 5000 req/hour budget (a few hundred calls per scan).
	DefaultPollMinutes = 15
	// DefaultLookbackHours bounds how far back a scan looks for failures.
	// 72h means a Friday-night failure is still surfaced on Monday. It
	// also bounds the in-memory dedup set and the per-repo API page count.
	DefaultLookbackHours = 72
	// maxPollMinutes guards against time.Duration overflow / nonsense
	// configuration (a year between scans defeats the purpose).
	maxPollMinutes = 60 * 24 * 365
	// maxLookbackHours caps the lookback window. 30 days is already far
	// past "actionable"; beyond it the dedup set and API cost grow without
	// surfacing anything a human would still act on.
	maxLookbackHours = 24 * 30
)

// Config is the effective runtime configuration after env var parsing.
type Config struct {
	// ExcludeRepos is a set of repo names (not full names) to skip. Used
	// to silence repos that legitimately fail or that the owner does not
	// want surfaced. Keyed by bare name for O(1) lookup.
	ExcludeRepos map[string]bool
	// Token is the GitHub PAT used for API auth. Never logged.
	Token string
	// Owner is the GitHub login (user or org) whose repos are scanned.
	Owner string
	// PollInterval is the gap between scans (0 = one-shot: scan once then
	// exit-idle, mirroring registry-stats' one-shot mode for debugging).
	PollInterval time.Duration
	// Lookback is how far back each scan considers runs.
	Lookback time.Duration
	// LogLevel is parsed from LOG_LEVEL.
	LogLevel slog.Level
}

// Load reads configuration from the environment with sensible defaults.
func Load() Config {
	return Config{
		Token:        os.Getenv("GITHUB_TOKEN"),
		Owner:        strings.TrimSpace(os.Getenv("GITHUB_OWNER")),
		ExcludeRepos: parseExcludes(os.Getenv("EXCLUDE_REPOS")),
		PollInterval: time.Duration(clampedInt("POLL_INTERVAL_MINUTES", DefaultPollMinutes, 0, maxPollMinutes)) * time.Minute,
		Lookback:     time.Duration(clampedInt("LOOKBACK_HOURS", DefaultLookbackHours, 1, maxLookbackHours)) * time.Hour,
		LogLevel:     parseLogLevel(os.Getenv("LOG_LEVEL")),
	}
}

// Valid reports whether the config has the minimum needed to run: an
// owner to scan and a token to authenticate. Unauthenticated GitHub API
// access is rate-limited to 60 req/hour, far too low for a fleet scan, so
// a missing token is treated as fatal misconfiguration rather than a
// degraded mode.
func (c Config) Valid() bool {
	return c.Owner != "" && c.Token != "" && urlsafe.IsSafeURLSegment(c.Owner)
}

// parseExcludes parses a comma-separated list of bare repo names to skip.
// Entries are trimmed; empties are dropped. Unsafe names are kept (they
// only ever compared, never used to build a URL) but trimmed.
func parseExcludes(s string) map[string]bool {
	out := make(map[string]bool)
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	return out
}

// clampedInt reads an integer env var and clamps it to [lo, hi]. On parse
// error or out-of-range it returns def (when below lo) or the clamp bound.
// A negative or non-numeric value falls back to def.
func clampedInt(key string, def, lo, hi int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return def
	}
	if v < lo {
		// 0 is meaningful for POLL_INTERVAL_MINUTES (one-shot), so lo=0
		// there; for LOOKBACK_HOURS lo=1 and a 0/negative falls back.
		if v < 0 {
			return def
		}
		return lo
	}
	if v > hi {
		slog.Warn("env value clamped", "key", key, "requested", v, "max", hi)
		return hi
	}
	return v
}

// parseLogLevel converts LOG_LEVEL to slog.Level (default Info).
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
