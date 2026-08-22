package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

func TestLoadDefaults(t *testing.T) {
	// No env set → defaults. t.Setenv guarantees a clean, restored env.
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_OWNER", "")
	t.Setenv("SCAN_INTERVAL", "")
	t.Setenv("LOOKBACK_HOURS", "")
	t.Setenv("EXCLUDE_REPOS", "")
	t.Setenv("CODE_SCANNING_EXCLUDE_REPOS", "")
	t.Setenv("LOG_LEVEL", "")

	cfg := Load()

	// Expected values are written out rather than read back from the
	// constants they check: an assertion against DefaultScanInterval moves
	// with any edit to it and so pins nothing. 15m and 72h are the cadence
	// and window the compose contract and the bundled dashboard assume.
	if cfg.ScanInterval != 15*time.Minute {
		t.Errorf("ScanInterval = %v, want 15m0s", cfg.ScanInterval)
	}
	if cfg.Lookback != 72*time.Hour {
		t.Errorf("Lookback = %v, want 72h0m0s", cfg.Lookback)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want Info", cfg.LogLevel)
	}
	if len(cfg.ExcludeRepos) != 0 {
		t.Errorf("ExcludeRepos = %v, want empty", cfg.ExcludeRepos)
	}
	if len(cfg.CodeScanningExcludeRepos) != 0 {
		t.Errorf("CodeScanningExcludeRepos = %v, want empty", cfg.CodeScanningExcludeRepos)
	}
}

func TestLoadParsesValues(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	t.Setenv("GITHUB_OWNER", "cplieger")
	t.Setenv("SCAN_INTERVAL", "30m")
	t.Setenv("LOOKBACK_HOURS", "48")
	t.Setenv("EXCLUDE_REPOS", "noisy-repo, other ,")
	t.Setenv("LOG_LEVEL", "debug")

	cfg := Load()
	if cfg.Token != "ghp_secret" {
		t.Errorf("Token not parsed")
	}
	if cfg.Owner != "cplieger" {
		t.Errorf("Owner = %q, want cplieger", cfg.Owner)
	}
	if cfg.ScanInterval != 30*time.Minute {
		t.Errorf("ScanInterval = %v, want 30m", cfg.ScanInterval)
	}
	if cfg.Lookback != 48*time.Hour {
		t.Errorf("Lookback = %v, want 48h", cfg.Lookback)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want Debug", cfg.LogLevel)
	}
	if !cfg.ExcludeRepos["noisy-repo"] || !cfg.ExcludeRepos["other"] {
		t.Errorf("ExcludeRepos = %v, want noisy-repo+other", cfg.ExcludeRepos)
	}
	if cfg.ExcludeRepos[""] {
		t.Errorf("empty exclude entry should be dropped")
	}
}

// TestScanIntervalSentinelsFallBackToDefault pins the no-external-mode
// contract: the fleet's external-scheduling sentinels (off / disabled / 0)
// are not valid values for this app — its stdout is the product, so scans
// never run outside the daemon — and they get the standard invalid-input
// treatment: exactly one warning, default cadence. No zero ever escapes
// config, so the daemon needs no interval guard and the health probe's
// freshness deadline is always armed.
func TestScanIntervalSentinelsFallBackToDefault(t *testing.T) {
	for _, v := range []string{"off", "disabled", "0", "0s", "OFF"} {
		t.Run(v, func(t *testing.T) {
			rec := captureDefaultSlog(t)
			t.Setenv("SCAN_INTERVAL", v)
			if got := Load().ScanInterval; got != 15*time.Minute {
				t.Errorf("SCAN_INTERVAL=%q ScanInterval = %v, want default 15m0s", v, got)
			}
			if n := rec.CountExact("invalid SCAN_INTERVAL, using default"); n != 1 {
				t.Errorf("SCAN_INTERVAL=%q warned %d times, want exactly 1", v, n)
			}
		})
	}
}

func TestScanIntervalParsesDuration(t *testing.T) {
	t.Setenv("SCAN_INTERVAL", "1h30m")
	if got := Load().ScanInterval; got != 90*time.Minute {
		t.Errorf("ScanInterval = %v, want 1h30m", got)
	}
}

// TestClampingAndFallbacks pins every out-of-range and unparseable input to the
// duration it actually yields. Each want is a literal, never the constant the
// clamp reads: a want written as `maxScanInterval` holds however that constant
// is edited, so the bound it claims to check is unpinned.
func TestClampingAndFallbacks(t *testing.T) {
	tests := []struct {
		selector func(Config) time.Duration
		name     string
		key      string
		val      string
		want     time.Duration
	}{
		{name: "scan negative falls back to default", key: "SCAN_INTERVAL", val: "-5m", want: 15 * time.Minute, selector: func(c Config) time.Duration { return c.ScanInterval }},
		{name: "scan garbage falls back to default", key: "SCAN_INTERVAL", val: "abc", want: 15 * time.Minute, selector: func(c Config) time.Duration { return c.ScanInterval }},
		{name: "scan over max is clamped", key: "SCAN_INTERVAL", val: "10000h", want: 8760 * time.Hour, selector: func(c Config) time.Duration { return c.ScanInterval }}, // 365 days
		{name: "lookback zero floors to lo=1", key: "LOOKBACK_HOURS", val: "0", want: 1 * time.Hour, selector: func(c Config) time.Duration { return c.Lookback }},
		{name: "lookback at lo boundary is kept", key: "LOOKBACK_HOURS", val: "1", want: 1 * time.Hour, selector: func(c Config) time.Duration { return c.Lookback }},
		{name: "lookback negative falls back to default", key: "LOOKBACK_HOURS", val: "-1", want: 72 * time.Hour, selector: func(c Config) time.Duration { return c.Lookback }},
		{name: "lookback at hi boundary is kept", key: "LOOKBACK_HOURS", val: "720", want: 720 * time.Hour, selector: func(c Config) time.Duration { return c.Lookback }}, // 30 days
		{name: "lookback over max is clamped", key: "LOOKBACK_HOURS", val: "100000", want: 720 * time.Hour, selector: func(c Config) time.Duration { return c.Lookback }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.val)
			if got := tt.selector(Load()); got != tt.want {
				t.Errorf("%s = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

// TestClampedIntWarnsOverMax pins the over-max warning side effect. The clamp
// uses min/max builtins (no boundary operators to mutate), so the only
// remaining conditional is `clamped != v`, which gates this warning. Asserting
// the warning fires when (and only when) the value is clamped down makes that
// guard's mutants (==, removal) killable — the return-value table tests alone
// cannot see a log-only branch.
func TestClampedIntWarnsOverMax(t *testing.T) {
	capture := func(val string) string {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		defer slog.SetDefault(prev)
		t.Setenv("LOOKBACK_HOURS", val)
		_ = Load()
		return buf.String()
	}

	if out := capture("100000"); !strings.Contains(out, "env value clamped") {
		t.Errorf("over-max value should warn; log = %q", out)
	}
	// In-range and at-boundary values must NOT warn (kills the negated guard).
	for _, v := range []string{"48", "720", "1"} {
		if out := capture(v); strings.Contains(out, "env value clamped") {
			t.Errorf("LOOKBACK_HOURS=%s should not warn; log = %q", v, out)
		}
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		name  string
		owner string
		token string
		want  bool
	}{
		{"complete", "cplieger", "tok", true},
		{"no owner", "", "tok", false},
		{"no token", "cplieger", "", false},
		{"unsafe owner", "../etc", "tok", false},
		{"neither", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{Owner: tt.owner, Token: tt.token}
			if got := c.Valid(); got != tt.want {
				t.Errorf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExcludeQueriesDefaultWhenUnset(t *testing.T) {
	// Unset (empty) PR/issue exclude vars fall back to the defaults.
	t.Setenv("PR_EXCLUDE_QUERY", "")
	t.Setenv("ISSUE_EXCLUDE_QUERY", "")

	cfg := Load()

	const (
		wantPR    = "-author:app/renovate"
		wantIssue = "-author:app/renovate -label:renovate -label:auto-generated"
	)
	if cfg.PRExclude != wantPR {
		t.Errorf("PRExclude = %q, want default %q", cfg.PRExclude, wantPR)
	}
	if cfg.IssueExclude != wantIssue {
		t.Errorf("IssueExclude = %q, want default %q", cfg.IssueExclude, wantIssue)
	}
}

func TestExcludeQueriesOverriddenByEnv(t *testing.T) {
	// A non-empty value is used verbatim instead of the default (exercises
	// envx.String's "env set" branch).
	t.Setenv("PR_EXCLUDE_QUERY", "-author:dependabot")
	t.Setenv("ISSUE_EXCLUDE_QUERY", "-label:wontfix")

	cfg := Load()

	if cfg.PRExclude != "-author:dependabot" {
		t.Errorf("PRExclude = %q, want -author:dependabot", cfg.PRExclude)
	}
	if cfg.IssueExclude != "-label:wontfix" {
		t.Errorf("IssueExclude = %q, want -label:wontfix", cfg.IssueExclude)
	}
}

func TestParseLogLevels(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"nonsense", slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", tt.in)
			if got := Load().LogLevel; got != tt.want {
				t.Errorf("LOG_LEVEL=%q LogLevel = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestLookbackAtMaxIsAcceptedWithoutWarning(t *testing.T) {
	// LOOKBACK_HOURS exactly at the maximum is accepted as-is and must NOT
	// emit a "clamped" warning. This pins the clamp boundary at v > hi
	// (a v >= hi mutant would warn spuriously at the legal maximum).
	rec := captureDefaultSlog(t)

	t.Setenv("LOOKBACK_HOURS", "720") // the maximum, 30 days
	cfg := Load()

	if cfg.Lookback != 720*time.Hour {
		t.Errorf("Lookback = %v, want 720h0m0s (max accepted as-is)", cfg.Lookback)
	}
	if n := rec.CountExact("env value clamped"); n != 0 {
		t.Errorf("value at exactly the max should not warn; got %d clamp warnings", n)
	}
}

func TestLookbackAboveMaxIsClampedWithWarning(t *testing.T) {
	// One hour over the maximum is clamped down and warns exactly once —
	// the positive control for the boundary pinned above.
	rec := captureDefaultSlog(t)

	t.Setenv("LOOKBACK_HOURS", "721")
	cfg := Load()

	if cfg.Lookback != 720*time.Hour {
		t.Errorf("Lookback = %v, want clamped to 720h0m0s", cfg.Lookback)
	}
	if n := rec.CountExact("env value clamped"); n != 1 {
		t.Errorf("value over the max should warn once; got %d clamp warnings", n)
	}
}

// captureDefaultSlog redirects the global slog logger (which config's clamp
// and parse warnings target) to a shared capture.Recorder for the duration of
// the test, restoring the previous default on cleanup (capture.Default).
// Assertions use CountExact — the exact-match semantics the former hand-rolled
// countingHandler had.
func captureDefaultSlog(t *testing.T) *capture.Recorder {
	t.Helper()
	return capture.Default(t)
}

// TestScanIntervalBelowMinimumClamped pins the minScanInterval floor: a positive
// sub-minute SCAN_INTERVAL is clamped up to 1m so a too-frequent scan of a
// multi-repo account can't exhaust GitHub's 5000 req/hour budget. The
// existing TestClampingAndFallbacks covers negative/garbage/over-max but no
// sub-minute case. The "exactly 1m" row is kept via the default branch, pinning
// the strict `<` lower edge; "2m" confirms an above-floor value passes through.
// The floor is written as a literal, not as minScanInterval, so the row states
// what the floor IS rather than restating whatever it has become.
func TestScanIntervalBelowMinimumClamped(t *testing.T) {
	tests := []struct {
		name string
		val  string
		want time.Duration
	}{
		{"sub-minute clamps up to the floor", "30s", time.Minute},
		{"one second below the floor clamps", "59s", time.Minute},
		{"exactly at the floor is kept", "1m", time.Minute},
		{"above the floor is kept", "2m", 2 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SCAN_INTERVAL", tt.val)
			if got := Load().ScanInterval; got != tt.want {
				t.Errorf("SCAN_INTERVAL=%q ScanInterval = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}

// TestExcludeReposCaseInsensitive pins parseExcludes' lowercasing: a mixed-case
// EXCLUDE_REPOS entry is stored under its lowercase key so the collector's
// (also-lowercased) lookups match it. GitHub repo names are case-insensitive.
func TestExcludeReposCaseInsensitive(t *testing.T) {
	t.Setenv("EXCLUDE_REPOS", "Noisy-Repo, OTHER")
	cfg := Load()
	if !cfg.ExcludeRepos["noisy-repo"] {
		t.Errorf("ExcludeRepos missing lowercased key noisy-repo: %v", cfg.ExcludeRepos)
	}
	if !cfg.ExcludeRepos["other"] {
		t.Errorf("ExcludeRepos missing lowercased key other: %v", cfg.ExcludeRepos)
	}
}

// TestLoadParsesCodeScanningExcludes pins CODE_SCANNING_EXCLUDE_REPOS parsing:
// it is a SEPARATE set from EXCLUDE_REPOS, trimmed and lowercased the same way,
// so a private repo without GitHub Advanced Security can be silenced for code
// scanning only (its runs / PRs / issues stay scanned). Mixed case and a
// trailing empty entry confirm the shared parseExcludes normalization.
func TestLoadParsesCodeScanningExcludes(t *testing.T) {
	t.Setenv("EXCLUDE_REPOS", "")
	t.Setenv("CODE_SCANNING_EXCLUDE_REPOS", ".config, MyRepo ,")
	cfg := Load()
	if !cfg.CodeScanningExcludeRepos[".config"] || !cfg.CodeScanningExcludeRepos["myrepo"] {
		t.Errorf("CodeScanningExcludeRepos = %v, want .config+myrepo (lowercased)", cfg.CodeScanningExcludeRepos)
	}
	if cfg.CodeScanningExcludeRepos[""] {
		t.Errorf("empty exclude entry should be dropped")
	}
	if len(cfg.ExcludeRepos) != 0 {
		t.Errorf("EXCLUDE_REPOS must stay independent of CODE_SCANNING_EXCLUDE_REPOS; got %v", cfg.ExcludeRepos)
	}
}

// TestCodeScanningExcludeForks pins the CODE_SCANNING_EXCLUDE_FORKS contract in
// both directions plus its default. The DEFAULT is the load-bearing case: it is
// true, so an operator who never sets the var gets forks skipped, and a
// regression flipping it would silently readmit hundreds of upstream alerts.
func TestCodeScanningExcludeForks(t *testing.T) {
	for _, tc := range []struct {
		desc string
		set  bool
		raw  string
		want bool
	}{
		{desc: "unset defaults to exclude", set: false, want: DefaultCodeScanningExcludeForks},
		{desc: "empty is treated as unset", set: true, raw: "", want: DefaultCodeScanningExcludeForks},
		{desc: "false includes forks", set: true, raw: "false", want: false},
		{desc: "true excludes forks", set: true, raw: "true", want: true},
		{desc: "envx accepts 0", set: true, raw: "0", want: false},
		{desc: "unparseable falls back to the default", set: true, raw: "ture", want: DefaultCodeScanningExcludeForks},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if tc.set {
				t.Setenv("CODE_SCANNING_EXCLUDE_FORKS", tc.raw)
			}
			if got := Load().CodeScanningExcludeForks; got != tc.want {
				t.Errorf("Load().CodeScanningExcludeForks with CODE_SCANNING_EXCLUDE_FORKS=%q (set=%v) = %v, want %v",
					tc.raw, tc.set, got, tc.want)
			}
		})
	}
	if !DefaultCodeScanningExcludeForks {
		t.Error("DefaultCodeScanningExcludeForks = false, want true: a fork's alerts are the upstream project's, so the safe default is to skip them")
	}
}
