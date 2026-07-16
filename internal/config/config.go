// Package config parses github-scout configuration from environment
// variables. The env var names (GITHUB_TOKEN, GITHUB_OWNER, SCAN_INTERVAL,
// LOOKBACK_HOURS, LOG_LEVEL, EXCLUDE_REPOS, CODE_SCANNING_EXCLUDE_REPOS,
// PR_EXCLUDE_QUERY, ISSUE_EXCLUDE_QUERY) are an inviolate compose-file
// contract — the in-memory shape may evolve, but the names and parsing
// semantics must stay stable.
//
// SCAN_INTERVAL follows the shared scheduled-app convention (DUMP_INTERVAL,
// SCHEDULE_INTERVAL, SYNC_INTERVAL, …): a Go duration string, with the
// sentinels off / disabled / 0 / 0s selecting resident-idle mode (no
// internal timer; scans are driven externally by `github-scout trigger`,
// e.g. an Ofelia job-exec).
package config

import (
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cplieger/envx"
	"github.com/cplieger/github-scout/internal/urlsafe"
	"github.com/cplieger/scheduler"
	"github.com/cplieger/slogx"
)

// Defaults for env-var-backed fields.
const (
	// DefaultScanInterval is the gap between scans in scheduled mode. 15
	// minutes keeps the "what just broke" latency low while staying far
	// under GitHub's authenticated 5000 req/hour budget (a few hundred
	// calls per scan).
	DefaultScanInterval = 15 * time.Minute
	// DefaultLookbackHours bounds how far back a scan looks for failures.
	// 72h means a Friday-night failure is still surfaced on Monday. It
	// also bounds the in-memory dedup set and the per-repo API page count.
	DefaultLookbackHours = 72
	// DefaultPRExclude filters Renovate's PRs out of the open-PR signal.
	// Renovate PRs are high-volume bot noise, not "needs a human" work.
	DefaultPRExclude = "-author:app/renovate"
	// DefaultIssueExclude filters Renovate "Dependency Dashboard" issues
	// (authored by the repo owner but carrying the `renovate` label) and
	// auto-generated trackers (gremlins mutation-testing issues carry the
	// `auto-generated` label) out of the open-issue signal.
	DefaultIssueExclude = "-author:app/renovate -label:renovate -label:auto-generated"
	// maxScanInterval guards against nonsense configuration (a year between
	// scans defeats the purpose).
	maxScanInterval = 365 * 24 * time.Hour
	// minScanInterval floors a too-frequent interval: a sub-minute scan of a
	// multi-repo account exhausts GitHub's 5000 req/hour budget and thrashes.
	minScanInterval = 1 * time.Minute
	// maxLookbackHours caps the lookback window. 30 days is already far
	// past "actionable"; beyond it the dedup set and API cost grow without
	// surfacing anything a human would still act on.
	maxLookbackHours = 24 * 30
)

// Config is the effective runtime configuration after env var parsing.
type Config struct {
	// ExcludeRepos is a set of repo names (not full names) to skip across
	// all signals. Used to silence repos that legitimately fail or that the
	// owner does not want surfaced. Keyed by bare name for O(1) lookup.
	ExcludeRepos map[string]bool
	// CodeScanningExcludeRepos is a set of repo names (not full names) to skip
	// for the code-scanning signal ONLY, while still scanning them for runs,
	// PRs, and issues. Use it for repos whose code-scanning API always fails
	// expectedly — a private repo on a plan without GitHub Advanced Security
	// 403s every scan — so that expected failure stops marking every scan
	// degraded, without dropping the repo's other signals (which EXCLUDE_REPOS
	// would). Keyed by bare name for O(1) lookup.
	CodeScanningExcludeRepos map[string]bool
	// Token is the GitHub PAT used for API auth. Never logged.
	Token string
	// Owner is the GitHub login (user or org) whose repos are scanned.
	Owner string
	// PRExclude is appended to the open-PR search query to filter bot noise.
	PRExclude string
	// IssueExclude is appended to the open-issue search query to filter
	// bot / auto-generated noise.
	IssueExclude string
	// ScanInterval is the gap between scans in scheduled mode. Zero selects
	// resident-idle mode: no internal timer, scans driven externally by the
	// `trigger` subcommand (Ofelia job-exec). Parsed from SCAN_INTERVAL.
	ScanInterval time.Duration
	// Lookback is how far back each scan considers runs.
	Lookback time.Duration
	// LogLevel is parsed from LOG_LEVEL.
	LogLevel slog.Level
}

// Load reads configuration from the environment with sensible defaults.
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
		PRExclude:                envx.String("PR_EXCLUDE_QUERY", DefaultPRExclude),
		IssueExclude:             envx.String("ISSUE_EXCLUDE_QUERY", DefaultIssueExclude),
		ScanInterval:             parseScanInterval(os.Getenv("SCAN_INTERVAL")),
		Lookback:                 time.Duration(clampedInt("LOOKBACK_HOURS", DefaultLookbackHours, 1, maxLookbackHours)) * time.Hour,
		LogLevel:                 lvl,
	}
}

// parseScanInterval parses SCAN_INTERVAL into the gap between scans. It
// delegates to scheduler.ParseInterval (with WithBounds clamping a built-in
// cadence to [minScanInterval, maxScanInterval]): a Go duration (e.g. "15m",
// "1h30m") runs built-in; the sentinels off / disabled / 0 / 0s select
// resident-idle mode (return 0: no internal timer, scans driven by the
// `trigger` subcommand); an unset value uses the default; and an invalid or
// negative value warns and falls back to the default so a typo degrades to
// "still scanning" rather than silent idle.
func parseScanInterval(raw string) time.Duration {
	s := scheduler.ParseInterval(raw, DefaultScanInterval,
		scheduler.WithBounds(minScanInterval, maxScanInterval),
		scheduler.WithName("SCAN_INTERVAL"))
	if s.Mode == scheduler.ModeExternal {
		return 0
	}
	return s.Interval
}

// Valid reports whether the config has the minimum needed to run: an owner
// to scan and a token to authenticate. Unauthenticated GitHub API access is
// rate-limited to 60 req/hour, far too low for a multi-repo scan, so a missing
// token is fatal misconfiguration rather than a degraded mode. Pointer
// receiver: Config is large enough that copying it per call is wasteful.
func (c *Config) Valid() bool {
	return c.Owner != "" && c.Token != "" && urlsafe.IsSafeURLSegment(c.Owner)
}

// parseExcludes parses a comma-separated list of bare repo names to skip.
// Entries are trimmed and lowercased; empties are dropped. Matching is
// case-insensitive — the collect-side lookup keys are lowercased to match —
// mirroring keep()'s case-insensitive owner test (GitHub repo names are
// themselves case-insensitive). Unsafe names are kept (they are only ever
// compared, never used to build a URL) but trimmed.
func parseExcludes(s string) map[string]bool {
	out := make(map[string]bool)
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[strings.ToLower(p)] = true
		}
	}
	return out
}

// clampedInt reads an integer env var and clamps it into [lo, hi]. A
// non-numeric or negative value is treated as "unset" and falls back to def.
// A non-negative value below lo is floored to lo; a value above hi is capped
// to hi (and logged). Used for LOOKBACK_HOURS (def=72, lo=1, hi=720).
//
// The negative-vs-zero split is deliberate: LOOKBACK_HOURS=0 is read as "the
// smallest useful window" (floor to lo=1), whereas a negative value is
// nonsense input that should restore the default.
//
// The clamp itself uses the min/max builtins rather than `if v < lo` / `if v
// > hi` comparisons. A comparison-based clamp has an unkillable
// CONDITIONALS_BOUNDARY mutant at each bound (at v==lo, both `v < lo` and `v
// <= lo` yield lo; likewise at v==hi) — the classic equivalent mutant.
// Expressing the clamp as max(lo, min(v, hi)) removes those operators
// entirely. The one remaining comparison (clamped != v, gating the
// over-max warning) is exercised by TestClampedIntWarnsOverMax, which
// captures the slog output — so its boundary mutant is killable too.
func clampedInt(key string, def, lo, hi int) int {
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
