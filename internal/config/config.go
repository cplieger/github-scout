// Package config parses github-scout environment configuration.
//
// This daemon rejects external-scheduling sentinels because external scans cannot write to its log stream.
package config

import (
	"cmp"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cplieger/envx/v2"
	"github.com/cplieger/github-scout/internal/urlsafe"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/slogx"
)

// Defaults for environment-backed fields.
const (
	// DefaultScanInterval stays within GitHub's authenticated request budget.
	DefaultScanInterval = 15 * time.Minute
	// DefaultLookbackHours retains failures across a weekend.
	DefaultLookbackHours = 72
	// DefaultPRExclude removes Renovate PR noise.
	DefaultPRExclude = "-author:app/renovate"
	// DefaultIssueExclude removes Renovate and generated issue noise.
	DefaultIssueExclude = "-author:app/renovate -label:renovate -label:auto-generated"
	// DefaultCodeScanningExcludeForks excludes inherited upstream alerts.
	DefaultCodeScanningExcludeForks = true
	// maxScanInterval prevents a cadence too slow to be actionable.
	maxScanInterval = 365 * 24 * time.Hour
	// minScanInterval prevents quota exhaustion.
	minScanInterval = time.Minute
	// maxLookbackHours prevents unbounded API and deduplication cost.
	maxLookbackHours = 24 * 30
)

// Config is the effective runtime configuration.
type Config struct {
	// ExcludeRepos contains bare names excluded from every signal.
	ExcludeRepos map[string]bool
	// CodeScanningExcludeRepos excludes only code-scanning reads.
	CodeScanningExcludeRepos map[string]bool
	Token                    string
	Owner                    string
	PRExclude                string
	IssueExclude             string
	// ScanInterval is always positive for the health-probe deadline.
	ScanInterval time.Duration
	Lookback     time.Duration
	LogLevel     slog.Level
	// CodeScanningExcludeForks excludes inherited alerts without skipping other signals.
	CodeScanningExcludeForks bool
}

// Load reads configuration from the environment.
func Load() Config {
	rawLogLevel := os.Getenv("LOG_LEVEL")
	lvl, ok := slogx.ParseLevel(rawLogLevel, slog.LevelInfo)
	if !ok {
		slog.Warn("invalid LOG_LEVEL, using default", "value", rawLogLevel, "default", "info")
	}

	return Config{
		Token:                    strings.TrimSpace(os.Getenv("GITHUB_TOKEN")),
		Owner:                    strings.TrimSpace(os.Getenv("GITHUB_OWNER")),
		ExcludeRepos:             parseExcludes(os.Getenv("EXCLUDE_REPOS")),
		CodeScanningExcludeRepos: parseExcludes(os.Getenv("CODE_SCANNING_EXCLUDE_REPOS")),
		PRExclude:                cmp.Or(envx.String("PR_EXCLUDE_QUERY"), DefaultPRExclude),
		IssueExclude:             cmp.Or(envx.String("ISSUE_EXCLUDE_QUERY"), DefaultIssueExclude),
		ScanInterval:             ScanInterval(),
		Lookback:                 time.Duration(clampedInt("LOOKBACK_HOURS", DefaultLookbackHours, 1, maxLookbackHours)) * time.Hour,
		LogLevel:                 lvl,
		CodeScanningExcludeForks: envx.Bool("CODE_SCANNING_EXCLUDE_FORKS", DefaultCodeScanningExcludeForks),
	}
}

// ScanInterval returns the positive effective SCAN_INTERVAL.
func ScanInterval() time.Duration {
	return parseScanInterval(os.Getenv("SCAN_INTERVAL"))
}

// parseScanInterval falls back to the default for invalid or external modes.
func parseScanInterval(raw string) time.Duration {
	s := scheduler.ParseInterval(raw, DefaultScanInterval,
		scheduler.WithBounds(minScanInterval, maxScanInterval),
		scheduler.WithName("SCAN_INTERVAL"))
	if s.Mode == scheduler.ModeExternal {
		slog.Warn("invalid SCAN_INTERVAL, using default",
			"value", raw, "default", DefaultScanInterval.String())
		return DefaultScanInterval
	}
	return s.Interval
}

// Valid reports whether the owner and authenticated API access are configured.
func (c *Config) Valid() bool {
	return c.Owner != "" && c.Token != "" && urlsafe.IsSafeURLSegment(c.Owner)
}

// parseExcludes lowercases bare names for case-insensitive comparison only.
func parseExcludes(s string) map[string]bool {
	out := make(map[string]bool)
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[strings.ToLower(p)] = true
		}
	}
	return out
}

// clampedInt treats negative input as unset and floors zero to lo.
func clampedInt(key envx.Key, def, lo, hi int) int {
	v, ok, err := envx.IntStrict(key)
	if err != nil || !ok || v < 0 {
		return def
	}
	clamped := max(lo, min(v, hi))
	if clamped != v {
		slog.Warn("env value clamped", "key", key, "requested", v, "clamped_to", clamped)
	}
	return clamped
}
