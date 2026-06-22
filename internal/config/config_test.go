package config

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	// No env set → defaults. t.Setenv guarantees a clean, restored env.
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_OWNER", "")
	t.Setenv("SCAN_INTERVAL", "")
	t.Setenv("LOOKBACK_HOURS", "")
	t.Setenv("EXCLUDE_REPOS", "")
	t.Setenv("LOG_LEVEL", "")

	cfg := Load()
	if cfg.ScanInterval != DefaultScanInterval {
		t.Errorf("ScanInterval = %v, want %v", cfg.ScanInterval, DefaultScanInterval)
	}
	if cfg.Lookback != DefaultLookbackHours*time.Hour {
		t.Errorf("Lookback = %v, want %v", cfg.Lookback, DefaultLookbackHours*time.Hour)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want Info", cfg.LogLevel)
	}
	if len(cfg.ExcludeRepos) != 0 {
		t.Errorf("ExcludeRepos = %v, want empty", cfg.ExcludeRepos)
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

func TestScanIntervalSentinelsAreResidentIdle(t *testing.T) {
	for _, v := range []string{"off", "disabled", "0", "0s", "OFF"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("SCAN_INTERVAL", v)
			if got := Load().ScanInterval; got != 0 {
				t.Errorf("SCAN_INTERVAL=%q ScanInterval = %v, want 0 (resident-idle)", v, got)
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

func TestClampingAndFallbacks(t *testing.T) {
	tests := []struct {
		selector func(Config) time.Duration
		name     string
		key      string
		val      string
		want     time.Duration
	}{
		{name: "scan negative falls back to default", key: "SCAN_INTERVAL", val: "-5m", want: DefaultScanInterval, selector: func(c Config) time.Duration { return c.ScanInterval }},
		{name: "scan garbage falls back to default", key: "SCAN_INTERVAL", val: "abc", want: DefaultScanInterval, selector: func(c Config) time.Duration { return c.ScanInterval }},
		{name: "scan over max is clamped", key: "SCAN_INTERVAL", val: "10000h", want: maxScanInterval, selector: func(c Config) time.Duration { return c.ScanInterval }},
		{name: "lookback zero floors to lo=1", key: "LOOKBACK_HOURS", val: "0", want: 1 * time.Hour, selector: func(c Config) time.Duration { return c.Lookback }},
		{name: "lookback negative falls back to default", key: "LOOKBACK_HOURS", val: "-1", want: DefaultLookbackHours * time.Hour, selector: func(c Config) time.Duration { return c.Lookback }},
		{name: "lookback over max is clamped", key: "LOOKBACK_HOURS", val: "100000", want: maxLookbackHours * time.Hour, selector: func(c Config) time.Duration { return c.Lookback }},
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

	if cfg.PRExclude != DefaultPRExclude {
		t.Errorf("PRExclude = %q, want default %q", cfg.PRExclude, DefaultPRExclude)
	}
	if cfg.IssueExclude != DefaultIssueExclude {
		t.Errorf("IssueExclude = %q, want default %q", cfg.IssueExclude, DefaultIssueExclude)
	}
}

func TestExcludeQueriesOverriddenByEnv(t *testing.T) {
	// A non-empty value is used verbatim instead of the default (exercises
	// GetEnv's "env set" branch).
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

	t.Setenv("LOOKBACK_HOURS", "720") // == maxLookbackHours
	cfg := Load()

	if cfg.Lookback != maxLookbackHours*time.Hour {
		t.Errorf("Lookback = %v, want %v (max accepted as-is)", cfg.Lookback, maxLookbackHours*time.Hour)
	}
	if n := rec.count("env value clamped"); n != 0 {
		t.Errorf("value at exactly the max should not warn; got %d clamp warnings", n)
	}
}

func TestLookbackAboveMaxIsClampedWithWarning(t *testing.T) {
	// One hour over the maximum is clamped down and warns exactly once —
	// the positive control for the boundary pinned above.
	rec := captureDefaultSlog(t)

	t.Setenv("LOOKBACK_HOURS", "721")
	cfg := Load()

	if cfg.Lookback != maxLookbackHours*time.Hour {
		t.Errorf("Lookback = %v, want clamped to %v", cfg.Lookback, maxLookbackHours*time.Hour)
	}
	if n := rec.count("env value clamped"); n != 1 {
		t.Errorf("value over the max should warn once; got %d clamp warnings", n)
	}
}

// captureDefaultSlog redirects the global slog logger (which config's clamp
// and parse warnings target) to a recording handler for the duration of the
// test, restoring the previous default on cleanup.
func captureDefaultSlog(t *testing.T) *countingHandler {
	t.Helper()
	rec := &countingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rec
}

// countingHandler records log messages so a test can assert on warnings the
// config package emits through the global slog logger.
type countingHandler struct{ msgs []string }

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) count(msg string) int {
	n := 0
	for _, m := range h.msgs {
		if m == msg {
			n++
		}
	}
	return n
}
