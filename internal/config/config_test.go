package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/runstate"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/slogx/capture"
)

// writeConfig writes body as a config file in a fresh directory and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("Setup: write config: %v", err)
	}
	return path
}

const minimal = `
connections:
  - name: github
    url: https://github.com
    token: ${GITHUB_TOKEN}
    owners: [example]
`

func TestLoad_minimal_file_takes_the_defaults(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	cfg, warns, err := Load(writeConfig(t, minimal))
	if err != nil {
		t.Fatalf("Load(minimal) = error %v, want nil", err)
	}
	if len(warns) != 0 {
		t.Errorf("Load(minimal) warnings = %v, want none", warns)
	}
	if cfg.ScanInterval != 15*time.Minute || cfg.Lookback != 72*time.Hour || cfg.LogLevel != slog.LevelInfo {
		t.Errorf("Load(minimal) = interval %v lookback %v level %v, want 15m0s 72h0m0s INFO", cfg.ScanInterval, cfg.Lookback, cfg.LogLevel)
	}
	if len(cfg.Connections) != 1 {
		t.Fatalf("Load(minimal) connections = %d, want 1", len(cfg.Connections))
	}
	c := cfg.Connections[0]
	if c.Name != "github" || c.URL != "https://github.com" || c.Token != "test-token" {
		t.Errorf("connection = name %q url %q token-set %t, want github https://github.com and the expanded token", c.Name, c.URL, c.Token == "test-token")
	}
	if len(c.Owners) != 1 || c.Owners[0] != "example" {
		t.Errorf("Owners = %v, want [example]", c.Owners)
	}
	if !c.Security.Enabled || !c.Security.SkipForks || c.Security.IncludePrivate || c.SecuritySet {
		t.Errorf("Security = %+v set %t, want enabled, skip_forks, private repositories skipped, not set explicitly", c.Security, c.SecuritySet)
	}
	if c.PrivateAddresses || c.AllowPlaintext {
		t.Errorf("PrivateAddresses %t AllowPlaintext %t, want both false", c.PrivateAddresses, c.AllowPlaintext)
	}
}

func TestLoad_keeps_the_token_bytes_verbatim(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "  exact token bytes\t")
	cfg, _, err := Load(writeConfig(t, minimal))
	if err != nil {
		t.Fatalf("Load(token with edge whitespace) = error %v, want nil", err)
	}
	if got := cfg.Connections[0].Token; got != "  exact token bytes\t" {
		t.Errorf("Token = %q, want the environment value byte for byte", got)
	}
	t.Setenv("GITHUB_TOKEN", " \t ")
	if _, _, err := Load(writeConfig(t, minimal)); err == nil || !strings.Contains(err.Error(), "token references GITHUB_TOKEN, which is empty") {
		t.Errorf("Load(blank token) = %v, want token references GITHUB_TOKEN, which is empty", err)
	}
}

func TestLoad_parses_every_key(t *testing.T) {
	t.Setenv("FORGE_SCOUT_GITEA_TOKEN", "gitea-token")
	t.Setenv("GITHUB_TOKEN", "test-token")
	body := `
scan_interval: 30m
lookback: 48h
log_level: debug
connections:
  - name: github
    url: https://github.com
    token: ${GITHUB_TOKEN}
    owners: [example, example-org]
    exclude_repos: [Example/Noisy]
    exclude_authors: [Renovate, "renovate[bot]"]
    exclude_labels: [Renovate]
    bot_authors: [My-Bot]
    security: { enabled: false, skip_forks: false, include_private: true, skip_repos: [example/private] }
  - name: home_gitea
    url: http://git.example.test:3000
    api_url: http://git.example.test:3000
    token: ${FORGE_SCOUT_GITEA_TOKEN}
    owners: [me]
    private_addresses: true
    allow_plaintext: true
`
	cfg, warns, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load = error %v, want nil", err)
	}
	if len(warns) != 0 {
		t.Errorf("Load warnings = %v, want none", warns)
	}
	if cfg.ScanInterval != 30*time.Minute || cfg.Lookback != 48*time.Hour || cfg.LogLevel != slog.LevelDebug {
		t.Errorf("Load = interval %v lookback %v level %v, want 30m0s 48h0m0s DEBUG", cfg.ScanInterval, cfg.Lookback, cfg.LogLevel)
	}
	if len(cfg.Connections) != 2 {
		t.Fatalf("connections = %d, want 2", len(cfg.Connections))
	}
	gh, gt := cfg.Connections[0], cfg.Connections[1]
	if !gh.ExcludeRepos["example/noisy"] || !gh.ExcludeAuthors["renovate"] || !gh.ExcludeAuthors["renovate[bot]"] ||
		!gh.ExcludeLabels["renovate"] || !gh.BotAuthors["my-bot"] {
		t.Errorf("github excludes = repos %v authors %v labels %v bots %v, want each lowercased", gh.ExcludeRepos, gh.ExcludeAuthors, gh.ExcludeLabels, gh.BotAuthors)
	}
	if gh.Security.Enabled || gh.Security.SkipForks || !gh.Security.IncludePrivate || !gh.Security.SkipRepos["example/private"] || !gh.SecuritySet {
		t.Errorf("github Security = %+v set %t, want disabled, forks and private repositories read, example/private skipped, set explicitly", gh.Security, gh.SecuritySet)
	}
	if gt.Token != "gitea-token" || !gt.PrivateAddresses || !gt.AllowPlaintext || gt.APIURL != "http://git.example.test:3000" {
		t.Errorf("gitea connection = token-set %t private %t plaintext %t api %q, want the expanded token, both flags, the api url", gt.Token == "gitea-token", gt.PrivateAddresses, gt.AllowPlaintext, gt.APIURL)
	}
}

func TestLoad_refuses_invalid_files(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	tests := []struct {
		name string
		body string
		want string
	}{
		{"no_connections", "scan_interval: 15m\n", "at least one connection"},
		{"unknown_key", "connections:\n  - name: a\n    url: https://github.com\n    token: x\n    owners: [o]\n    product: forgejo\n", "product"},
		{"unknown_top_key", "github_owner: me\n" + minimal, "github_owner"},
		{"two_documents", minimal + "---\n" + minimal, "document"},
		{"missing_name", "connections:\n  - url: https://github.com\n    token: x\n    owners: [o]\n", "name"},
		{"bad_name", "connections:\n  - name: GitHub!\n    url: https://github.com\n    token: x\n    owners: [o]\n", "name"},
		{"duplicate_name", "connections:\n  - name: a\n    url: https://github.com\n    token: x\n    owners: [o]\n  - name: a\n    url: https://gitlab.com\n    token: y\n    owners: [o]\n", "twice"},
		{"missing_url", "connections:\n  - name: a\n    token: x\n    owners: [o]\n", "url"},
		{"bad_scheme", "connections:\n  - name: a\n    url: ftp://example.test\n    token: x\n    owners: [o]\n", "url"},
		{"http_without_plaintext", "connections:\n  - name: a\n    url: http://example.test\n    token: x\n    owners: [o]\n", "allow_plaintext"},
		{"bad_api_url", "connections:\n  - name: a\n    url: https://example.test\n    api_url: example.test/api\n    token: x\n    owners: [o]\n", "api_url"},
		{"hostless_url", "connections:\n  - name: a\n    url: https://:443\n    token: x\n    owners: [o]\n", "url"},
		{"out_of_range_port", "connections:\n  - name: a\n    url: https://example.test:99999\n    token: x\n    owners: [o]\n", "url"},
		{"zero_port", "connections:\n  - name: a\n    url: https://example.test:0\n    token: x\n    owners: [o]\n", "url"},
		{"hostless_api_url", "connections:\n  - name: a\n    url: https://example.test\n    api_url: https://:8443/api\n    token: x\n    owners: [o]\n", "api_url"},
		{"out_of_range_api_port", "connections:\n  - name: a\n    url: https://example.test\n    api_url: https://example.test:65536/api\n    token: x\n    owners: [o]\n", "api_url"},
		{"missing_token", "connections:\n  - name: a\n    url: https://github.com\n    owners: [o]\n", "token"},
		{"unset_token_reference", "connections:\n  - name: a\n    url: https://github.com\n    token: ${UNSET_TOKEN}\n    owners: [o]\n", "token references UNSET_TOKEN, which is not set"},
		{"disallowed_reference", "connections:\n  - name: a\n    url: https://github.com\n    token: ${HOME_PATH}\n    owners: [o]\n", "token references HOME_PATH, which is not an allowed name"},
		{"malformed_token_reference", "connections:\n  - name: a\n    url: https://github.com\n    token: ${1BAD}\n    owners: [o]\n", "token must be one ${NAME} reference"},
		{"no_owners", "connections:\n  - name: a\n    url: https://github.com\n    token: x\n", "owner"},
		{"bad_owner", "connections:\n  - name: a\n    url: https://github.com\n    token: x\n    owners: [\"has space\"]\n", "owner"},
		{"duplicate_owner", "connections:\n  - name: a\n    url: https://github.com\n    token: x\n    owners: [o, O]\n", "owner"},
		{"bare_exclude_repo", "connections:\n  - name: a\n    url: https://github.com\n    token: x\n    owners: [o]\n    exclude_repos: [noisy]\n", "exclude_repos"},
		{"bare_skip_repo", "connections:\n  - name: a\n    url: https://github.com\n    token: x\n    owners: [o]\n    security: {skip_repos: [noisy]}\n", "skip_repos"},
		{"level_warn", "log_level: warn\n" + minimal, "log_level WARN"},
		{"level_warning", "log_level: warning\n" + minimal, "log_level WARN"},
		{"level_error", "log_level: error\n" + minimal, "log_level ERROR"},
		{"level_above_info", "log_level: info+1\n" + minimal, "log_level INFO+1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("Load(%s) = nil error, want one mentioning %q", tc.name, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load(%s) = %q, want it to mention %q", tc.name, err, tc.want)
			}
		})
	}
}

func TestLoad_refuses_a_token_that_is_not_exactly_one_reference(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	const secret = "plaintext-secret-value"
	tests := []struct{ name, token string }{
		{"literal", secret},
		{"quoted_literal", `"` + secret + `"`},
		{"prefix", secret + "${GITHUB_TOKEN}"},
		{"suffix", "${GITHUB_TOKEN}" + secret},
		{"two_references", "${GITHUB_TOKEN}${GITHUB_TOKEN}"},
		{"quoted_edge_space", `" ${GITHUB_TOKEN}"`},
		{"block_scalar", "|\n      ${GITHUB_TOKEN}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := "connections:\n  - name: a\n    url: https://github.com\n    token: " + tc.token + "\n    owners: [o]\n"
			_, _, err := Load(writeConfig(t, body))
			if err == nil {
				t.Fatalf("Load(token %s) = nil error, want it refused", tc.name)
			}
			if msg := err.Error(); !strings.Contains(msg, `connection "a": token must be one ${NAME} reference`) || strings.Contains(msg, secret) || strings.Contains(msg, "test-token") {
				t.Errorf("Load(token %s) = %q, want the connection named, the ${NAME} form pointed at and no value echoed", tc.name, msg)
			}
		})
	}
}

func TestLoad_accepts_a_quoted_token_reference(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	for _, token := range []string{`"${GITHUB_TOKEN}"`, `'${GITHUB_TOKEN}'`} {
		body := "connections:\n  - name: a\n    url: https://github.com\n    token: " + token + "\n    owners: [o]\n"
		cfg, _, err := Load(writeConfig(t, body))
		if err != nil || cfg.Connections[0].Token != "test-token" {
			t.Errorf("Load(token %s) = %v, want the expanded token", token, err)
		}
	}
}

func TestLoad_unset_token_reference_names_the_variable_not_a_value(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	os.Unsetenv("GITHUB_TOKEN")
	_, _, err := Load(writeConfig(t, minimal))
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") || !strings.Contains(err.Error(), `"github"`) {
		t.Fatalf("Load with GITHUB_TOKEN unset = %v, want an error naming the connection and the variable", err)
	}
}

func TestLoad_empty_token_variable_names_the_variable_not_a_value(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	_, _, err := Load(writeConfig(t, minimal))
	if err == nil || !strings.Contains(err.Error(), "token references GITHUB_TOKEN, which is empty") || !strings.Contains(err.Error(), `"github"`) {
		t.Fatalf("Load with GITHUB_TOKEN set and empty = %v, want an error naming the connection and the variable", err)
	}
}

func TestLoad_expands_only_allowlisted_names(t *testing.T) {
	t.Setenv("HOME_SECRET", "must-not-appear")
	body := "connections:\n  - name: a\n    url: https://github.com\n    token: ${HOME_SECRET}\n    owners: [o]\n"
	_, _, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load with a token referencing a non-allowlisted variable = nil error, want the unexpanded reference refused")
	}
	if strings.Contains(err.Error(), "must-not-appear") {
		t.Errorf("Load error = %q, carries the variable's value", err)
	}
}

func TestLoad_durations_clamp_and_fall_back(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	tests := []struct {
		name         string
		head         string
		wantInterval time.Duration
		wantLookback time.Duration
		wantWarn     string
	}{
		{"interval_below_floor", "scan_interval: 10s\n", time.Minute, 72 * time.Hour, "clamped"},
		{"interval_above_ceiling", "scan_interval: 9000h\nlookback: 720h\n", 171 * time.Hour, 720 * time.Hour, "clamped"},
		{"interval_off_sentinel", "scan_interval: \"off\"\n", 15 * time.Minute, 72 * time.Hour, "invalid scan_interval"},
		{"interval_zero", "scan_interval: \"0\"\n", 15 * time.Minute, 72 * time.Hour, "invalid scan_interval"},
		{"lookback_above_ceiling", "lookback: 1000h\n", 15 * time.Minute, 720 * time.Hour, "clamped"},
		{"lookback_below_floor", "scan_interval: 10m\nlookback: 1m\n", 10 * time.Minute, time.Hour, "clamped"},
		{"lookback_garbage", "lookback: soon\n", 15 * time.Minute, 72 * time.Hour, "invalid lookback"},
		{"level_garbage", "log_level: chatty\n", 15 * time.Minute, 72 * time.Hour, "invalid log_level"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, warns, err := Load(writeConfig(t, tc.head+minimal))
			if err != nil {
				t.Fatalf("Load(%s) = error %v, want nil", tc.name, err)
			}
			if cfg.ScanInterval != tc.wantInterval || cfg.Lookback != tc.wantLookback {
				t.Errorf("Load(%s) = interval %v lookback %v, want %v %v", tc.name, cfg.ScanInterval, cfg.Lookback, tc.wantInterval, tc.wantLookback)
			}
			if len(warns) != 1 || !strings.Contains(warns[0].Msg, tc.wantWarn) {
				t.Errorf("Load(%s) warnings = %+v, want exactly one containing %q", tc.name, warns, tc.wantWarn)
			}
		})
	}
}

func TestLoad_refuses_a_lookback_shorter_than_two_intervals_and_one_scan(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	tests := []struct {
		name string
		head string
		// want is what the error names, empty for a file that loads.
		want []string
	}{
		{"lookback_below_the_interval", "scan_interval: 2h\nlookback: 1h\n", []string{"lookback 1h0m0s", "scan_interval 2h0m0s", "4h0m0s limit", "at least 8h24m0s"}},
		{"default_lookback_short", "scan_interval: 17h9m\n", []string{"lookback 72h0m0s", "scan_interval 17h9m0s", "at least 72h1m48s"}},
		{"four_intervals_without_the_jitter", "scan_interval: 15m\nlookback: 1h\n", []string{"lookback 1h0m0s", "10% late", "at least 1h3m0s"}},
		{"lookback_at_the_rule", "scan_interval: 10h\nlookback: 42h\n", nil},
		{"four_intervals_and_the_jitter", "scan_interval: 15m\nlookback: 63m\n", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, tc.head+minimal))
			if tc.want == nil {
				if err != nil {
					t.Errorf("Load(%s) = %v, want nil: the lookback covers two intervals and one scan at its limit", tc.name, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load(%s) = nil error, want the lookback refused", tc.name)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("Load(%s) = %q, want it to name %q", tc.name, err, w)
				}
			}
		})
	}
}

// TestMaxScanInterval_keeps_two_scans_inside_the_listing_retention pins the
// coupling between the interval ceiling and the store: two scan starts may be
// one scan at its limit plus the longest jittered wait apart, and the store
// keeps when it last listed a repository longer than that, so a gap after
// the longest wait is still reported.
func TestMaxScanInterval_keeps_two_scans_inside_the_listing_retention(t *testing.T) {
	gap := ScanLimit(maxScanInterval) + maxWait(maxScanInterval)
	if gap >= runstate.WorkflowRetention {
		t.Errorf("two scans at maxScanInterval %s can start %s apart, want under the listing retention %s", maxScanInterval, gap, runstate.WorkflowRetention)
	}
}

func TestMaxScanInterval_is_the_longest_whole_hour_the_longest_lookback_covers(t *testing.T) {
	if err := (source{}).overlap(maxScanInterval, maxLookback); err != nil {
		t.Errorf("overlap(maxScanInterval %s, maxLookback %s) = %v, want nil", maxScanInterval, maxLookback, err)
	}
	if err := (source{}).overlap(maxScanInterval+time.Hour, maxLookback); err == nil {
		t.Errorf("overlap(%s, maxLookback %s) = nil, want refused: maxScanInterval is not the longest whole hour", maxScanInterval+time.Hour, maxLookback)
	}
}

func TestMaxWait_bounds_every_jittered_wait_of_the_scan_loop(t *testing.T) {
	for _, interval := range []time.Duration{time.Minute, 15 * time.Minute, 17*time.Hour + 9*time.Minute, maxScanInterval} {
		for range 200 {
			if d := scheduler.JitteredDelay(interval, ScanJitter); d > maxWait(interval) {
				t.Fatalf("JitteredDelay(%s, ScanJitter) = %s, want at most maxWait %s", interval, d, maxWait(interval))
			}
		}
	}
}

func TestConnection_Instance_names_one_forge_in_every_spelling(t *testing.T) {
	tests := []struct{ url, want string }{
		{"https://github.test", "https://github.test"},
		{"HTTPS://GitHub.Test", "https://github.test"},
		{"https://github.test:443", "https://github.test"},
		{"http://git.test:80", "http://git.test"},
		{"https://git.test:8443", "https://git.test:8443"},
		{"https://git.test/Forge/", "https://git.test/Forge"},
		{"https://[2001:DB8::1]:443", "https://[2001:db8::1]"},
		{"https://[2001:db8::1]:8443", "https://[2001:db8::1]:8443"},
		{"https://www.github.com", "https://github.com"},
		{"https://api.github.com", "https://github.com"},
		{"http://github.com", "https://github.com"},
		{"http://WWW.GitHub.com:80/x", "https://github.com"},
		{"https://github.com/some/path", "https://github.com"},
	}
	for _, tc := range tests {
		c := Connection{URL: tc.url}
		if got := c.Instance(); got != tc.want {
			t.Errorf("Connection{URL: %q}.Instance() = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestLoad_missing_file_with_old_variables_names_the_migration(t *testing.T) {
	for _, name := range []string{"GITHUB_OWNER", "GITHUB_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GITHUB_OWNER", "")
			t.Setenv("GITHUB_TOKEN", "")
			t.Setenv(name, "example")
			_, _, err := Load(filepath.Join(t.TempDir(), "config.yaml"))
			if err == nil || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "README") {
				t.Fatalf("Load(missing) with %s set = %v, want a not-exist error pointing at the README migration", name, err)
			}
			if strings.Contains(err.Error(), "example") {
				t.Errorf("Load(missing) error %q carries the variable's value, want its name only", err)
			}
		})
	}
}

func TestLoad_missing_file_is_a_not_exist_error(t *testing.T) {
	t.Setenv("GITHUB_OWNER", "")
	t.Setenv("GITHUB_TOKEN", "")
	_, _, err := Load(filepath.Join(t.TempDir(), "config.yaml"))
	if !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "README") {
		t.Fatalf("Load(missing) with no old variable set = %v, want os.ErrNotExist and no migration hint", err)
	}
}

func TestLoad_refuses_a_fifo_without_blocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := mkfifo(path); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if _, _, err := Load(path); err == nil {
		t.Fatal("Load(fifo) = nil error, want a refusal")
	}
}

func TestLoad_never_logs(t *testing.T) {
	rec := capture.Default(t)
	t.Setenv("GITHUB_TOKEN", "test-token")
	if _, _, err := Load(writeConfig(t, "scan_interval: nope\n"+minimal)); err != nil {
		t.Fatalf("Load = error %v", err)
	}
	if _, _, err := Load(writeConfig(t, "bogus: 1\n")); err == nil {
		t.Fatal("Load(bogus) = nil error")
	}
	if rec.Len() != 0 {
		t.Errorf("Load logged %d record(s) %v, want none: warnings are returned, never logged", rec.Len(), rec.Messages())
	}
}

func TestScanIntervalFromFile(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	if got := ScanIntervalFromFile(writeConfig(t, "scan_interval: 5m\n"+minimal)); got != 5*time.Minute {
		t.Errorf("ScanIntervalFromFile(5m) = %v, want 5m0s", got)
	}
	if got := ScanIntervalFromFile(filepath.Join(t.TempDir(), "absent.yaml")); got != 15*time.Minute {
		t.Errorf("ScanIntervalFromFile(absent) = %v, want 15m0s", got)
	}
	if got := ScanIntervalFromFile(writeConfig(t, "scan_interval: 5m\nbogus: 1\n")); got != 15*time.Minute {
		t.Errorf("ScanIntervalFromFile(invalid file) = %v, want 15m0s", got)
	}
}

func TestPath_reads_CONFIG_PATH(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	if got := Path(); got != "/config/config.yaml" {
		t.Errorf("Path() with CONFIG_PATH unset = %q, want /config/config.yaml", got)
	}
	for _, want := range []string{"/tmp/forge-scout.yaml", " /tmp/forge scout.yaml ", "   "} {
		t.Setenv("CONFIG_PATH", want)
		if got := Path(); got != want {
			t.Errorf("Path() with CONFIG_PATH=%q = %q, want it verbatim", want, got)
		}
	}
}

func TestLoad_quotes_the_path_it_could_not_read(t *testing.T) {
	path := filepath.Join(t.TempDir(), "padded ")
	_, _, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), strconv.Quote(path)) {
		t.Errorf("Load(%q) error = %v, want it to quote the path", path, err)
	}
}

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

func TestLoad_the_shipped_example_config(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("FORGEJO_TOKEN", "test-token")
	cfg, warns, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil || len(warns) != 0 {
		t.Fatalf("Load(config.example.yaml) = %v, warnings %v, want the example to load clean", err, warns)
	}
	if len(cfg.Connections) == 0 {
		t.Error("config.example.yaml declares no connection")
	}
}

func TestLoad_refuses_a_token_variable_outside_token(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret-token-value")
	for _, body := range []string{
		"connections:\n  - name: ${GITHUB_TOKEN}\n    url: https://github.com\n    token: ${GITHUB_TOKEN}\n    owners: [o]\n",
		"connections:\n  - name: a\n    url: https://github.com\n    token: ${GITHUB_TOKEN}\n    owners: [\"${GITHUB_TOKEN}\"]\n",
		"connections:\n  - name: a\n    url: https://github.com\n    token: &t ${GITHUB_TOKEN}\n    owners: [o]\n    bot_authors: [*t]\n",
		"connections:\n  - name: a\n    url: https://github.com\n    token: ${GITHUB_TOKEN}\n    owners: [o]\n    security:\n      skip_repos: [\"o/${GITHUB_TOKEN}\"]\n",
	} {
		_, _, err := Load(writeConfig(t, body))
		if err == nil || strings.Contains(err.Error(), "secret-token-value") || !strings.Contains(err.Error(), "outside token") {
			t.Errorf("Load(%q) = %v, want a refusal that does not carry the value", body, err)
		}
	}
}

func TestLoad_refuses_a_variable_a_token_reads_anywhere_else(t *testing.T) {
	const secret = "https://credential.example/opaque-token"
	t.Setenv("FORGE_SCOUT_SECRET", secret)
	body := "connections:\n  - name: a\n    url: ${FORGE_SCOUT_SECRET}\n    token: ${FORGE_SCOUT_SECRET}\n    owners: [o]\n"
	cfg, warns, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatalf("Load = url %q, nil error, want a refusal: the url would log the token variable", cfg.Connections[0].URL)
	}
	if strings.Contains(err.Error(), "opaque-token") || !strings.Contains(err.Error(), "outside token") {
		t.Errorf("Load error = %q, want a refusal naming the place, never the value", err)
	}
	for _, w := range warns {
		for _, a := range w.Attrs {
			if strings.Contains(a.Value.String(), "opaque-token") {
				t.Errorf("Load warning %q carries the token value", w.Msg)
			}
		}
	}
}

func TestLoad_a_prefixed_variable_no_token_reads_expands_in_any_value(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("FORGE_SCOUT_GITHUB_URL", "https://github.example")
	body := "connections:\n  - name: a\n    url: ${FORGE_SCOUT_GITHUB_URL}\n    token: ${GITHUB_TOKEN}\n    owners: [o]\n"
	cfg, _, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load with FORGE_SCOUT_GITHUB_URL as the url = %v, want it loaded", err)
	}
	if got := cfg.Connections[0].URL; got != "https://github.example" {
		t.Errorf("Load url = %q, want https://github.example expanded", got)
	}
}

func TestLoad_an_unreferenced_token_variable_never_refuses_the_file(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("X_TOKEN", "github")
	body := "connections:\n  - name: github\n    url: https://github.com\n    token: ${GITHUB_TOKEN}\n    owners: [github]\n"
	if _, _, err := Load(writeConfig(t, body)); err != nil {
		t.Errorf("Load with an unrelated X_TOKEN whose value appears in the file = %v, want it loaded: the file never references X_TOKEN", err)
	}
}

// TestLoad_never_shows_a_value_expanded_from_the_environment plants a value
// through FORGE_SCOUT_SECRET on every path that warns or errors. No error or
// warning may carry it, in any form a diagnostic would print it; one that
// would name the value names the reference instead.
func TestLoad_never_shows_a_value_expanded_from_the_environment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	const ref = "${FORGE_SCOUT_SECRET}"
	conn := func(name, fields string) string {
		return "  - name: " + name + "\n    url: https://github.com\n    token: ${GITHUB_TOKEN}\n" + fields
	}
	owned := "    owners: [o]\n"
	tests := []struct {
		name, secret, doc string
		// shown are the forms a diagnostic would print the value in, beside
		// the value itself.
		shown []string
		// named is whether the diagnostic names the value's reference.
		named bool
	}{
		{"scan_interval_above_ceiling", "999h", "scan_interval: " + ref + "\nlookback: 720h\n" + minimal, []string{"999h0m0s"}, true},
		{"scan_interval_below_floor", "7s", "scan_interval: " + ref + "\n" + minimal, nil, true},
		{"scan_interval_invalid", "hunter2-interval", "scan_interval: " + ref + "\n" + minimal, nil, false},
		{"lookback_above_ceiling", "9999h", "lookback: " + ref + "\n" + minimal, []string{"9999h0m0s"}, true},
		{"lookback_invalid", "never-valid", "lookback: " + ref + "\n" + minimal, nil, false},
		{"log_level_invalid", "chatty-secret", "log_level: " + ref + "\n" + minimal, nil, false},
		{"log_level_above_info", "Warn+3", "log_level: " + ref + "\n" + minimal, []string{"WARN+3"}, true},
		{"lookback_short_of_the_interval", "3h", "scan_interval: 1h\nlookback: " + ref + "\n" + minimal, []string{"3h0m0s"}, true},
		{"interval_too_long_for_the_lookback", "7h", "scan_interval: " + ref + "\nlookback: 8h\n" + minimal, []string{"7h0m0s", "14h0m0s", "29h24m0s"}, true},
		{"name_invalid", "Not A Name", "connections:\n" + conn(ref, owned), nil, false},
		{"name_twice", "twin", "connections:\n" + conn(ref, owned) + conn(ref, owned), nil, true},
		{"name_labels_another_problem", "labelled", "connections:\n" + conn(ref, ""), nil, true},
		{"url_invalid", "ftp://secret-host.example", "connections:\n  - name: a\n    url: " + ref + "\n    token: ${GITHUB_TOKEN}\n" + owned, nil, false},
		{"url_plaintext", "http://plain-host.example", "connections:\n  - name: a\n    url: " + ref + "\n    token: ${GITHUB_TOKEN}\n" + owned, nil, false},
		{"api_url_invalid", "https://user:pw@api-host.example", "connections:\n" + conn("a", owned+"    api_url: "+ref+"\n"), nil, false},
		{"owner_twice", "secret-owner", "connections:\n" + conn("a", "    owners: [\""+ref+"\", \""+ref+"\"]\n"), nil, true},
		{"owner_invalid", "bad owner!", "connections:\n" + conn("a", "    owners: [\""+ref+"\"]\n"), nil, false},
		{"exclude_repos_bare", "secretrepo", "connections:\n" + conn("a", owned+"    exclude_repos: [\""+ref+"\"]\n"), nil, false},
		{"skip_repos_bare", "secretskip", "connections:\n" + conn("a", owned+"    security:\n      skip_repos: [\""+ref+"\"]\n"), nil, false},
		{"decode_type_mismatch", "notabool-secret", "connections:\n" + conn("a", owned+"    private_addresses: "+ref+"\n"), nil, false},
		{"token_beside_another_problem", "tok-secret-value", "connections:\n  - name: a\n    url: https://github.com\n    token: " + ref + "\n", nil, false},
		{"token_variable_elsewhere", "tok-elsewhere-value", "connections:\n  - name: a\n    url: https://github.com\n    token: " + ref + "\n" + owned + "    bot_authors: [\"" + ref + "\"]\n", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FORGE_SCOUT_SECRET", tc.secret)
			_, warns, err := Load(writeConfig(t, tc.doc))
			var said []string
			if err != nil {
				said = append(said, err.Error())
			}
			for _, w := range warns {
				said = append(said, w.Msg)
				for _, a := range w.Attrs {
					said = append(said, a.Key+"="+a.Value.String())
				}
			}
			if len(said) == 0 {
				t.Fatalf("Load(%s) = no error and no warning, want a diagnostic", tc.name)
			}
			text := strings.Join(said, "\n")
			for _, v := range append([]string{tc.secret}, tc.shown...) {
				if strings.Contains(text, v) {
					t.Errorf("Load(%s) says %q, which carries the expanded value as %q", tc.name, text, v)
				}
			}
			if tc.named && !strings.Contains(text, ref) {
				t.Errorf("Load(%s) says %q, want the value named as %s", tc.name, text, ref)
			}
		})
	}
}
