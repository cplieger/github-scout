package collect

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/forge"
)

func TestScan_a_repository_whose_run_read_failed_publishes_no_workflow_rows(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.repos["o"] = append(ep.conn.repos["o"], forge.Repo{Path: "o/b", DefaultBranch: "main"})
	var timedFailures []forge.Run
	for i := range minDurationRuns {
		timedFailures = append(timedFailures, run(int64(i+1), forge.RunFailing, now.Add(-time.Duration(i+3)*time.Hour)))
	}
	b := run(10, forge.RunFailing, now.Add(-3*time.Hour))
	b.Repo = "o/b"
	ep.conn.runs = map[string]listing{"o/r": {Runs: timedFailures}, "o/b": {Runs: []forge.Run{b}}}
	h := newHarness(t, ep)
	h.scan(t)
	if n := h.rec.CountExact("failing workflow"); n != 2 {
		t.Fatalf("Setup: first scan failing workflow lines = %d, want o/r's and o/b's", n)
	}
	lineFor(t, h.rec, "slow workflow", "gh")
	passed := run(11, forge.RunPassing, now.Add(-time.Hour))
	passed.Repo = "o/b"
	ep.conn.runs["o/b"] = listing{Runs: []forge.Run{b, passed}}
	ep.conn.errs = map[string]error{"runs o/r": errors.New("upstream 502")}
	h.scan(t)
	for _, msg := range []string{"failing workflow", "slow workflow"} {
		if got := lines(h.rec, msg); len(got) != 0 {
			t.Errorf("%q lines after o/r's run read failed and o/b passed = %v, want none: o/r's stored rows were not read this scan", msg, got)
		}
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	if c["failing_workflows"] != "0" || c["failing_workflows_read"] != "partial" || c["run_durations_known"] != "2" {
		t.Errorf("scan complete failing_workflows %q read %q run_durations_known %q, want 0, partial, and o/b's 2",
			c["failing_workflows"], c["failing_workflows_read"], c["run_durations_known"])
	}
}

func TestScan_a_cut_discovery_publishes_no_snapshot_read_through_it(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.truncated = true
	ep.conn.prs = map[string][]forge.PullRequest{"o": {
		{Repo: "o/r", Number: 1, Author: "x", HeadSHA: "h1"}, {Repo: "o/b", Number: 2, Author: "x"},
	}}
	ep.conn.issues = map[string][]forge.Issue{"o": {{Repo: "o/b", Number: 3, Author: "x"}}}
	ep.gh.alerts = map[string][]forge.Alert{"o/r": {{Repo: "o/r", Number: 4, Severity: "high", Tool: "CodeQL"}}}
	ep.gh.workflows = map[string][]forge.Workflow{"o/r": {{Repo: "o/r", Name: "nightly", State: "disabled_inactivity"}}}
	var timedFailures []forge.Run
	for i := range minDurationRuns {
		timedFailures = append(timedFailures, run(int64(i+1), forge.RunFailing, now.Add(-time.Duration(i+3)*time.Hour)))
	}
	ep.conn.runs = map[string]listing{"o/r": {Runs: timedFailures}}
	h := newHarness(t, ep)
	h.scan(t)
	for _, msg := range []string{
		"open pull request", "open issue", "security alert", "top security alert", "security tool", "disabled workflow",
		"failing workflow", "slow workflow",
	} {
		if got := lines(h.rec, msg); len(got) != 0 {
			t.Errorf("%q lines with the repository listing cut = %v, want none: a prefix of the repositories is not the snapshot", msg, got)
		}
	}
	if slices.Contains(ep.conn.calls, "checks h1") {
		t.Error("the head checks of pull requests the scan cannot publish were read")
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	for _, k := range []string{
		"open_prs", "excluded_prs", "from_others_prs", "open_issues", "excluded_issues", "from_others_issues",
		"open_age_lt7d", "idle_30d", "failing_checks_prs", "checks_unread", "security_alerts", "security_alerts_high",
		"disabled_workflows", "failing_workflows", "failing_workflows_listed", "slow_workflows_listed", "run_durations_known",
	} {
		if v, ok := c[k]; ok {
			t.Errorf("scan complete with the repository listing cut carries %s = %s, want it absent, never a count of a prefix", k, v)
		}
	}
	for _, k := range []string{
		"repos_truncated", "security_repos_read", "security_alerts_read", "disabled_workflows_read", "failing_workflows_read",
		"new_runs", "new_failures", "tracked",
	} {
		if _, ok := c[k]; !ok {
			t.Errorf("scan complete with the repository listing cut lacks %s, want the coverage kept", k)
		}
	}
	if c["degraded"] != "true" || c["repos_truncated"] != "true" {
		t.Errorf("scan complete degraded %q repos_truncated %q, want true and true", c["degraded"], c["repos_truncated"])
	}
	if n := h.rec.CountExact("ci run"); n != minDurationRuns {
		t.Errorf("ci run lines with the repository listing cut = %d, want each of the %d runs read: a run event is not a snapshot", n, minDurationRuns)
	}
	noLineFor(t, h.rec, "scan degraded", "gh")
}

func TestScan_a_stopped_connection_is_an_incomplete_scan(t *testing.T) {
	stops := []struct {
		name string
		err  error
	}{
		{name: "rate_limited", err: forge.ErrRateLimited},
		{name: "read_deferred", err: forge.ErrReadDeferred},
	}
	for _, stop := range stops {
		for _, at := range []struct {
			setup func(ep *endpoint, err error)
			name  string
		}{
			{name: "open", setup: func(ep *endpoint, err error) { ep.openErr = fmt.Errorf("identify account: %w", err) }},
			{name: "discovery", setup: func(ep *endpoint, err error) { ep.conn.errs = map[string]error{"discover o": err} }},
			{name: "runs", setup: func(ep *endpoint, err error) { ep.conn.errs = map[string]error{"runs o/r": err} }},
		} {
			t.Run(at.name+"_"+stop.name, func(t *testing.T) {
				ep := githubEndpoint("gh")
				at.setup(ep, stop.err)
				if got := newHarness(t, ep).scan(t); got != Incomplete {
					t.Errorf("Scan with the %s read stopped (%s) = %s, want incomplete: the signals after it were not read", at.name, stop.name, got)
				}
			})
		}
	}
}

func TestScan_scan_stopped_says_whether_the_scan_degraded(t *testing.T) {
	tests := []struct {
		setup     func(ep *endpoint)
		name      string
		want      string
		cause     string
		escalated int
	}{
		{name: "budget_reserve", want: "false", setup: func(ep *endpoint) {
			ep.conn.errs = map[string]error{"runs o/r": forge.ErrReadDeferred}
		}},
		{name: "rate_limited", want: "true", escalated: 1, cause: "rate_limited", setup: func(ep *endpoint) {
			ep.conn.errs = map[string]error{"runs o/r": forge.ErrRateLimited}
		}},
		{name: "past_its_limit", want: "true", escalated: 1, cause: "scan_timeout", setup: func(ep *endpoint) {
			ep.conn.block = blockUntilDone("runs o/r", 5*time.Second)
		}},
		{name: "budget_reserve_after_a_blind_read", want: "true", escalated: 1, cause: "signal_blind", setup: func(ep *endpoint) {
			ep.conn.errs = map[string]error{"issues o": errors.New("upstream answered 502"), "runs o/r": forge.ErrReadDeferred}
		}},
		{name: "budget_reserve_after_a_cut_listing", want: "true", setup: func(ep *endpoint) {
			ep.conn.repos = map[string][]forge.Repo{"o": {{Path: "o/a", DefaultBranch: "main"}, {Path: "o/r", DefaultBranch: "main"}}}
			ep.conn.runs = map[string]listing{"o/a": {Runs: []forge.Run{run(1, forge.RunPassing, now.Add(-time.Hour))}, Truncated: true}}
			ep.conn.errs = map[string]error{"runs o/r": forge.ErrReadDeferred}
		}},
		{name: "budget_reserve_after_a_failed_checks_read", want: "true", setup: func(ep *endpoint) {
			ep.conn.prs = map[string][]forge.PullRequest{"o": {
				{Repo: "o/r", Number: 1, Ref: "#1", Author: "x", HeadSHA: "h1"},
				{Repo: "o/r", Number: 2, Ref: "#2", Author: "x", HeadSHA: "h2"},
			}}
			ep.conn.errs = map[string]error{"checks h1": errors.New("upstream answered 502"), "checks h2": forge.ErrReadDeferred}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := githubEndpoint("gh")
			tc.setup(ep)
			h := newLimitedHarness(t, 2*time.Second, ep)
			h.scan(t)
			if s := lineFor(t, h.rec, "scan stopped", "gh"); s["degraded"] != tc.want {
				t.Errorf("scan stopped (%s) degraded = %q, want %q; line %v", tc.name, s["degraded"], tc.want, s)
			}
			if got := h.rec.CountExact("scan degraded"); got != tc.escalated {
				t.Errorf("scan stopped (%s) logged %d scan degraded lines, want %d; messages %v", tc.name, got, tc.escalated, h.rec.Messages())
			}
			if tc.escalated == 1 {
				if l := lineFor(t, h.rec, "scan degraded", "gh"); l["cause"] != tc.cause {
					t.Errorf("scan degraded after a %s stop has cause %q, want %q", tc.name, l["cause"], tc.cause)
				}
			}
		})
	}
}

func TestScan_scan_complete_and_scan_stopped_name_the_scan_interval(t *testing.T) {
	done, stopped := githubEndpoint("done"), githubEndpoint("stopped")
	stopped.conn.errs = map[string]error{"runs o/r": forge.ErrReadDeferred}
	h := newHarness(t, done, stopped)
	h.interval = 25 * time.Minute
	h.build(t)
	h.scan(t)
	if c := lineFor(t, h.rec, "scan complete", "done"); c["scan_interval_s"] != "1500" {
		t.Errorf("scan complete scan_interval_s = %q at a 25m scan_interval, want %q", c["scan_interval_s"], "1500")
	}
	if s := lineFor(t, h.rec, "scan stopped", "stopped"); s["scan_interval_s"] != "1500" {
		t.Errorf("scan stopped scan_interval_s = %q at a 25m scan_interval, want %q", s["scan_interval_s"], "1500")
	}
}

func TestScan_owner_fallback_reads_the_repositories_discovery_returned_for_that_owner(t *testing.T) {
	for _, owner := range []string{"123", "Some-Group"} {
		t.Run(owner, func(t *testing.T) {
			ep := githubEndpoint("gl")
			ep.conn.product = forge.ProductGitLab
			ep.gh = nil
			ep.cfg.Owners = []string{owner}
			ep.conn.repos = map[string][]forge.Repo{owner: {{Path: "canonical/repo", DefaultBranch: "main"}}}
			ep.conn.errs = map[string]error{"prs " + owner: forge.ErrOwnerUnresolved, "issues " + owner: forge.ErrOwnerUnresolved}
			ep.conn.prs = map[string][]forge.PullRequest{"canonical/repo": {{Repo: "canonical/repo", Number: 1, Ref: "!1", Author: "x"}}}
			ep.conn.issues = map[string][]forge.Issue{"canonical/repo": {{Repo: "canonical/repo", Number: 2, Author: "x"}}}
			h := newHarness(t, ep)
			h.scan(t)
			if n, m := h.rec.CountExact("open pull request"), h.rec.CountExact("open issue"); n != 1 || m != 1 {
				t.Errorf("owner %q whose repository path is canonical/repo: %d pull request and %d issue lines, want 1 and 1; calls %v",
					owner, n, m, ep.conn.calls)
			}
			if c := lineFor(t, h.rec, "scan complete", "gl"); c["open_prs"] != "1" || c["open_issues"] != "1" {
				t.Errorf("scan complete open_prs %q open_issues %q, want 1 and 1", c["open_prs"], c["open_issues"])
			}
			if !strings.Contains(strings.Join(ep.conn.calls, ","), "repo-prs canonical/repo") {
				t.Errorf("calls %v, want the per-project fallback over canonical/repo", ep.conn.calls)
			}
		})
	}
}

// TestScan_a_cut_run_listing_judges_what_it_read_and_vouches_for_nothing_older
// stores o/r's failing CI, then lists o/r cut short: once with a newer CI
// failure in its rows, once with only another workflow's newer run.
func TestScan_a_cut_run_listing_judges_what_it_read_and_vouches_for_nothing_older(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.repos["o"] = append(ep.conn.repos["o"], forge.Repo{Path: "o/b", DefaultBranch: "main"})
	first := run(1, forge.RunFailing, now.Add(-3*time.Hour))
	b := run(10, forge.RunFailing, now.Add(-3*time.Hour))
	b.Repo = "o/b"
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{first}}, "o/b": {Runs: []forge.Run{b}}}
	h := newHarness(t, ep)
	h.scan(t)
	if n := h.rec.CountExact("failing workflow"); n != 2 {
		t.Fatalf("Setup: first scan failing workflow lines = %d, want o/r's and o/b's", n)
	}
	second := run(2, forge.RunFailing, now.Add(-5*time.Minute))
	ep.conn.runs["o/r"] = listing{Runs: []forge.Run{second}, Truncated: true}
	*h.clock = now.Add(15 * time.Minute)
	h.scan(t)
	byRepo := map[string]map[string]string{}
	for _, l := range lines(h.rec, "failing workflow") {
		byRepo[l["repo"]] = l
	}
	if l := byRepo["o/r"]; l["run_id"] != "2" || l["state"] != "failing" || l["consecutive_failures"] != "1" || l["failing_since_clipped"] != "true" {
		t.Errorf("cut listing with a newer failure: o/r failing workflow = %v, want run 2 failing, streak 1 clipped: the runs below the cut are unknown", l)
	}
	if l := byRepo["o/b"]; l["run_id"] != "10" || l["state"] != "failing" {
		t.Errorf("cut listing of o/r: o/b failing workflow = %v, want run 10 failing, whose listing was whole", l)
	}
	if l := lineFor(t, h.rec, "ci run", "gh"); l["run_id"] != "2" {
		t.Errorf("cut listing: ci run = %v, want run 2 logged", l)
	}
	deploy := run(3, forge.RunPassing, now.Add(20*time.Minute))
	deploy.Workflow = "Deploy"
	ep.conn.runs["o/r"] = listing{Runs: []forge.Run{deploy}, Truncated: true}
	*h.clock = now.Add(30 * time.Minute)
	h.scan(t)
	for _, l := range lines(h.rec, "failing workflow") {
		if l["repo"] == "o/r" && (l["run_id"] != "2" || l["state"] != "unknown") {
			t.Errorf("cut listing with only a newer Deploy run: o/r failing workflow = %v, want run 2 read unknown: CI may have run below the cut", l)
		}
	}
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["runs_truncated_repos"] != "1" || c["degraded"] != "true" {
		t.Errorf("scan complete after a cut run listing = %v, want 1 truncated runs repo, degraded", c)
	}
}

// TestScan_a_newer_run_only_partial_listings_read_never_restores_an_older_failure
// stores o/r's failing CI, then reads a newer passing run only through
// partial listings until it leaves the lookback, then lists o/r whole.
func TestScan_a_newer_run_only_partial_listings_read_never_restores_an_older_failure(t *testing.T) {
	tests := []struct {
		name    string
		product forge.Product
		list    func(runs []forge.Run) listing
	}{
		{name: "gitea_truncated_daily", product: forge.ProductGitea, list: func(runs []forge.Run) listing { return listing{Runs: runs, Truncated: true} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := githubEndpoint("gh")
			ep.conn.product = tc.product
			if tc.product != forge.ProductGitHub {
				ep.gh = nil
			}
			ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
			h := newHarness(t, ep)
			h.scan(t)
			lineFor(t, h.rec, "failing workflow", "gh")
			passing := run(2, forge.RunPassing, now.Add(24*time.Hour))
			for day := 1; day <= 4; day++ {
				*h.clock = now.Add(time.Duration(day) * 24 * time.Hour)
				ep.conn.runs["o/r"] = tc.list([]forge.Run{passing})
				h.scan(t)
			}
			*h.clock = now.Add(5 * 24 * time.Hour)
			ep.conn.runs["o/r"] = listing{}
			h.scan(t)
			if got := lines(h.rec, "failing workflow"); len(got) != 0 {
				t.Errorf("%s: whole listing after run 2 aged out: failing workflow lines = %v, want none: run 1 failed before run 2 passed", tc.name, got)
			}
			if c := lineFor(t, h.rec, "scan complete", "gh"); c["failing_workflows"] != "0" {
				t.Errorf("%s: scan complete failing_workflows = %q, want 0", tc.name, c["failing_workflows"])
			}
		})
	}
}
