package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/collect"
	"github.com/cplieger/github-scout/internal/config"
	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/github-scout/internal/forgeconn/forgeconntest"
	"github.com/cplieger/github-scout/internal/githubrest"
	"github.com/cplieger/github-scout/internal/runstate"
	"github.com/cplieger/slogx"
	"github.com/cplieger/slogx/capture"
)

func world(owner string) forgeconntest.World {
	t0 := time.Now().UTC().Truncate(time.Second)
	return forgeconntest.World{
		Login: "me", Owner: owner,
		Repos: []forgeconntest.Repo{{
			Name: "api", DefaultBranch: "main",
			Runs: []forgeconntest.Run{{
				ID: 7, Number: 2, Workflow: "ci", Branch: "main", Event: "push", Outcome: "failure",
				Created: t0.Add(-time.Hour), Started: t0.Add(-59 * time.Minute), Updated: t0.Add(-50 * time.Minute),
			}},
			PRs:    []forgeconntest.Item{{Number: 3, Title: "Change", Author: "someone", HeadSHA: "abc", Created: t0.Add(-time.Hour), Updated: t0}},
			Issues: []forgeconntest.Item{{Number: 4, Title: "Bug", Author: "me", Created: t0.Add(-time.Hour), Updated: t0}},
			Checks: map[string]string{"abc": "success"},
			Alerts: []forgeconntest.Alert{{Number: 1, Rule: "go/x", Severity: "high", Tool: "CodeQL", Created: t0}},
			Workflows: []forgeconntest.Workflow{
				{ID: 1, Name: "nightly", Path: ".github/workflows/n.yml", State: "disabled_inactivity"},
				{ID: 2, Name: "ci", Path: ".github/workflows/ci.yml", State: "active"},
			},
		}},
	}
}

// TestScan_two_forges_through_the_real_adapter drives one scan through
// forgeconn and githubrest against a GitHub and a Forgejo fake and checks
// each connection's lines carry its own forge.
func TestScan_two_forges_through_the_real_adapter(t *testing.T) {
	rec := capture.Default(t)
	gh := forgeconntest.New(forgeconntest.GitHub, world("acme"))
	t.Cleanup(gh.Close)
	fj := forgeconntest.New(forgeconntest.Forgejo, world("home"))
	t.Cleanup(fj.Close)
	cfg := config.Config{
		Lookback: 72 * time.Hour, ScanInterval: 15 * time.Minute,
		Connections: []config.Connection{gh.Connection("hub"), fj.Connection("forgejo-home")},
	}
	c := newCollector(t.Context(), &cfg, slog.Default(), testStore(t))
	t.Cleanup(c.Close)
	if got := c.Scan(t.Context()); got != collect.Complete {
		t.Fatalf("scan = %s, want complete; messages %v; github unhandled %v; forgejo unhandled %v", got, rec.Messages(), gh.Unhandled(), fj.Unhandled())
	}
	want := map[string]string{"hub": "github", "forgejo-home": "forgejo"}
	for _, msg := range []string{"scanning", "ci run", "open pull request", "open issue", "failing workflow", "scan complete"} {
		got := map[string]string{}
		for _, r := range rec.Records() {
			if r.Message != msg {
				continue
			}
			var conn, fg string
			r.Attrs(func(a slog.Attr) bool {
				switch a.Key {
				case "connection":
					conn = a.Value.String()
				case "forge":
					fg = a.Value.String()
				}
				return true
			})
			got[conn] = fg
		}
		for conn, fg := range want {
			if got[conn] != fg {
				t.Errorf("%q for connection %s carries forge %q, want %q (lines %v)", msg, conn, got[conn], fg, got)
			}
		}
	}
	intervals := map[string]string{}
	for _, r := range rec.Records() {
		if r.Message != "scan complete" {
			continue
		}
		var conn, interval string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "connection":
				conn = a.Value.String()
			case "scan_interval_s":
				interval = a.Value.String()
			}
			return true
		})
		intervals[conn] = interval
	}
	for conn := range want {
		if intervals[conn] != "900" {
			t.Errorf("scan complete for connection %s carries scan_interval_s %q, want the configured 15m as 900", conn, intervals[conn])
		}
	}
	if n := rec.CountExact("security alert"); n != 1 {
		t.Errorf("security alert lines = %d, want GitHub's 1", n)
	}
	if n := rec.CountExact("disabled workflow"); n != 1 {
		t.Errorf("disabled workflow lines = %d, want GitHub's 1", n)
	}
	if n := rec.CountExact("scan degraded"); n != 0 {
		t.Errorf("scan degraded lines = %d, want 0: %v", n, rec.Messages())
	}
	if len(gh.Unhandled())+len(fj.Unhandled()) != 0 {
		t.Errorf("unserved requests: github %v forgejo %v", gh.Unhandled(), fj.Unhandled())
	}
}

// TestScan_a_forgejo_run_history_past_the_page_ceiling_reads_whole_from_cold
// drives the real adapter against a Forgejo fake paging its runs at 50,
// holding 2,100 runs ten days old and one failing run an hour old, over
// three scans of a cold store.
func TestScan_a_forgejo_run_history_past_the_page_ceiling_reads_whole_from_cold(t *testing.T) {
	rec := capture.Default(t)
	w := world("home")
	t0 := time.Now().UTC().Truncate(time.Second)
	w.Repos[0].Runs = []forgeconntest.Run{{
		ID: 5000, Number: 5000, Workflow: "ci", Branch: "main", Event: "push", Outcome: "failure",
		Created: t0.Add(-time.Hour), Started: t0.Add(-59 * time.Minute), Updated: t0.Add(-50 * time.Minute),
	}}
	for i := range 2100 {
		old := t0.Add(-240*time.Hour - time.Duration(i)*time.Minute)
		w.Repos[0].Runs = append(w.Repos[0].Runs, forgeconntest.Run{
			ID: int64(2100 - i), Number: int64(2100 - i), Workflow: "ci", Branch: "main", Event: "push", Outcome: "success",
			Created: old, Started: old, Updated: old.Add(time.Minute),
		})
	}
	fj := forgeconntest.New(forgeconntest.Forgejo, w)
	t.Cleanup(fj.Close)
	cfg := config.Config{Lookback: 72 * time.Hour, Connections: []config.Connection{fj.Connection("forgejo-home")}}
	store := testStore(t)
	for scan := 1; scan <= 3; scan++ {
		c := newCollector(t.Context(), &cfg, slog.Default(), store)
		before, failing := len(fj.Requests()), rec.CountExact("failing workflow")
		c.Scan(t.Context())
		c.Close()
		got := map[string]string{}
		for _, r := range rec.Records() {
			if r.Message == "scan complete" {
				r.Attrs(func(a slog.Attr) bool {
					got[a.Key] = a.Value.String()
					return true
				})
			}
		}
		if got["runs_read"] != "complete" || got["runs_truncated_repos"] != "0" || got["degraded"] != "false" || got["failing_workflows"] != "1" {
			t.Errorf("scan %d: runs_read %q runs_truncated_repos %q degraded %q failing_workflows %q, want complete, 0, false, 1",
				scan, got["runs_read"], got["runs_truncated_repos"], got["degraded"], got["failing_workflows"])
		}
		if rec.CountExact("failing workflow") != failing+1 {
			t.Errorf("scan %d logged no failing workflow line, want run 5000's", scan)
		}
		pages := 0
		for _, r := range fj.Requests()[before:] {
			if strings.Contains(r, "actions/runs") {
				pages++
			}
		}
		if pages > 2 {
			t.Errorf("scan %d read %d run pages, want at most 2", scan, pages)
		}
	}
}

// TestScan_a_gitlab_user_named_by_digits_alone_is_read_per_project drives the
// real adapter against a GitLab fake whose owner is the user "123".
func TestScan_a_gitlab_user_named_by_digits_alone_is_read_per_project(t *testing.T) {
	rec := capture.Default(t)
	w := world("123")
	w.Repos[0].Alerts, w.Repos[0].Workflows = nil, nil
	w.UserNamespace = true
	gl := forgeconntest.New(forgeconntest.GitLab, w)
	t.Cleanup(gl.Close)
	cfg := config.Config{Lookback: 72 * time.Hour, Connections: []config.Connection{gl.Connection("lab")}}
	c := newCollector(t.Context(), &cfg, slog.Default(), testStore(t))
	t.Cleanup(c.Close)
	if got := c.Scan(t.Context()); got != collect.Complete {
		t.Fatalf("scan = %s, want complete; messages %v; unhandled %v", got, rec.Messages(), gl.Unhandled())
	}
	if rec.CountExact("open pull request") != 1 || rec.CountExact("open issue") != 1 || rec.CountExact("scan degraded") != 0 {
		t.Errorf("messages %v, want the user's one merge request and one issue and no scan degraded", rec.Messages())
	}
	for _, want := range []string{"/api/v4/projects/123%2Fapi/merge_requests", "/api/v4/projects/123%2Fapi/issues"} {
		if !slices.ContainsFunc(gl.Requests(), func(r string) bool { return strings.Contains(r, want) }) {
			t.Errorf("requests %v, want the per-project fallback read %s", gl.Requests(), want)
		}
	}
}

// TestScan_a_refused_github_only_dial_names_its_connection redirects the
// code-scanning read to a port the connection does not allow, so the
// transport refuses the dial, and checks that refusal names its connection.
func TestScan_a_refused_github_only_dial_names_its_connection(t *testing.T) {
	rec := capture.Default(t)
	base := scopeDefault()
	w := world("acme")
	w.Repos[0].AlertsRedirect = "http://127.0.0.1:9/alerts"
	gh := forgeconntest.New(forgeconntest.GitHub, w)
	t.Cleanup(gh.Close)
	cfg := config.Config{Lookback: 72 * time.Hour, Connections: []config.Connection{gh.Connection("hub")}}
	c := newCollector(t.Context(), &cfg, base, testStore(t))
	t.Cleanup(c.Close)
	c.Scan(t.Context())
	refusals := 0
	for _, r := range rec.Records() {
		if r.Message != "ssrf dial blocked" {
			continue
		}
		refusals++
		attrs := map[string]string{}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.String()
			return true
		})
		if attrs["forge"] != "github" || attrs["connection"] != "hub" {
			t.Errorf("ssrf dial blocked carries forge %q and connection %q, want github and hub", attrs["forge"], attrs["connection"])
		}
	}
	if refusals == 0 {
		t.Fatalf("no ssrf dial blocked line; messages %v", rec.Messages())
	}
}

// TestScan_a_token_rejected_after_detection_keeps_its_forge drives a Forgejo
// fake that answers detection but rejects the token at the account route,
// beside a healthy GitHub fake.
func TestScan_a_token_rejected_after_detection_keeps_its_forge(t *testing.T) {
	rec := capture.Default(t)
	dead := world("home")
	dead.RejectToken = true
	fj := forgeconntest.New(forgeconntest.Forgejo, dead)
	t.Cleanup(fj.Close)
	gh := forgeconntest.New(forgeconntest.GitHub, world("acme"))
	t.Cleanup(gh.Close)
	cfg := config.Config{Lookback: 72 * time.Hour, Connections: []config.Connection{fj.Connection("dead"), gh.Connection("hub")}}
	c := newCollector(t.Context(), &cfg, slog.Default(), testStore(t))
	t.Cleanup(c.Close)
	c.Scan(t.Context())
	for _, msg := range []string{"repo discovery failed", "scan degraded", "scan complete"} {
		if got := forgeOf(rec, msg, "dead"); got != "forgejo" {
			t.Errorf("%q for the rejected connection carries forge %q, want forgejo; messages %v", msg, got, rec.Messages())
		}
	}
	if got := forgeOf(rec, "scan complete", "hub"); got != "github" {
		t.Errorf("scan complete for the healthy connection carries forge %q, want github", got)
	}
	if got := forgeapiLineForge(rec, "Whoami", "dead"); got != "forgejo" {
		t.Errorf("forgeapi's Whoami line for the rejected connection carries forge %q, want forgejo: the product was detected before it", got)
	}
	if got := forgeOf(rec, "scan degraded", "hub"); got != "" {
		t.Errorf("the healthy connection logged scan degraded (forge %q)", got)
	}
}

// TestScan_a_connection_stopped_at_the_reserve_resumes_once_its_window_renews
// keeps one open GitHub connection across three scans: one clean, one whose
// budget falls to the reserve, and one after the reported reset.
func TestScan_a_connection_stopped_at_the_reserve_resumes_once_its_window_renews(t *testing.T) {
	rec := capture.Default(t)
	gh := forgeconntest.New(forgeconntest.GitHub, world("acme"))
	t.Cleanup(gh.Close)
	cfg := config.Config{Lookback: 72 * time.Hour, Connections: []config.Connection{gh.Connection("hub")}}
	c := newCollector(t.Context(), &cfg, slog.Default(), testStore(t))
	t.Cleanup(c.Close)
	if got := c.Scan(t.Context()); got != collect.Complete {
		t.Fatalf("Setup: scan 1 = %s, want complete; messages %v", got, rec.Messages())
	}
	reset := time.Now().Add(2 * time.Second).Truncate(time.Second)
	gh.SetBudget(forge.ReadReserve, reset)
	c.Scan(t.Context())
	if got := rec.CountExact("scan stopped"); got != 1 {
		t.Fatalf("scan 2 with the budget at the reserve logged %d scan stopped, want 1; messages %v", got, rec.Messages())
	}
	gh.SetBudget(4900, time.Time{})
	for time.Now().Before(reset) {
		time.Sleep(50 * time.Millisecond)
	}
	c.Scan(t.Context())
	if got := rec.CountExact("scan complete"); got != 2 {
		t.Errorf("scan complete lines after the reset = %d, want 2: the connection resumes once its window renews; messages %v", got, rec.Messages())
	}
	opens := 0
	for _, r := range gh.Requests() {
		if r == "GET /api/v3/user" {
			opens++
		}
	}
	if opens != 1 {
		t.Errorf("account reads = %d, want 1: the connection is reused, not reopened", opens)
	}
}

// TestScan_a_cold_open_at_the_reserve_reports_the_budget_detection_read
// starts against a GitHub fake whose detection answer reports the reserve,
// so the account read is deferred inside the open, then scans again after
// the reported reset.
func TestScan_a_cold_open_at_the_reserve_reports_the_budget_detection_read(t *testing.T) {
	rec := capture.Default(t)
	gh := forgeconntest.New(forgeconntest.GitHub, world("acme"))
	t.Cleanup(gh.Close)
	reset := time.Now().Add(2 * time.Second).Truncate(time.Second)
	gh.SetBudget(forge.ReadReserve, reset)
	cfg := config.Config{Lookback: 72 * time.Hour, Connections: []config.Connection{gh.Connection("hub")}}
	c := newCollector(t.Context(), &cfg, slog.Default(), testStore(t))
	t.Cleanup(c.Close)
	c.Scan(t.Context())
	stopped := map[string]string{}
	for _, r := range rec.Records() {
		if r.Message == "scan stopped" {
			r.Attrs(func(a slog.Attr) bool {
				stopped[a.Key] = a.Value.String()
				return true
			})
		}
	}
	wantReset := reset.UTC().Format(time.RFC3339)
	if stopped["reason"] != "read_reserve" || stopped["phase"] != "open" || stopped["budget_remaining"] != strconv.Itoa(forge.ReadReserve) || stopped["budget_reset"] != wantReset {
		t.Fatalf("scan stopped of a cold open at the reserve = %v, want reason read_reserve, phase open, budget_remaining %d and budget_reset %s", stopped, forge.ReadReserve, wantReset)
	}
	gh.SetBudget(4900, time.Time{})
	for time.Now().Before(reset) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := c.Scan(t.Context()); got != collect.Complete {
		t.Errorf("scan after the reset = %s, want complete: the connection opens once its window renews; messages %v", got, rec.Messages())
	}
}

// TestScan_a_github_403_on_a_run_listing_sends_no_further_request drives
// both real GitHub clients against a fake whose first repository's run
// listing answers 403 with budget left: neither client sends another request
// that scan, the second repository's runs and every GitHub-only read
// included, and the next scan reads again.
func TestScan_a_github_403_on_a_run_listing_sends_no_further_request(t *testing.T) {
	rec := capture.Default(t)
	w := world("acme")
	second := w.Repos[0]
	second.Name, second.RunsForbidden = "web", false
	w.Repos[0].RunsForbidden = true
	w.Repos = append(w.Repos, second)
	gh := forgeconntest.New(forgeconntest.GitHub, w)
	t.Cleanup(gh.Close)
	cfg := config.Config{Lookback: 72 * time.Hour, Connections: []config.Connection{gh.Connection("hub")}}
	c := newCollector(t.Context(), &cfg, slog.Default(), testStore(t))
	t.Cleanup(c.Close)
	c.Scan(t.Context())
	requests := gh.Requests()
	refused := slices.IndexFunc(requests, func(r string) bool { return strings.Contains(r, "/repos/acme/api/actions/runs") })
	if refused < 0 {
		t.Fatalf("requests %v, want acme/api's run listing", requests)
	}
	if after := requests[refused+1:]; len(after) != 0 {
		t.Errorf("requests after the 403 = %v, want none of either client: GitHub asks a refused client to stop", after)
	}
	if rec.CountExact("scan degraded") != 1 {
		t.Errorf("messages %v, want one scan degraded", rec.Messages())
	}
	complete := map[string]string{}
	for _, r := range rec.Records() {
		if r.Message == "scan degraded" {
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "cause" && a.Value.String() != "github_refused" {
					t.Errorf("scan degraded after the 403 cause = %q, want github_refused", a.Value.String())
				}
				return true
			})
		}
		if r.Message == "scan complete" {
			r.Attrs(func(a slog.Attr) bool {
				complete[a.Key] = a.Value.String()
				return true
			})
		}
	}
	for _, k := range []string{
		"security_alerts", "security_alerts_listed", "security_alerts_critical", "security_alerts_high", "security_alerts_medium",
		"security_alerts_low", "security_alerts_unrated", "disabled_workflows", "disabled_workflows_listed", "failing_workflows",
	} {
		if v, ok := complete[k]; ok {
			t.Errorf("scan complete after the 403 carries %s = %s, want no count: none of that family's reads was sent", k, v)
		}
	}
	for k, want := range map[string]string{
		"security_alerts_read": "blind", "disabled_workflows_read": "blind", "runs_read": "blind",
		"security_unread_repos": "2", "workflows_unread_repos": "2", "runs_unread_repos": "1",
	} {
		if complete[k] != want {
			t.Errorf("scan complete after the 403 %s = %q, want %q; line %v", k, complete[k], want, complete)
		}
	}
	c.Scan(t.Context())
	if got := gh.Requests()[len(requests):]; !slices.ContainsFunc(got, func(r string) bool { return strings.Contains(r, "/repos/acme/api/actions/runs") }) {
		t.Errorf("the next scan sent %v, want it to read up to the run listing again: the refusal holds one scan", got)
	}
}

// TestScan_a_pull_request_whose_checks_github_refuses_the_token_keeps_the_list_whole
// drives the real adapter against a GitHub fake whose search answers every
// pull request whole and refuses one private repository's check rollup, as
// GitHub answers a fine-grained token, over two scans of one process.
func TestScan_a_pull_request_whose_checks_github_refuses_the_token_keeps_the_list_whole(t *testing.T) {
	rec := capture.Default(t)
	w := world("acme")
	private := w.Repos[0]
	private.Name, private.Private, private.Alerts, private.Runs = "infra", true, nil, nil
	t0 := time.Now().UTC().Truncate(time.Second)
	private.PRs = []forgeconntest.Item{{Number: 9, Title: "Infra change", Author: "someone", HeadSHA: "def", Created: t0.Add(-time.Hour), Updated: t0, ChecksForbidden: true}}
	w.Repos = append(w.Repos, private)
	gh := forgeconntest.New(forgeconntest.GitHub, w)
	t.Cleanup(gh.Close)
	cfg := config.Config{Lookback: 72 * time.Hour, Connections: []config.Connection{gh.Connection("hub")}}
	c := newCollector(t.Context(), &cfg, slog.Default(), testStore(t))
	t.Cleanup(c.Close)
	for scan := 1; scan <= 2; scan++ {
		mark := len(rec.Records())
		if got := c.Scan(t.Context()); got != collect.Complete {
			t.Fatalf("scan %d = %s, want complete; messages %v", scan, got, rec.Messages())
		}
		checks := map[string]string{}
		complete := map[string]string{}
		for _, r := range rec.Records()[mark:] {
			attrs := map[string]string{}
			r.Attrs(func(a slog.Attr) bool {
				attrs[a.Key] = a.Value.String()
				return true
			})
			switch r.Message {
			case "open pull request":
				checks[attrs["number"]] = attrs["checks"]
			case "scan complete":
				complete = attrs
			}
		}
		if len(checks) != 2 || checks["3"] != "passing" || checks["9"] != "unreadable" {
			t.Errorf("scan %d open pull request checks = %v, want #3 passing and #9 unreadable", scan, checks)
		}
		for k, want := range map[string]string{
			"open_prs_read": "complete", "pr_checks_read": "complete", "degraded": "false", "open_prs": "2",
			"failing_checks_prs": "0", "checks_unread": "0", "checks_unsupported_for_token": "1",
		} {
			if complete[k] != want {
				t.Errorf("scan %d scan complete %s = %q, want %q; line %v", scan, k, complete[k], want, complete)
			}
		}
	}
	if n := rec.CountExact("scan degraded"); n != 0 {
		t.Errorf("scan degraded lines = %d, want 0: a check rollup GitHub refuses the token degrades nothing", n)
	}
	if n := rec.CountExact("pull request checks not available with this token"); n != 1 {
		t.Errorf("checks warnings over two scans = %d, want 1 per process", n)
	}
	for _, r := range rec.Records() {
		if r.Message == "pull request checks not available with this token" && r.Level != slog.LevelWarn {
			t.Errorf("checks warning level = %s, want WARN", r.Level)
		}
	}
	if u := gh.Unhandled(); len(u) != 0 {
		t.Errorf("unserved requests: %v", u)
	}
}

// TestScan_a_run_in_a_state_forgeapi_cannot_map_is_counted_and_never_judged
// drives the real adapter against a GitHub fake whose second run carries a
// conclusion no table knows: the run is counted, never delivered, and its
// workflow keeps the verdict of its last judged run.
func TestScan_a_run_in_a_state_forgeapi_cannot_map_is_counted_and_never_judged(t *testing.T) {
	rec := capture.Default(t)
	w := world("acme")
	known := w.Repos[0].Runs[0]
	unknown := known
	unknown.ID, unknown.Number, unknown.Outcome = 8, 3, "some_future_failure"
	w.Repos[0].Runs = append(w.Repos[0].Runs, unknown)
	gh := forgeconntest.New(forgeconntest.GitHub, w)
	t.Cleanup(gh.Close)
	cfg := config.Config{Lookback: 72 * time.Hour, Connections: []config.Connection{gh.Connection("hub")}}
	c := newCollector(t.Context(), &cfg, slog.Default(), testStore(t))
	t.Cleanup(c.Close)
	if got := c.Scan(t.Context()); got != collect.Complete {
		t.Errorf("scan with an unmapped run = %s, want complete: the listing was read to its end", got)
	}
	got := map[string]string{}
	for _, r := range rec.Records() {
		if r.Message == "scan complete" {
			r.Attrs(func(a slog.Attr) bool {
				got[a.Key] = a.Value.String()
				return true
			})
		}
	}
	if got["runs_unmapped"] != "1" || got["runs_read"] != "complete" || got["degraded"] != "false" {
		t.Errorf("scan complete runs_unmapped %q runs_read %q degraded %q, want 1, complete, false",
			got["runs_unmapped"], got["runs_read"], got["degraded"])
	}
	if rec.CountExact("ci run") != 1 || rec.CountExact("run state unknown") != 1 {
		t.Errorf("messages %v, want the known run's ci run line and one run state unknown", rec.Messages())
	}
}

// testStore is a run state store in a fresh directory, closed at the end.
func testStore(t *testing.T) *runstate.Store {
	t.Helper()
	s, err := runstate.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// forgeOf is the forge field of the msg line of connection, empty when absent.
func forgeOf(rec *capture.Recorder, msg, connection string) string {
	for _, r := range rec.Records() {
		if r.Message != msg {
			continue
		}
		var conn, fg string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "connection":
				conn = a.Value.String()
			case "forge":
				fg = a.Value.String()
			}
			return true
		})
		if conn == connection {
			return fg
		}
	}
	return ""
}

// forgeapiLineForge is the forge field of forgeapi's request line for op on
// connection, empty when there is none.
func forgeapiLineForge(rec *capture.Recorder, op, connection string) string {
	for _, r := range rec.Records() {
		if r.Message != "forgeapi request" {
			continue
		}
		var o, conn, fg string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "op":
				o = a.Value.String()
			case "connection":
				conn = a.Value.String()
			case "forge":
				fg = a.Value.String()
			}
			return true
		})
		if o == op && conn == connection {
			return fg
		}
	}
	return ""
}

// TestRunTrigger_every_accepted_log_level_keeps_the_scan_heartbeat runs a
// trigger scan through the production JSON handler at each documented
// log_level: an accepted level must emit the scan complete line the dashboard
// and the stall alert read, and a level that would drop it is refused.
func TestRunTrigger_every_accepted_log_level_keeps_the_scan_heartbeat(t *testing.T) {
	tests := []struct {
		level    string
		wantCode int
		wantMsg  string
	}{
		{"debug", 0, `"msg":"scan complete"`},
		{"info", 0, `"msg":"scan complete"`},
		{"warn", 1, `"msg":"invalid configuration"`},
		{"error", 1, `"msg":"invalid configuration"`},
	}
	for _, tc := range tests {
		t.Run(tc.level, func(t *testing.T) {
			capture.Default(t)
			var out bytes.Buffer
			h, level := slogx.NewHandler(slogx.Options{Format: slogx.JSON, Output: &out})
			slog.SetDefault(slog.New(h))
			gh := forgeconntest.New(forgeconntest.GitHub, world("acme"))
			t.Cleanup(gh.Close)
			t.Setenv("FORGE_SCOUT_TEST_TOKEN", "test-token")
			dir := t.TempDir()
			body := "log_level: " + tc.level + "\nconnections:\n  - name: hub\n    url: " + gh.URL +
				"\n    token: ${FORGE_SCOUT_TEST_TOKEN}\n    owners: [acme]\n    private_addresses: true\n    allow_plaintext: true\n"
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("Setup: %v", err)
			}
			t.Setenv("CONFIG_PATH", path)
			if code := runTrigger(level, scopeDefault(), dir); code != tc.wantCode {
				t.Errorf("runTrigger at log_level %s = %d, want %d; output %s", tc.level, code, tc.wantCode, out.String())
			}
			if n := strings.Count(out.String(), tc.wantMsg); n != 1 {
				t.Errorf("runTrigger at log_level %s logged %d %s lines, want 1; output %s", tc.level, n, tc.wantMsg, out.String())
			}
		})
	}
}

// TestTrigger_an_interrupted_scan_exits_1_without_claiming_completion cancels
// the trigger's context, as SIGINT or SIGTERM does, before its scan reads.
func TestTrigger_an_interrupted_scan_exits_1_without_claiming_completion(t *testing.T) {
	rec := capture.Default(t)
	gh := forgeconntest.New(forgeconntest.GitHub, world("acme"))
	t.Cleanup(gh.Close)
	cfg := config.Config{Lookback: 72 * time.Hour, Connections: []config.Connection{gh.Connection("hub")}}
	c := newCollector(t.Context(), &cfg, slog.Default(), testStore(t))
	t.Cleanup(c.Close)
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errors.New("interrupt"))
	if code := trigger(ctx, c, slog.Default()); code != 1 {
		t.Errorf("trigger(interrupted) = %d, want 1; messages %v", code, rec.Messages())
	}
	if rec.CountExact("trigger scan complete") != 0 || rec.CountExact("trigger scan interrupted") != 1 {
		t.Errorf("messages %v, want one trigger scan interrupted and no trigger scan complete", rec.Messages())
	}
}

// cancelOn passes every record to next, calling cancel as the first logged
// as msg arrives.
type cancelOn struct {
	next   slog.Handler
	cancel func()
	msg    string
}

func (h cancelOn) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h cancelOn) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.cancel()
	}
	return h.next.Handle(ctx, r)
}

func (h cancelOn) WithAttrs(a []slog.Attr) slog.Handler {
	return cancelOn{next: h.next.WithAttrs(a), cancel: h.cancel, msg: h.msg}
}

func (h cancelOn) WithGroup(name string) slog.Handler {
	return cancelOn{next: h.next.WithGroup(name), cancel: h.cancel, msg: h.msg}
}

// TestTrigger_a_shutdown_during_the_snapshot_burst_exits_1 cancels the
// trigger's context as the burst logs a snapshot line: its first, or its
// last before the verdicts.
func TestTrigger_a_shutdown_during_the_snapshot_burst_exits_1(t *testing.T) {
	for at, absent := range map[string]string{"open pull request": "open issue", "security tool": "scan degraded"} {
		t.Run(strings.ReplaceAll(at, " ", "_"), func(t *testing.T) {
			rec := capture.Default(t)
			gh := forgeconntest.New(forgeconntest.GitHub, world("acme"))
			t.Cleanup(gh.Close)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			base := slog.New(cancelOn{next: slog.Default().Handler(), cancel: cancel, msg: at})
			cfg := config.Config{Lookback: 72 * time.Hour, Connections: []config.Connection{gh.Connection("hub")}}
			c := newCollector(t.Context(), &cfg, base, testStore(t))
			t.Cleanup(c.Close)
			if code := trigger(ctx, c, base); code != 1 {
				t.Errorf("trigger(cancelled at %q) = %d, want 1; messages %v", at, code, rec.Messages())
			}
			if n := rec.CountExact(at); n != 1 {
				t.Fatalf("Setup: %q lines = %d, want the 1 that cancels; messages %v", at, n, rec.Messages())
			}
			for msg, want := range map[string]int{"trigger scan interrupted": 1, "trigger scan complete": 0, "scan complete": 0, absent: 0} {
				if n := rec.CountExact(msg); n != want {
					t.Errorf("trigger cancelled at %q logged %d %q line(s), want %d; messages %v", at, n, msg, want, rec.Messages())
				}
			}
		})
	}
}

func TestLoadConfig_refuses_an_invalid_file(t *testing.T) {
	rec := capture.Default(t)
	if _, ok := loadConfig(new(slog.LevelVar), slog.Default(), filepath.Join(t.TempDir(), "absent.yaml")); ok {
		t.Error("loadConfig(absent file) = ok, want refused")
	}
	if rec.CountExact("invalid configuration") != 1 {
		t.Errorf("messages %v, want one invalid configuration line", rec.Messages())
	}
}

func TestLoadConfig_never_logs_a_value_expanded_from_the_environment(t *testing.T) {
	const secret = "999h"
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("FORGE_SCOUT_SECRET", secret)
	conn := "connections:\n  - name: a\n    url: https://github.com\n    token: ${GITHUB_TOKEN}\n    owners: [o]\n"
	for name, doc := range map[string]string{
		"clamped":  "scan_interval: ${FORGE_SCOUT_SECRET}\nlookback: 720h\n" + conn,
		"warned":   "scan_interval: 1h\nlookback: ${FORGE_SCOUT_SECRET}\nlog_level: \"${FORGE_SCOUT_SECRET}\"\n" + conn,
		"labelled": "connections:\n  - name: \"${FORGE_SCOUT_SECRET}\"\n    url: https://github.com\n    token: ${GITHUB_TOKEN}\n",
	} {
		t.Run(name, func(t *testing.T) {
			rec := capture.Default(t)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatalf("Setup: %v", err)
			}
			loadConfig(new(slog.LevelVar), slog.Default(), path)
			if rec.Len() == 0 {
				t.Fatal("loadConfig logged nothing, want its diagnostics")
			}
			for _, r := range rec.Records() {
				line := r.Message
				r.Attrs(func(a slog.Attr) bool {
					line += " " + a.Key + "=" + a.Value.String()
					return true
				})
				if strings.Contains(line, secret) {
					t.Errorf("loadConfig logged %q, which carries the expanded value", line)
				}
			}
		})
	}
}

func TestHealthLease_covers_three_intervals_and_one_scan_at_its_limit(t *testing.T) {
	if got := healthLease(15 * time.Minute).Duration(); got != 75*time.Minute {
		t.Errorf("healthLease(15m).Duration() = %v, want 1h15m0s", got)
	}
}

// TestRunTrigger_exits_1_when_a_connection_was_not_listed runs a trigger scan
// against a forge whose request budget is already at the read reserve, so the
// connection is stopped before its repositories are listed.
func TestRunTrigger_exits_1_when_a_connection_was_not_listed(t *testing.T) {
	rec := capture.Default(t)
	w := world("acme")
	w.Remaining = 1
	gh := forgeconntest.New(forgeconntest.GitHub, w)
	t.Cleanup(gh.Close)
	t.Setenv("FORGE_SCOUT_TEST_TOKEN", "test-token")
	dir := t.TempDir()
	body := "connections:\n  - name: hub\n    url: " + gh.URL + "\n    token: ${FORGE_SCOUT_TEST_TOKEN}\n    owners: [acme]\n    private_addresses: true\n    allow_plaintext: true\n"
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Setenv("CONFIG_PATH", path)
	if code := runTrigger(new(slog.LevelVar), scopeDefault(), dir); code != 1 {
		t.Errorf("runTrigger with the budget at the reserve = %d, want 1; messages %v", code, rec.Messages())
	}
	if rec.CountExact("scan stopped") != 1 || rec.CountExact("scan complete") != 0 {
		t.Errorf("messages %v, want one scan stopped and no scan complete", rec.Messages())
	}
}

// TestRunTrigger_refuses_a_state_directory_another_process_holds holds the
// state directory as a running daemon does and runs a trigger beside it.
func TestRunTrigger_refuses_a_state_directory_another_process_holds(t *testing.T) {
	rec := capture.Default(t)
	gh := forgeconntest.New(forgeconntest.GitHub, world("acme"))
	t.Cleanup(gh.Close)
	t.Setenv("FORGE_SCOUT_TEST_TOKEN", "test-token")
	dir := t.TempDir()
	body := "connections:\n  - name: hub\n    url: " + gh.URL + "\n    token: ${FORGE_SCOUT_TEST_TOKEN}\n    owners: [acme]\n    private_addresses: true\n    allow_plaintext: true\n"
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Setenv("CONFIG_PATH", path)
	held, err := runstate.Open(dir)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(held.Close)
	if code := runTrigger(new(slog.LevelVar), scopeDefault(), dir); code != 1 {
		t.Errorf("runTrigger beside a holder of the state directory = %d, want 1; messages %v", code, rec.Messages())
	}
	if rec.CountExact(runstate.ErrInUse.Error()) != 1 {
		t.Errorf("messages %v, want one %q line", rec.Messages(), runstate.ErrInUse.Error())
	}
	if reqs := gh.Requests(); len(reqs) != 0 {
		t.Errorf("forge requests = %v, want none: a refused trigger reads nothing", reqs)
	}
}

// TestEvery_line_carries_one_forge_and_one_connection runs a trigger scan
// over two fake forges from a config file, with a corrupt state file, and
// checks every line the process logs carries exactly one forge and one
// connection field.
func TestEvery_line_carries_one_forge_and_one_connection(t *testing.T) {
	rec := capture.Default(t)
	base := scopeDefault()
	gh := forgeconntest.New(forgeconntest.GitHub, world("acme"))
	t.Cleanup(gh.Close)
	fj := forgeconntest.New(forgeconntest.Forgejo, world("home"))
	t.Cleanup(fj.Close)
	t.Setenv("FORGE_SCOUT_TEST_TOKEN", "test-token")
	dir := t.TempDir()
	body := "log_level: debug\nscan_interval: soon\nconnections:\n" +
		"  - name: hub\n    url: " + gh.URL + "\n    token: ${FORGE_SCOUT_TEST_TOKEN}\n    owners: [acme]\n    private_addresses: true\n    allow_plaintext: true\n" +
		"  - name: home\n    url: " + fj.URL + "\n    token: ${FORGE_SCOUT_TEST_TOKEN}\n    owners: [home]\n    private_addresses: true\n    allow_plaintext: true\n"
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{garbage"), 0o600); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Setenv("CONFIG_PATH", path)
	if code := runTrigger(new(slog.LevelVar), base, dir); code != 0 {
		t.Fatalf("runTrigger = %d, want 0; messages %v", code, rec.Messages())
	}
	for _, msg := range []string{"invalid scan_interval, using default", "connection configured", "run state unreadable; starting cold", "scan complete", "trigger scan complete"} {
		if rec.CountExact(msg) == 0 {
			t.Errorf("no %q line; messages %v", msg, rec.Messages())
		}
	}
	stamped := map[string]bool{}
	for _, r := range rec.Records() {
		fields := map[string]int{}
		var fg string
		r.Attrs(func(a slog.Attr) bool {
			fields[a.Key]++
			if a.Key == "forge" {
				fg = a.Value.String()
			}
			return true
		})
		if fields["forge"] != 1 || fields["connection"] != 1 {
			t.Errorf("%q carries forge %d times and connection %d times, want each once", r.Message, fields["forge"], fields["connection"])
		}
		if r.Message == "forgeapi request" {
			stamped[fg] = true
		}
	}
	if !stamped["github"] || !stamped["forgejo"] {
		t.Errorf("forgeapi request lines carry forges %v, want github and forgejo once each connection answered", stamped)
	}
}

// TestGitHubDotcom_spellings_name_one_forge_and_one_API holds the account
// gate and the GitHub-only client to one reading of each GitHub.com URL.
func TestGitHubDotcom_spellings_name_one_forge_and_one_API(t *testing.T) {
	for _, url := range []string{
		"https://github.com", "https://www.github.com", "https://api.github.com", "http://github.com",
		"https://GitHub.com/some/path/", "http://www.github.com:80",
	} {
		c := config.Connection{URL: url}
		if got, api := c.Instance(), githubrest.APIBase(c.URL, c.APIURL); got != "https://github.com" || api != forge.GitHubDotcomAPI {
			t.Errorf("url %q: Instance() = %q, APIBase = %q, want https://github.com and %s", url, got, api, forge.GitHubDotcomAPI)
		}
	}
	c := config.Connection{URL: "https://ghe.example.test"}
	if got, api := c.Instance(), githubrest.APIBase(c.URL, c.APIURL); got != "https://ghe.example.test" || api != "https://ghe.example.test/api/v3" {
		t.Errorf("url %q: Instance() = %q, APIBase = %q, want the instance itself and its /api/v3", c.URL, got, api)
	}
}
