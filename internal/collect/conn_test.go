package collect

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/github-scout/internal/ghquota"
	"github.com/cplieger/github-scout/internal/githubrest"
	"github.com/cplieger/httpx/v5"
)

func TestScan_github_rest_403_with_throttle_headers_is_a_rate_limit_stop(t *testing.T) {
	tests := []struct{ name, remaining, retryAfter string }{
		{name: "budget_spent", remaining: "0"},
		{name: "retry_after", remaining: "4000", retryAfter: "60"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-RateLimit-Remaining", tc.remaining)
				w.Header().Set("X-RateLimit-Reset", "1791374400")
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(http.StatusForbidden)
			}))
			t.Cleanup(srv.Close)
			ep := githubEndpoint("gh")
			ep.rest = githubrest.New(httpx.NewClient(5*time.Second), srv.URL, "test-token", "2026-03-10", armedMeter(),
				slog.New(slog.NewTextHandler(io.Discard, nil)), httpx.WithMaxAttempts(1))
			h := newHarness(t, ep)
			h.scan(t)
			if s := lineFor(t, h.rec, "scan stopped", "gh"); s["reason"] != "rate_limited" || s["phase"] != "security" {
				t.Errorf("scan stopped reason %q phase %q, want rate_limited in security", s["reason"], s["phase"])
			}
			if slices.Contains(h.rec.Messages(), "code scanning unreadable") {
				t.Errorf("a throttle 403 was logged as a permission refusal; messages %v", h.rec.Messages())
			}
			if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "rate_limited" || l["failed_signals"] != "security_alerts" || l["errors"] != "1" {
				t.Errorf("scan degraded cause %q failed_signals %q errors %q, want rate_limited, security_alerts and 1", l["cause"], l["failed_signals"], l["errors"])
			}
		})
	}
}

// secondaryLimitBody is the error envelope GitHub documents for a secondary
// rate limit, which may come with neither Retry-After nor a spent budget
// (https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api).
const secondaryLimitBody = `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.",` +
	`"documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api"}`

func TestScan_a_github_rest_403_with_no_throttle_header_sends_no_further_github_only_request(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "4000")
		w.Header().Set("X-RateLimit-Reset", "1791374400")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, secondaryLimitBody)
	}))
	t.Cleanup(srv.Close)
	ep := githubEndpoint("gh")
	ep.conn.repos["o"] = []forge.Repo{{Path: "o/a", DefaultBranch: "main"}, {Path: "o/b", DefaultBranch: "main"}}
	ep.rest = githubrest.New(httpx.NewClient(5*time.Second), srv.URL, "test-token", "2026-03-10", armedMeter(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), httpx.WithMaxAttempts(1))
	h := newHarness(t, ep)
	h.scan(t)
	if n := requests.Load(); n != 1 {
		t.Errorf("GitHub-only requests after a 403 with budget left and no Retry-After = %d, want 1", n)
	}
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "github_refused" {
		t.Errorf("scan degraded cause = %q, want github_refused: the refusal is ambiguous, never named a rate limit", l["cause"])
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	if c["security_unreadable"] != "1" || c["security_unread_repos"] != "1" || c["workflows_unread_repos"] != "2" {
		t.Errorf("scan complete security_unreadable %q security_unread_repos %q workflows_unread_repos %q, want 1, 1 and 2",
			c["security_unreadable"], c["security_unread_repos"], c["workflows_unread_repos"])
	}
}

func TestScan_a_request_timeout_under_a_live_scan_is_a_failed_read(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	ep := githubEndpoint("gh")
	ep.rest = githubrest.New(httpx.NewClient(2*time.Millisecond), srv.URL, "test-token", "2026-03-10", armedMeter(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), httpx.WithMaxAttempts(1))
	h := newHarness(t, ep)
	h.scan(t)
	c := lineFor(t, h.rec, "scan complete", "gh")
	if _, ok := c["security_alerts"]; ok || c["degraded"] != "true" || c["security_unreadable"] != "1" {
		t.Errorf("scan complete after a timed-out alert read = degraded %q security_alerts %q unreadable %q, want degraded, no count, 1 unreadable",
			c["degraded"], c["security_alerts"], c["security_unreadable"])
	}
	if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != "code_scanning_blind" {
		t.Errorf("scan degraded cause = %q, want code_scanning_blind", l["cause"])
	}
}

func TestScan_a_github_workflows_404_is_never_read_as_no_disabled_workflows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(srv.Close)
	ep := githubEndpoint("gh")
	ep.rest = githubrest.New(httpx.NewClient(5*time.Second), srv.URL, "test-token", "2026-03-10", armedMeter(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), httpx.WithMaxAttempts(1))
	h := newHarness(t, ep)
	h.scan(t)
	c := lineFor(t, h.rec, "scan complete", "gh")
	if v, ok := c["disabled_workflows"]; ok || c["disabled_workflows_read"] == "complete" || !strings.Contains(c["failed_signals"], "disabled_workflows") {
		t.Errorf("scan complete after a workflows 404 = disabled_workflows %q (present %t), disabled_workflows_read %q, failed_signals %q, "+
			"want no count, a read short of complete and disabled_workflows failed", v, ok, c["disabled_workflows_read"], c["failed_signals"])
	}
	if c["security_alerts_read"] != "complete" || c["security_alerts"] != "0" {
		t.Errorf("scan complete after a code-scanning 404 = security_alerts_read %q security_alerts %q, want complete with 0: no analyses means no alerts",
			c["security_alerts_read"], c["security_alerts"])
	}
}

func TestScan_a_truncated_github_only_listing_emits_no_snapshot(t *testing.T) {
	tests := []struct {
		name, key, msg, signal string
		absent                 []string
		kept                   []string
	}{
		{
			name: "security", key: "alerts o/r", msg: "security alert", signal: "security_alerts",
			absent: []string{"security_alerts", "security_alerts_listed", "security_alerts_low"},
			kept:   []string{"security_truncated_repos", "security_repos_read", "security_alerts_read"},
		},
		{
			name: "workflows", key: "workflows o/r", msg: "disabled workflow", signal: "disabled_workflows",
			absent: []string{"disabled_workflows"},
			kept:   []string{"workflows_truncated_repos", "disabled_workflows_read"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := githubEndpoint("gh")
			ep.gh.alerts = map[string][]forge.Alert{"o/r": {{Repo: "o/r", Number: 1, Severity: "low", Tool: "CodeQL"}}}
			ep.gh.workflows = map[string][]forge.Workflow{"o/r": {{Repo: "o/r", Name: "nightly", State: "disabled_manually"}}}
			ep.gh.truncated = map[string]bool{tc.key: true}
			h := newHarness(t, ep)
			h.scan(t)
			if n := h.rec.CountExact(tc.msg); n != 0 {
				t.Errorf("a truncated %s listing logged %d %q line(s), want 0: never a smaller snapshot", tc.name, n, tc.msg)
			}
			c := lineFor(t, h.rec, "scan complete", "gh")
			for _, k := range tc.absent {
				if v, ok := c[k]; ok {
					t.Errorf("scan complete after a truncated %s listing carries %s = %s, want it absent", tc.name, k, v)
				}
			}
			for _, k := range tc.kept {
				if _, ok := c[k]; !ok {
					t.Errorf("scan complete after a truncated %s listing lacks %s, want the coverage kept", tc.name, k)
				}
			}
			if c["degraded"] != "true" || !strings.Contains(c["failed_signals"], tc.signal) {
				t.Errorf("scan complete degraded %q failed_signals %q, want degraded naming %s", c["degraded"], c["failed_signals"], tc.signal)
			}
			noLineFor(t, h.rec, "scan degraded", "gh")
		})
	}
}

func TestScan_a_rate_limited_read_names_its_failed_signal(t *testing.T) {
	tests := []struct {
		setup  func(ep *endpoint)
		name   string
		signal string
	}{
		{name: "open", signal: "repos", setup: func(ep *endpoint) { ep.openErr = forge.ErrRateLimited }},
		{name: "discovery", signal: "repos", setup: func(ep *endpoint) { ep.conn.errs = map[string]error{"discover o": forge.ErrRateLimited} }},
		{name: "prs", signal: "open_prs", setup: func(ep *endpoint) { ep.conn.errs = map[string]error{"prs o": forge.ErrRateLimited} }},
		{name: "issues", signal: "open_issues", setup: func(ep *endpoint) { ep.conn.errs = map[string]error{"issues o": forge.ErrRateLimited} }},
		{name: "runs", signal: "runs", setup: func(ep *endpoint) { ep.conn.errs = map[string]error{"runs o/r": forge.ErrRateLimited} }},
		{name: "checks", signal: "pr_checks", setup: func(ep *endpoint) {
			ep.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 1, Author: "x", HeadSHA: "h1"}}}
			ep.conn.errs = map[string]error{"checks h1": forge.ErrRateLimited}
		}},
		{name: "workflows", signal: "disabled_workflows", setup: func(ep *endpoint) {
			ep.gh.errs = map[string]error{"workflows o/r": forge.ErrRateLimited}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := githubEndpoint("gh")
			tc.setup(ep)
			h := newHarness(t, ep)
			h.scan(t)
			if l := lineFor(t, h.rec, "scan degraded", "gh"); l["failed_signals"] != tc.signal || l["errors"] != "1" {
				t.Errorf("rate-limited %s read: scan degraded failed_signals %q errors %q, want %s and 1",
					tc.name, l["failed_signals"], l["errors"], tc.signal)
			}
		})
	}
}

// armedMeter is the quota meter of a connection that answered as GitHub.
func armedMeter() *ghquota.Meter {
	m := ghquota.New(nil)
	m.Arm()
	return m
}
