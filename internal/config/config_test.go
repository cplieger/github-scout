package config

import (
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
