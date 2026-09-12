// Package config parses github-scout environment configuration.
//
// This daemon rejects external-scheduling sentinels because external scans cannot write to its log stream.
//
// This package never logs: every non-fatal parse problem is returned as a
// Warning value for the caller to emit.
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

// Warning carries a non-fatal configuration message and its slog attributes
// for the caller to emit. This package never logs.
type Warning struct {
	Msg   string
	Attrs []slog.Attr
}

// Load reads configuration from the environment and returns the non-fatal
// warnings its parse produced.
func Load() (Config, []Warning) {
	rawLogLevel := os.Getenv("LOG_LEVEL")
	lvl, ok := slogx.ParseLevel(rawLogLevel, slog.LevelInfo)
	var warns []Warning
	if !ok {
		warns = append(warns, Warning{
			Msg:   "invalid LOG_LEVEL, using default",
			Attrs: []slog.Attr{slog.String("value", rawLogLevel), slog.String("default", "info")},
		})
	}
	scanInterval, scanWarns := ScanInterval()
	warns = append(warns, scanWarns...)
	lookback, lookbackWarns := clampedInt("LOOKBACK_HOURS", DefaultLookbackHours, 1, maxLookbackHours)
	warns = append(warns, lookbackWarns...)

	return Config{
		Token:                    strings.TrimSpace(os.Getenv("GITHUB_TOKEN")),
		Owner:                    strings.TrimSpace(os.Getenv("GITHUB_OWNER")),
		ExcludeRepos:             parseExcludes(os.Getenv("EXCLUDE_REPOS")),
		CodeScanningExcludeRepos: parseExcludes(os.Getenv("CODE_SCANNING_EXCLUDE_REPOS")),
		PRExclude:                cmp.Or(envx.String("PR_EXCLUDE_QUERY"), DefaultPRExclude),
		IssueExclude:             cmp.Or(envx.String("ISSUE_EXCLUDE_QUERY"), DefaultIssueExclude),
		ScanInterval:             scanInterval,
		Lookback:                 time.Duration(lookback) * time.Hour,
		LogLevel:                 lvl,
		CodeScanningExcludeForks: envx.Bool("CODE_SCANNING_EXCLUDE_FORKS", DefaultCodeScanningExcludeForks),
	}, warns
}

// ScanInterval returns the positive effective SCAN_INTERVAL plus the warnings
// its parse produced. Exported so the health subcommand can derive its probe
// max-age without loading the rest of the configuration; that caller discards
// the warnings so the frequent probe stays silent.
func ScanInterval() (time.Duration, []Warning) {
	return parseScanInterval(os.Getenv("SCAN_INTERVAL"))
}

// parseScanInterval falls back to the default for invalid or external modes.
func parseScanInterval(raw string) (time.Duration, []Warning) {
	s := scheduler.ParseInterval(raw, DefaultScanInterval,
		scheduler.WithBounds(minScanInterval, maxScanInterval),
		scheduler.WithName("SCAN_INTERVAL"))
	if s.Mode == scheduler.ModeExternal {
		return DefaultScanInterval, []Warning{{
			Msg:   "invalid SCAN_INTERVAL, using default",
			Attrs: []slog.Attr{slog.String("value", raw), slog.String("default", DefaultScanInterval.String())},
		}}
	}
	return s.Interval, nil
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

// clampedInt treats negative input as unset and floors zero to lo. A value the
// clamp moved is reported as a warning.
func clampedInt(key envx.Key, def, lo, hi int) (int, []Warning) {
	v, ok, err := envx.IntStrict(key)
	if err != nil || !ok || v < 0 {
		return def, nil
	}
	clamped := max(lo, min(v, hi))
	if clamped != v {
		return clamped, []Warning{{
			Msg:   "env value clamped",
			Attrs: []slog.Attr{slog.String("key", string(key)), slog.Int("requested", v), slog.Int("clamped_to", clamped)},
		}}
	}
	return clamped, nil
}
