package collect

import (
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/forge"
)

func TestScan_a_pull_request_run_on_a_branch_named_like_the_default_is_not_a_default_branch_run(t *testing.T) {
	ep := githubEndpoint("gh")
	pr := run(1, forge.RunFailing, now.Add(-time.Hour))
	pr.Trigger = "pull_request"
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{pr}}}
	h := newHarness(t, ep)
	h.scan(t)
	if l := lineFor(t, h.rec, "ci run", "gh"); l["default_branch"] != "false" {
		t.Errorf("ci run of a pull_request run on main: default_branch = %q, want false", l["default_branch"])
	}
	if n := h.rec.CountExact("failing workflow"); n != 0 {
		t.Errorf("a failing pull_request run on a branch named main logged %d failing workflow line(s), want 0", n)
	}
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["failing_workflows"] != "0" {
		t.Errorf("scan complete failing_workflows = %s, want 0", c["failing_workflows"])
	}
}

// moveDefault changes the one repository's default branch, as a rename on
// the forge does.
func moveDefault(ep *endpoint, branch string) {
	ep.conn.repos["o"][0].DefaultBranch = branch
}

func TestScan_a_default_branch_change_drops_the_old_branchs_failure(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	h := newHarness(t, ep)
	h.scan(t)
	lineFor(t, h.rec, "failing workflow", "gh")
	moveDefault(ep, "trunk")
	ep.conn.runs = map[string]listing{}
	h.scan(t)
	if n := h.rec.CountExact("failing workflow"); n != 0 {
		t.Errorf("after the default branch moved to trunk, %d failing workflow line(s) from main, want 0", n)
	}
	h.restart(t)
	h.scan(t)
	if n := h.rec.CountExact("failing workflow"); n != 0 {
		t.Errorf("after a restart on trunk, %d failing workflow line(s) from main, want 0", n)
	}
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["failing_workflows"] != "0" {
		t.Errorf("scan complete failing_workflows = %s, want 0", c["failing_workflows"])
	}
}

func TestScan_the_first_failure_on_a_new_default_branch_starts_a_new_streak(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-2*time.Hour))}}}
	h := newHarness(t, ep)
	h.scan(t)
	moveDefault(ep, "trunk")
	trunk := run(2, forge.RunFailing, now.Add(-time.Hour))
	trunk.Branch = "trunk"
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{trunk}}}
	h.scan(t)
	l := lineFor(t, h.rec, "failing workflow", "gh")
	if l["branch"] != "trunk" || l["consecutive_failures"] != "1" || l["failing_since"] != "2026-10-07T11:00:00Z" {
		t.Errorf("first trunk failure = branch %q streak %q since %q, want trunk, 1, 11:00", l["branch"], l["consecutive_failures"], l["failing_since"])
	}
}

// ageOut moves the clock past the lookback and scans with no runs listed, so
// every stored run is pruned and only the workflow verdicts remain.
func ageOut(t *testing.T, h *harness, ep *endpoint) {
	t.Helper()
	*h.clock = h.clock.Add(5 * 24 * time.Hour)
	ep.conn.runs = map[string]listing{}
	h.scan(t)
}

func TestScan_a_newer_pass_read_while_its_branch_was_not_the_default_clears_it_when_it_is_again(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	h := newHarness(t, ep)
	h.scan(t)
	lineFor(t, h.rec, "failing workflow", "gh")
	moveDefault(ep, "trunk")
	*h.clock = now.Add(2 * time.Hour)
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(2, forge.RunPassing, now.Add(time.Hour))}}}
	h.scan(t)
	ageOut(t, h, ep)
	moveDefault(ep, "main")
	h.scan(t)
	if n := h.rec.CountExact("failing workflow"); n != 0 {
		t.Errorf("main back as the default after its newer passing run aged out: %d failing workflow line(s), want 0", n)
	}
}

func TestScan_a_failure_read_while_its_branch_was_not_the_default_shows_once_it_is(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunPassing, now.Add(-time.Hour))}}}
	h := newHarness(t, ep)
	h.scan(t)
	trunk := run(2, forge.RunFailing, now.Add(time.Hour))
	trunk.Branch = "trunk"
	*h.clock = now.Add(2 * time.Hour)
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{trunk}}}
	h.scan(t)
	ageOut(t, h, ep)
	moveDefault(ep, "trunk")
	h.scan(t)
	if l := lineFor(t, h.rec, "failing workflow", "gh"); l["branch"] != "trunk" || l["run_id"] != "2" {
		t.Errorf("trunk made the default after its failing run aged out = %v, want run 2 failing on trunk", l)
	}
}

func TestScan_slow_workflows_leave_out_runs_of_a_former_default_branch(t *testing.T) {
	ep := githubEndpoint("gh")
	var runs []forge.Run
	for i := range 3 {
		runs = append(runs, run(int64(i+1), forge.RunPassing, now.Add(-time.Duration(i+1)*time.Hour)))
	}
	ep.conn.runs = map[string]listing{"o/r": {Runs: runs}}
	h := newHarness(t, ep)
	h.scan(t)
	lineFor(t, h.rec, "slow workflow", "gh")
	moveDefault(ep, "trunk")
	h.scan(t)
	if n := h.rec.CountExact("slow workflow"); n != 0 {
		t.Errorf("after the default branch moved to trunk, %d slow workflow line(s) from main runs, want 0", n)
	}
	if c := lineFor(t, h.rec, "scan complete", "gh"); c["run_durations_known"] != "0" {
		t.Errorf("run_durations_known = %s, want 0: no timed run is on trunk", c["run_durations_known"])
	}
}

func TestScan_slow_workflow_links_the_newest_run_on_a_creation_time_tie(t *testing.T) {
	ep := githubEndpoint("gh")
	at := now.Add(-time.Hour)
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{
		run(1, forge.RunPassing, now.Add(-2*time.Hour)), run(2, forge.RunPassing, at), run(3, forge.RunPassing, at),
	}}}
	for range 20 {
		h := newHarness(t, ep)
		h.scan(t)
		if l := lineFor(t, h.rec, "slow workflow", "gh"); l["url"] != "https://forge.test/o/r/runs/3" {
			t.Fatalf("slow workflow url with runs 2 and 3 created together = %q, want run 3's", l["url"])
		}
	}
}

// TestScan_a_workflow_a_whole_github_listing_no_longer_defines_leaves_the_failing_set
// stores a failing CI on main, then lists the repository's workflows a week
// later, past the lookback, in each shape a listing can take. first, when
// set, is the listing the first scan reads; else it defines CI.
func TestScan_a_workflow_a_whole_github_listing_no_longer_defines_leaves_the_failing_set(t *testing.T) {
	dynamic := func(gh *fakeGitHub) {
		gh.workflows = map[string][]forge.Workflow{"o/r": {{Repo: "o/r", Name: "CodeQL - Code Quality", State: "active"}}}
	}
	tests := []struct {
		first   func(gh *fakeGitHub)
		setup   func(gh *fakeGitHub)
		name    string
		failing bool
	}{
		{name: "never_listed_under_its_run_name", failing: true, first: dynamic, setup: dynamic},
		{name: "never_listed_then_deleted", first: dynamic, setup: func(gh *fakeGitHub) {
			gh.workflows = map[string][]forge.Workflow{"o/r": {{Repo: "o/r", Name: "CI", State: "deleted"}}}
		}},
		{name: "renamed", setup: func(gh *fakeGitHub) {
			gh.workflows = map[string][]forge.Workflow{"o/r": {{Repo: "o/r", Name: "Build", State: "active"}}}
		}},
		{name: "deleted_state", setup: func(gh *fakeGitHub) {
			gh.workflows = map[string][]forge.Workflow{"o/r": {{Repo: "o/r", Name: "CI", State: "deleted"}}}
		}},
		{name: "no_workflows_left", setup: func(gh *fakeGitHub) {
			gh.workflows = map[string][]forge.Workflow{"o/r": {}}
		}},
		{name: "still_defined", failing: true, setup: func(gh *fakeGitHub) {
			gh.workflows = map[string][]forge.Workflow{"o/r": {{Repo: "o/r", Name: "CI", State: "disabled_manually"}}}
		}},
		{name: "listing_cut", failing: true, setup: func(gh *fakeGitHub) {
			gh.workflows = map[string][]forge.Workflow{"o/r": {{Repo: "o/r", Name: "Build", State: "active"}}}
			gh.truncated = map[string]bool{"workflows o/r": true}
		}},
		{name: "listing_failed", failing: true, setup: func(gh *fakeGitHub) {
			gh.errs = map[string]error{"workflows o/r": errors.New("upstream 502")}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := githubEndpoint("gh")
			ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
			if tc.first != nil {
				tc.first(ep.gh)
			}
			h := newHarness(t, ep)
			h.scan(t)
			lineFor(t, h.rec, "failing workflow", "gh")
			*h.clock = now.Add(7 * 24 * time.Hour)
			ep.conn.runs["o/r"] = listing{}
			tc.setup(ep.gh)
			h.scan(t)
			got := lines(h.rec, "failing workflow")
			if tc.failing && (len(got) != 1 || got[0]["last_run_at"] != "2026-10-07T11:00:00Z") {
				t.Errorf("%s: failing workflow lines = %v, want CI still failing, last run 11:00 a week ago", tc.name, got)
			}
			if !tc.failing && len(got) != 0 {
				t.Errorf("%s: failing workflow lines = %v, want none: a whole listing proves CI gone", tc.name, got)
			}
		})
	}
}

// TestScan_a_workflow_a_whole_listing_once_named_deleted_stays_gone_after_its_row_leaves
// stores a failing CI on main whose name no active definition matches, then
// lists CI deleted, then lists no CI row at all; a dynamic workflow's name
// that no listing ever names stays failing throughout.
func TestScan_a_workflow_a_whole_listing_once_named_deleted_stays_gone_after_its_row_leaves(t *testing.T) {
	dynamic := forge.Workflow{Repo: "o/r", Name: "CodeQL - Code Quality", State: "active"}
	tests := []struct {
		name    string
		second  []forge.Workflow
		failing bool
	}{
		{name: "deleted_then_absent", second: []forge.Workflow{dynamic, {Repo: "o/r", Name: "CI", State: "deleted"}}},
		{name: "never_named", second: []forge.Workflow{dynamic}, failing: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := githubEndpoint("gh")
			ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
			ep.gh.workflows = map[string][]forge.Workflow{"o/r": {dynamic}}
			h := newHarness(t, ep)
			h.scan(t)
			lineFor(t, h.rec, "failing workflow", "gh")
			ep.gh.workflows["o/r"] = tc.second
			*h.clock = now.Add(15 * time.Minute)
			h.scan(t)
			ep.gh.workflows["o/r"] = []forge.Workflow{dynamic}
			*h.clock = now.Add(30 * time.Minute)
			h.scan(t)
			if got := lines(h.rec, "failing workflow"); (len(got) == 1) != tc.failing {
				t.Errorf("%s: third scan failing workflow lines = %v, want CI failing %t", tc.name, got, tc.failing)
			}
		})
	}
}

// TestScan_a_forge_with_no_workflow_listing_keeps_an_old_failure_with_its_last_run_time
// runs the same week on Gitea, which lists no workflow definitions.
func TestScan_a_forge_with_no_workflow_listing_keeps_an_old_failure_with_its_last_run_time(t *testing.T) {
	ep := giteaEndpoint("gt")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	h := newHarness(t, ep)
	h.scan(t)
	*h.clock = now.Add(7 * 24 * time.Hour)
	ep.conn.runs["o/r"] = listing{}
	h.scan(t)
	if l := lineFor(t, h.rec, "failing workflow", "gt"); l["run_id"] != "1" || l["last_run_at"] != "2026-10-07T11:00:00Z" {
		t.Errorf("gitea failing workflow a week later = %v, want run 1 still listed with its last run at 11:00", l)
	}
}

// dayLines is the last scan's ci day lines of connection conn, as "day repo"
// to "passing failing neutral clipped".
func dayLines(h *harness, conn string) map[string]string {
	out := map[string]string{}
	for _, l := range lines(h.rec, "ci day") {
		if l["connection"] == conn {
			out[l["day"]+" "+l["repo"]] = l["passing"] + " " + l["failing"] + " " + l["neutral"] + " " + l["clipped"]
		}
	}
	return out
}

// TestScan_ci_day_counts_each_run_once_on_the_day_it_was_created pins what
// the day charts read: every scan logs each retained day of each listed
// repository, zeros included, from the run state, so a second scan logs the
// same counts and a run logged twice counts once.
func TestScan_ci_day_counts_each_run_once_on_the_day_it_was_created(t *testing.T) {
	ep := githubEndpoint("gh")
	other := run(4, forge.RunFailing, now.Add(-26*time.Hour))
	other.Repo = "o/s"
	untimed := run(5, forge.RunPassing, now.Add(-2*time.Hour))
	untimed.StartedAt = time.Time{}
	pr := run(6, forge.RunPassing, now.Add(-2*time.Hour))
	pr.Trigger = forge.TriggerPullRequest
	ep.conn.repos["o"] = append(ep.conn.repos["o"], forge.Repo{Path: "o/s", DefaultBranch: "main"})
	ep.conn.runs = map[string]listing{
		"o/r": {Runs: []forge.Run{
			run(1, forge.RunFailing, now.Add(-50*time.Hour)), run(2, forge.RunPassing, now.Add(-26*time.Hour)),
			run(3, forge.RunNeutral, now.Add(-26*time.Hour)), untimed, pr,
		}},
		"o/s": {Runs: []forge.Run{other}},
	}
	h := newHarness(t, ep)
	// The 72h listing starts at 10-04 12:00, so that day counts at least.
	want := map[string]string{
		"2026-10-04 o/r": "0 0 0 true", "2026-10-04 o/s": "0 0 0 true", "2026-10-05 o/r": "0 1 0 false",
		"2026-10-06 o/r": "1 0 1 false", "2026-10-06 o/s": "0 1 0 false", "2026-10-07 o/r": "2 0 0 false",
	}
	for scan := range 2 {
		*h.clock = now.Add(time.Duration(scan) * 15 * time.Minute)
		h.scan(t)
		if got := dayLines(h, "gh"); !maps.Equal(got, want) {
			t.Errorf("scan %d ci day = %v, want %v", scan+1, got, want)
		}
	}
	for _, l := range lines(h.rec, "ci day") {
		if l["day"] == "2026-10-07" && (l["timed_runs"] != "0" || l["run_seconds"] != "0") {
			t.Errorf("ci day of 10-07 = %v, want no timed run: one has no start time, the other a pull request's", l)
		}
		if l["day"] == "2026-10-06" && l["repo"] == "o/r" && (l["timed_runs"] != "2" || l["run_seconds"] != "580") {
			t.Errorf("ci day of 10-06 o/r = %v, want 2 timed runs of 580 s", l)
		}
		if l["scan_id"] == "" || l["rank"] == "" || l["forge_rank"] == "" {
			t.Errorf("ci day line %v carries no scan_id, rank or forge_rank", l)
		}
	}
}

func TestScan_ci_day_moves_a_rerun_from_failed_to_passed(t *testing.T) {
	ep := githubEndpoint("gh")
	failed := run(1, forge.RunFailing, now.Add(-2*time.Hour))
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{failed}}}
	h := newHarness(t, ep)
	h.scan(t)
	rerun := failed
	rerun.State, rerun.UpdatedAt = forge.RunPassing, now.Add(14*time.Minute)
	ep.conn.runs["o/r"] = listing{Runs: []forge.Run{rerun}}
	*h.clock = now.Add(15 * time.Minute)
	h.scan(t)
	if got := dayLines(h, "gh")["2026-10-07 o/r"]; got != "1 0 0 false" {
		t.Errorf("ci day of 10-07 after run 1 failed then passed = %q, want 1 passing and 0 failing", got)
	}
}

func TestScan_ci_day_a_lookback_under_a_day_still_counts_whole_days(t *testing.T) {
	ep := githubEndpoint("gh")
	day := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	created := []forge.Run{
		run(1, forge.RunPassing, day.Add(time.Hour)), run(2, forge.RunFailing, day.Add(9*time.Hour)),
		run(3, forge.RunPassing, day.Add(17*time.Hour)), run(4, forge.RunNeutral, day.Add(23*time.Hour+40*time.Minute)),
	}
	h := newHarness(t, ep)
	h.lookback = time.Hour
	*h.clock = day.Add(-30 * time.Minute)
	h.build(t)
	for ; h.clock.Before(day.Add(24*time.Hour + 30*time.Minute)); *h.clock = h.clock.Add(15 * time.Minute) {
		// The forge holds only the runs created by now.
		var existing []forge.Run
		for _, r := range created {
			if !r.CreatedAt.After(*h.clock) {
				existing = append(existing, r)
			}
		}
		ep.conn.runs = map[string]listing{"o/r": {Runs: existing}}
		h.scan(t)
	}
	if got := dayLines(h, "gh")["2026-10-07 o/r"]; got != "2 1 1 false" {
		t.Errorf("ci day of 10-07 from 1h listings every 15 minutes = %q, want every run of the day, not clipped", got)
	}
}

func TestScan_a_stopped_connection_logs_no_ci_day(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-2*time.Hour))}}}
	ep.gh.errs = map[string]error{"alerts o/r": forge.ErrReadDeferred}
	h := newHarness(t, ep)
	h.scan(t)
	if l := lineFor(t, h.rec, "scan stopped", "gh"); l["runs_read"] != "complete" {
		t.Fatalf("Setup: the scan stopped with runs_read %q, want the runs read before the stop", l["runs_read"])
	}
	if n := h.rec.CountExact("ci day"); n != 0 {
		t.Errorf("a stopped scan logged %d ci day line(s), want 0", n)
	}
}

func TestScan_ci_day_skips_a_repository_whose_listing_failed(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-2*time.Hour))}}}
	h := newHarness(t, ep)
	h.scan(t)
	ep.conn.errs = map[string]error{"runs o/r": errors.New("boom")}
	*h.clock = now.Add(15 * time.Minute)
	h.scan(t)
	if n := h.rec.CountExact("ci day"); n != 0 {
		t.Errorf("a scan whose only run listing failed logged %d ci day line(s), want 0: the last scan's counts stand", n)
	}
}
