package runstate

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/atomicfile/v4"
	"github.com/cplieger/github-scout/internal/forge"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

const lookback = 72 * time.Hour

func run(id int64, state forge.RunState, created time.Time) *forge.Run {
	return &forge.Run{
		ID: id, Number: id, Repo: "o/r", Workflow: "CI", Branch: "main", Trigger: "push", State: state,
		URL: "https://forge.test/o/r/runs/1", CreatedAt: created, StartedAt: created.Add(10 * time.Second),
		UpdatedAt: created.Add(5 * time.Minute),
	}
}

// scanAt is a scan started at start that listed each run's repository whole
// over the lookback window.
func scanAt(start time.Time, runs ...*forge.Run) *Read {
	r := &Read{Start: start, Window: start.Add(-lookback)}
	at := map[string]int{}
	for _, run := range runs {
		i, ok := at[run.Repo]
		if !ok {
			i = len(r.Listings)
			at[run.Repo] = i
			r.Listings = append(r.Listings, Listing{Repo: run.Repo, Cover: r.Window})
		}
		r.Listings[i].Runs = append(r.Listings[i].Runs, Observation{Run: *run})
	}
	return r
}

// read is a scan at t0 listing runs.
func read(runs ...*forge.Run) *Read { return scanAt(t0, runs...) }

// cutAt is a scan at start whose listing of o/r was cut short with its
// oldest row created at oldest.
func cutAt(start, oldest time.Time, runs ...*forge.Run) *Read {
	r := scanAt(start, runs...)
	if len(r.Listings) == 0 {
		r.Listings = []Listing{{Repo: "o/r"}}
	}
	r.Listings[0].Cut, r.Listings[0].Cover = true, oldest
	return r
}

// withDefault marks every observation of r as of its default branch.
func withDefault(r *Read) *Read {
	for i := range r.Listings {
		for j := range r.Listings[i].Runs {
			r.Listings[i].Runs[j].OnDefault = true
		}
	}
	return r
}

// open opens the store in dir and closes it when the test ends.
func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%s) = %v", dir, err)
	}
	t.Cleanup(s.Close)
	return s
}

// commit commits r on conn and returns what it delivered, failing the test on
// an error.
func commit(t *testing.T, s *Store, conn string, r *Read) []Change {
	t.Helper()
	got, err := commitErr(t, s, conn, r)
	if err != nil {
		t.Fatalf("Commit = %v", err)
	}
	return got
}

// commitErr commits r on conn and returns what it delivered and its error.
func commitErr(t *testing.T, s *Store, conn string, r *Read) ([]Change, error) {
	t.Helper()
	var got []Change
	err := s.Commit(t.Context(), conn, r, func(ch []Change) bool { got = append(got, ch...); return true })
	return got, err
}

// reload closes s and returns the directory's store loaded from the file, as
// a restart does.
func reload(t *testing.T, s *Store) *Store {
	t.Helper()
	s.Close()
	next := open(t, s.dir)
	if d, err := next.Load(t.Context()); d != nil || err != nil {
		t.Fatalf("Load = %+v, %v, want the file accepted", d, err)
	}
	return next
}

func inRepo(r *forge.Run, repo string) *forge.Run {
	r.Repo = repo
	return r
}

// verdictOf is the verdict of repo's workflow name on branch, ok false when
// the store holds none.
func verdictOf(wfs iter.Seq2[WorkflowKey, WorkflowState], repo, name, branch string) (WorkflowState, bool) {
	for k, w := range wfs {
		if k == (WorkflowKey{Repo: repo, Name: name, Branch: branch}) {
			return w, true
		}
	}
	return WorkflowState{}, false
}

// workflow is the verdict of o/r's CI on main, failing the test when there
// is none.
func workflow(t *testing.T, s *Store) WorkflowState {
	t.Helper()
	return workflowOn(t, s, "o/r", "CI", "main")
}

func workflowOn(t *testing.T, s *Store, repo, name, branch string) WorkflowState {
	t.Helper()
	w, ok := verdictOf(s.Workflows("gh"), repo, name, branch)
	if !ok {
		t.Fatalf("Workflows has no entry for %s %s on %s", repo, name, branch)
	}
	return w
}

func TestCommit_reports_new_unchanged_and_changed_runs(t *testing.T) {
	s := open(t, t.TempDir())
	r := run(1, forge.RunFailing, t0)
	if got := commit(t, s, "gh", read(r)); len(got) != 1 || !got[0].New || got[0].Previous != "" {
		t.Errorf("Commit(first listing) = %+v, want run 1 new with no previous state", got)
	}
	if got := commit(t, s, "gh", read(r)); len(got) != 0 {
		t.Errorf("Commit(same run, same state and update time) = %+v, want no change", got)
	}
	r.State, r.UpdatedAt = forge.RunPassing, r.UpdatedAt.Add(time.Hour)
	if got := commit(t, s, "gh", read(r)); len(got) != 1 || got[0].New || got[0].Previous != forge.RunFailing {
		t.Errorf("Commit(re-run passing) = %+v, want a change from failing", got)
	}
	r.UpdatedAt = r.UpdatedAt.Add(time.Minute)
	if got := commit(t, s, "gh", read(r)); len(got) != 1 || got[0].Previous != forge.RunPassing {
		t.Errorf("Commit(update time moved, same state) = %+v, want a change with previous passing", got)
	}
	if n := s.Delivered("gh"); n != 1 {
		t.Errorf("Delivered = %d, want 1", n)
	}
	if got := commit(t, s, "other", read(r)); len(got) != 1 || !got[0].New {
		t.Errorf("Commit on a second connection = %+v, want run 1 new: connections share no run identity", got)
	}
}

func TestCommit_an_older_reading_of_a_delivered_run_changes_nothing(t *testing.T) {
	s := open(t, t.TempDir())
	newer := run(1, forge.RunPassing, t0)
	newer.UpdatedAt = newer.UpdatedAt.Add(time.Hour)
	commit(t, s, "gh", read(newer))
	if got := commit(t, s, "gh", read(run(1, forge.RunFailing, t0))); len(got) != 0 {
		t.Errorf("Commit(older reading of run 1) = %+v, want no change", got)
	}
	if w := workflow(t, s); w.State != forge.RunPassing {
		t.Errorf("verdict after an older reading = %+v, want the newer passing one kept", w)
	}
}

func TestCommit_run_ids_are_scoped_to_their_repository(t *testing.T) {
	alpha := inRepo(run(7, forge.RunFailing, t0), "alpha/repo")
	beta := inRepo(run(7, forge.RunPassing, t0.Add(time.Hour)), "beta/repo")
	s := open(t, t.TempDir())
	if got := commit(t, s, "gh", read(alpha, beta)); len(got) != 2 || !got[0].New || !got[1].New {
		t.Fatalf("Commit(alpha and beta run 7) = %+v, want both new: another repository's run 7 is a different run", got)
	}
	loaded := reload(t, s)
	if got := commit(t, loaded, "gh", read(alpha, beta)); len(got) != 0 || loaded.Delivered("gh") != 2 {
		t.Errorf("Commit after a reload = %+v with %d delivered, want no change and 2 delivered", got, loaded.Delivered("gh"))
	}
	if w := workflowOn(t, loaded, "alpha/repo", "CI", "main"); w.State != forge.RunFailing {
		t.Errorf("alpha/repo CI verdict = %s, want failing: beta/repo's passing run 7 is not alpha's", w.State)
	}
}

func TestCommit_refuses_a_run_the_store_cannot_hold(t *testing.T) {
	tests := []struct {
		name string
		edit func(*forge.Run)
	}{
		{"zero_id", func(r *forge.Run) { r.ID = 0 }},
		{"unknown_state", func(r *forge.Run) { r.State = "broken" }},
		{"no_creation_time", func(r *forge.Run) { r.CreatedAt = time.Time{} }},
		{"no_update_time", func(r *forge.Run) { r.UpdatedAt = time.Time{} }},
		{"updated_before_created", func(r *forge.Run) { r.UpdatedAt = r.CreatedAt.Add(-time.Second) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := run(1, forge.RunFailing, t0)
			tc.edit(r)
			if Storable(r) == nil {
				t.Errorf("Storable(%s) = nil, want a refusal", tc.name)
			}
			s := open(t, t.TempDir())
			if got := commit(t, s, "gh", read(r)); len(got) != 0 || s.Delivered("gh") != 0 {
				t.Errorf("Commit(%s) = %+v with %d delivered, want the run refused", tc.name, got, s.Delivered("gh"))
			}
			if _, ok := verdictOf(s.Workflows("gh"), "o/r", "CI", "main"); ok {
				t.Errorf("Commit(%s) judged the run, want no verdict", tc.name)
			}
		})
	}
}

func TestCommit_streak_counts_consecutive_default_branch_failures(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunPassing, t0.Add(-4*time.Hour))))
	commit(t, s, "gh", scanAt(t0.Add(time.Hour), run(2, forge.RunFailing, t0.Add(-3*time.Hour)), run(3, forge.RunFailing, t0.Add(-2*time.Hour))))
	commit(t, s, "gh", scanAt(t0.Add(2*time.Hour), run(4, forge.RunFailing, t0.Add(time.Hour))))
	w := workflow(t, s)
	if w.State != forge.RunFailing || w.Streak != 3 || !w.Since.Equal(t0.Add(-3*time.Hour)) || w.Clipped || w.RunID != 4 {
		t.Errorf("after pass, fail x3: verdict = %+v, want failing at run 4, streak 3 since run 2, not clipped", w)
	}
	commit(t, s, "gh", scanAt(t0.Add(3*time.Hour), run(5, forge.RunPassing, t0.Add(2*time.Hour))))
	if w := workflow(t, s); w.State != forge.RunPassing || w.Streak != 0 || !w.Since.IsZero() {
		t.Errorf("after a pass: verdict = %+v, want passing with no streak", w)
	}
}

func TestCommit_listing_the_same_runs_again_keeps_the_streak(t *testing.T) {
	s := open(t, t.TempDir())
	runs := []*forge.Run{run(1, forge.RunFailing, t0.Add(-2*time.Hour)), run(2, forge.RunFailing, t0.Add(-time.Hour))}
	for i := range 3 {
		commit(t, s, "gh", scanAt(t0.Add(time.Duration(i)*15*time.Minute), runs...))
	}
	if w := workflow(t, s); w.Streak != 2 || w.RunID != 2 || !w.Since.Equal(t0.Add(-2*time.Hour)) {
		t.Errorf("after three scans listing the same two failures = %+v, want streak 2 at run 2 since run 1", w)
	}
}

func TestCommit_a_re_run_of_the_verdicts_run_is_judged_against_the_streak_it_followed(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunPassing, t0.Add(-3*time.Hour)), run(2, forge.RunFailing, t0.Add(-2*time.Hour))))
	newest := run(3, forge.RunFailing, t0.Add(-time.Hour))
	commit(t, s, "gh", scanAt(t0.Add(time.Hour), newest))
	if w := workflow(t, s); w.Streak != 2 {
		t.Fatalf("Setup: streak = %d, want 2", w.Streak)
	}
	newest.State, newest.UpdatedAt = forge.RunPassing, newest.UpdatedAt.Add(time.Hour)
	commit(t, s, "gh", scanAt(t0.Add(2*time.Hour), newest))
	if w := workflow(t, s); w.State != forge.RunPassing || w.Streak != 0 || w.RunID != 3 {
		t.Errorf("after the newest run passed on a re-run: verdict = %+v, want passing at run 3", w)
	}
	next := reload(t, s)
	newest.State, newest.UpdatedAt = forge.RunFailing, newest.UpdatedAt.Add(time.Hour)
	commit(t, next, "gh", scanAt(t0.Add(3*time.Hour), newest))
	if w := workflow(t, next); w.State != forge.RunFailing || w.Streak != 2 || !w.Since.Equal(t0.Add(-2*time.Hour)) || w.Clipped {
		t.Errorf("after the re-run failed again across a reload: verdict = %+v, want failing, streak 2 since run 2, not clipped", w)
	}
}

func TestCommit_a_streak_no_listing_covered_the_start_of_is_clipped(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(7, forge.RunFailing, t0.Add(-2*time.Hour)), run(8, forge.RunFailing, t0.Add(-time.Hour))))
	if w := workflow(t, s); !w.Clipped || w.Streak != 2 || !w.Since.Equal(t0.Add(-2*time.Hour)) {
		t.Errorf("a streak from the store's first listing = %+v, want clipped, streak 2, since run 7", w)
	}
}

func TestCommit_a_coverage_gap_clips_the_streak_that_follows_it(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunPassing, t0.Add(-time.Hour)), run(2, forge.RunFailing, t0.Add(-30*time.Minute))))
	// Five days on, the window starts two days after the last listing: runs
	// created between them went unlisted.
	later := t0.Add(5 * 24 * time.Hour)
	if Covered(s.LastListed("gh", "o/r"), later.Add(-lookback)) {
		t.Fatal("Setup: the listing after the gap is covered by the one before it")
	}
	commit(t, s, "gh", scanAt(later, run(9, forge.RunFailing, later.Add(-time.Hour))))
	if w := workflow(t, s); w.Streak != 1 || !w.Clipped || !w.Since.Equal(later.Add(-time.Hour)) {
		t.Errorf("a failure after a coverage gap = %+v, want streak 1 since run 9, clipped: the failures in the gap are unknown", w)
	}
}

func TestCommit_a_listing_cut_short_judges_its_runs_and_clips_a_streak_across_its_cut(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunFailing, t0.Add(-5*time.Hour))))
	start := t0.Add(time.Hour)
	commit(t, s, "gh", cutAt(start, start.Add(-time.Hour), run(9, forge.RunFailing, start.Add(-time.Hour))))
	if w := workflow(t, s); w.RunID != 9 || w.Streak != 1 || !w.Clipped {
		t.Errorf("a failure past a cut over an older failing verdict = %+v, want run 9, streak 1, clipped: the runs below the cut are unknown", w)
	}
	commit(t, s, "gh", cutAt(start.Add(time.Hour), start.Add(-time.Hour), run(9, forge.RunFailing, start.Add(-time.Hour)), run(10, forge.RunFailing, start)))
	if w := workflow(t, s); w.RunID != 10 || w.Streak != 2 || !w.Clipped {
		t.Errorf("a failure after the verdict's run inside one cut listing = %+v, want run 10, streak 2, still clipped", w)
	}
}

func TestCommit_a_cut_listing_that_leaves_a_hole_keeps_the_listing_before_it(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunPassing, t0.Add(-time.Hour))))
	// 50h on, a cut listing reaches back only an hour: runs created between
	// t0 and then (a failing CI run among them) were listed by nobody.
	second := t0.Add(50 * time.Hour)
	other := run(3, forge.RunPassing, second.Add(-time.Hour))
	other.Workflow = "Docs"
	commit(t, s, "gh", cutAt(second, second.Add(-time.Hour), other))
	if got := s.LastListed("gh", "o/r"); !got.Equal(t0) {
		t.Errorf("LastListed after a cut leaving a hole = %s, want %s, the last listing it reached back to", got, t0)
	}
	third := second.Add(50 * time.Hour)
	if Covered(s.LastListed("gh", "o/r"), third.Add(-lookback)) {
		t.Errorf("Covered(LastListed, window from %s) = true, want false: the hole is past that window", third.Add(-lookback))
	}
	commit(t, s, "gh", scanAt(third, run(4, forge.RunFailing, third.Add(-time.Hour))))
	if w := workflow(t, s); w.RunID != 4 || w.Streak != 1 || !w.Clipped {
		t.Errorf("a failure after the hole = %+v, want run 4, streak 1, clipped: the runs in the hole are unknown", w)
	}
	if got := s.LastListed("gh", "o/r"); !got.Equal(third) {
		t.Errorf("LastListed after a whole listing past the hole = %s, want %s: the gap is reported once", got, third)
	}
}

func TestCommit_a_cold_cut_listing_records_its_listing_time_so_a_later_hole_is_reported(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", cutAt(t0, t0.Add(-2*time.Hour), run(1, forge.RunPassing, t0.Add(-time.Hour))))
	if got := s.LastListed("gh", "o/r"); !got.Equal(t0) {
		t.Errorf("LastListed after a cut listing on a cold store = %s, want %s: the runs it did not reach predate the store", got, t0)
	}
	second := t0.Add(15 * time.Minute)
	commit(t, s, "gh", cutAt(second, t0.Add(-90*time.Minute), run(2, forge.RunPassing, t0.Add(5*time.Minute))))
	// A failing CI run created at t0+10h is never listed: 50h on, a cut
	// listing reaches back only an hour.
	third := t0.Add(50 * time.Hour)
	other := run(4, forge.RunPassing, third.Add(-time.Hour))
	other.Workflow = "Docs"
	commit(t, s, "gh", cutAt(third, third.Add(-time.Hour), other))
	if got := s.LastListed("gh", "o/r"); !got.Equal(second) {
		t.Errorf("LastListed after a cut leaving a hole = %s, want %s, the last listing it reached back to", got, second)
	}
	fourth := third.Add(50 * time.Hour)
	if Covered(s.LastListed("gh", "o/r"), fourth.Add(-lookback)) {
		t.Errorf("Covered(LastListed, window from %s) = true, want false: the hole is past that window and must be reported", fourth.Add(-lookback))
	}
}

func TestCommit_a_cut_listing_advances_the_listing_time_when_it_reaches_back_or_reports_a_gap(t *testing.T) {
	tests := []struct {
		name          string
		start, oldest time.Time
	}{
		{"reaching_back_to_the_last_listing", t0.Add(time.Hour), t0.Add(-time.Minute)},
		{"after_a_gap_past_the_window", t0.Add(5 * 24 * time.Hour), t0.Add(5*24*time.Hour - time.Hour)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t, t.TempDir())
			commit(t, s, "gh", read(run(1, forge.RunPassing, t0.Add(-time.Hour))))
			commit(t, s, "gh", cutAt(tc.start, tc.oldest, run(2, forge.RunPassing, tc.oldest)))
			if got := s.LastListed("gh", "o/r"); !got.Equal(tc.start) {
				t.Errorf("LastListed after a cut listing at %s with its oldest row at %s = %s, want %s", tc.start, tc.oldest, got, tc.start)
			}
		})
	}
}

func TestCommit_a_cut_listing_above_a_run_the_listing_before_returned_pending_strands_it(t *testing.T) {
	s := open(t, t.TempDir())
	first := read(run(1, forge.RunPassing, t0.Add(-20*time.Hour)))
	pending := t0.Add(-10 * time.Hour)
	first.Listings[0].Pending = pending
	commit(t, s, "gh", first)
	s = reload(t, s)
	if got := s.Pending("gh", "o/r"); !got.Equal(pending) {
		t.Fatalf("Pending after a listing returned run 8 pending at %s = %s, want %s", pending, got, pending)
	}
	// 44h on, a cut listing reaches back past the scan before it but not to
	// the run it returned pending, which may have finished since unlisted.
	second, cover := t0.Add(44*time.Hour), t0.Add(-50*time.Minute)
	if !Covered(s.LastListed("gh", "o/r"), cover) {
		t.Fatal("Setup: the cut listing does not reach back to the scan before it")
	}
	if !Stranded(true, cover, s.Pending("gh", "o/r")) {
		t.Errorf("Stranded(cut from %s, pending %s) = false, want true", cover, pending)
	}
	if Stranded(true, pending.Add(-time.Minute), pending) || Stranded(false, cover, pending) {
		t.Error("Stranded = true for a cut reaching the pending run or a whole listing, want false")
	}
	commit(t, s, "gh", cutAt(second, cover, run(9, forge.RunFailing, cover)))
	if got := s.Pending("gh", "o/r"); !got.IsZero() {
		t.Errorf("Pending after a listing that returned no run pending = %s, want zero: the gap is reported once", got)
	}
}

func TestCommit_each_branch_keeps_its_own_verdict(t *testing.T) {
	feature := run(2, forge.RunFailing, t0.Add(-time.Hour))
	feature.Branch = "feature"
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunPassing, t0.Add(-2*time.Hour)), feature))
	if w := workflow(t, s); w.State != forge.RunPassing || w.RunID != 1 {
		t.Errorf("main after a feature-branch failure = %+v, want still passing at run 1", w)
	}
	if w := workflowOn(t, s, "o/r", "CI", "feature"); w.State != forge.RunFailing || w.RunID != 2 || w.Streak != 1 {
		t.Errorf("feature verdict = %+v, want failing at run 2, streak 1", w)
	}
}

func TestCommit_a_pull_request_run_is_no_run_of_its_branch(t *testing.T) {
	pr := run(2, forge.RunFailing, t0.Add(-time.Hour))
	pr.Trigger = forge.TriggerPullRequest
	s := open(t, t.TempDir())
	if got := commit(t, s, "gh", read(run(1, forge.RunPassing, t0.Add(-2*time.Hour)), pr)); len(got) != 2 {
		t.Fatalf("Commit(push and pull request runs) = %+v, want both delivered", got)
	}
	if w := workflow(t, s); w.State != forge.RunPassing || w.RunID != 1 {
		t.Errorf("main after a failing pull request run named main = %+v, want still passing at run 1", w)
	}
}

func TestCommit_an_older_run_does_not_move_the_verdict(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(5, forge.RunFailing, t0.Add(-time.Hour))))
	commit(t, s, "gh", scanAt(t0.Add(time.Hour), run(4, forge.RunPassing, t0.Add(-2*time.Hour)), run(5, forge.RunFailing, t0.Add(-time.Hour))))
	if w := workflow(t, s); w.RunID != 5 || w.State != forge.RunFailing {
		t.Errorf("after an older run arrived: verdict = %+v, want still run 5 failing", w)
	}
}

func TestCommit_a_verdict_outlives_its_run_until_the_retention(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunFailing, t0.Add(-time.Hour))))
	weekLater := t0.Add(7 * 24 * time.Hour)
	commit(t, s, "gh", scanAt(weekLater))
	s = reload(t, s)
	if n := s.Delivered("gh"); n != 0 {
		t.Errorf("Delivered a week on = %d, want 0: run 1 left the window", n)
	}
	if w := workflow(t, s); w.State != forge.RunFailing || w.RunID != 1 {
		t.Errorf("a weekly workflow's verdict a week after its run = %+v, want still failing at run 1", w)
	}
	commit(t, s, "gh", scanAt(t0.Add(WorkflowRetention)))
	if _, ok := verdictOf(s.Workflows("gh"), "o/r", "CI", "main"); ok {
		t.Error("a verdict whose run is older than the retention is still held, want it dropped")
	}
}

func TestCommit_a_delivered_run_is_kept_while_its_window_can_list_it(t *testing.T) {
	s := open(t, t.TempDir())
	r := run(1, forge.RunFailing, t0.Add(-time.Hour))
	commit(t, s, "gh", read(r))
	edge := r.CreatedAt.Add(lookback)
	if got := commit(t, s, "gh", scanAt(edge, r)); len(got) != 0 || s.Delivered("gh") != 1 {
		t.Errorf("Commit at the window's last instant on run 1 = %+v with %d delivered, want no change and run 1 kept", got, s.Delivered("gh"))
	}
	commit(t, s, "gh", scanAt(edge.Add(time.Second)))
	if n := s.Delivered("gh"); n != 0 {
		t.Errorf("Delivered once run 1 was created before the window = %d, want 0", n)
	}
}

func TestCommit_prunes_a_connection_no_longer_scanned(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "old", read(run(1, forge.RunFailing, t0.Add(-time.Hour))))
	commit(t, s, "gh", scanAt(t0.Add(WorkflowRetention+time.Hour)))
	if n := s.Delivered("old"); n != 0 {
		t.Errorf("Delivered(old) after another connection's save past the retention = %d, want 0", n)
	}
	for k := range s.Workflows("old") {
		t.Errorf("Workflows(old) still holds %v past the retention, want nothing", k)
	}
}

func TestCommit_a_whole_discovery_forgets_the_verdicts_of_repositories_it_did_not_keep(t *testing.T) {
	gone := inRepo(run(2, forge.RunFailing, t0.Add(-time.Hour)), "o/gone")
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunFailing, t0.Add(-time.Hour)), gone))
	r := read(run(1, forge.RunFailing, t0.Add(-time.Hour)))
	r.Kept = map[string]bool{"o/r": true}
	commit(t, s, "gh", r)
	if _, ok := verdictOf(s.Workflows("gh"), "o/gone", "CI", "main"); ok {
		t.Error("o/gone's verdict outlived a whole discovery without it, want it forgotten")
	}
	if !s.LastListed("gh", "o/gone").IsZero() {
		t.Error("o/gone's listing time outlived a whole discovery without it, want it forgotten")
	}
	if got := commit(t, s, "gh", scanAt(t0.Add(time.Hour), gone)); len(got) != 0 {
		t.Errorf("Commit when o/gone returns = %+v, want no change: its run was delivered", got)
	}
	if w := workflowOn(t, s, "o/gone", "CI", "main"); !w.Clipped {
		t.Errorf("o/gone's verdict after its return = %+v, want its streak clipped: nothing covered it while it was gone", w)
	}
}

func TestCommit_a_workflow_a_whole_listing_named_stays_defined_while_it_has_a_verdict(t *testing.T) {
	s := open(t, t.TempDir())
	r := read(run(1, forge.RunFailing, t0.Add(-time.Hour)))
	commit(t, s, "gh", r)
	if w := workflow(t, s); w.Defined {
		t.Fatalf("Setup: verdict = %+v, want not defined before any workflow listing", w)
	}
	r = scanAt(t0.Add(time.Hour), run(2, forge.RunFailing, t0))
	r.Defined = map[string][]string{"o/r": {"CI"}}
	commit(t, s, "gh", r)
	commit(t, s, "gh", scanAt(t0.Add(2*time.Hour), run(3, forge.RunPassing, t0.Add(time.Hour))))
	if w := workflow(t, reload(t, s)); !w.Defined || w.RunID != 3 {
		t.Errorf("verdict after a listing named it and a later run = %+v, want defined at run 3", w)
	}
}

func TestLastListed_is_the_start_of_the_last_scan_that_listed_the_repository(t *testing.T) {
	s := open(t, t.TempDir())
	if got := s.LastListed("gh", "o/r"); !got.IsZero() {
		t.Fatalf("LastListed on an empty store = %s, want zero", got)
	}
	commit(t, s, "gh", read(run(1, forge.RunFailing, t0)))
	commit(t, s, "gh", &Read{Start: t0.Add(time.Hour), Window: t0.Add(time.Hour - lookback)})
	if got := reload(t, s).LastListed("gh", "o/r"); !got.Equal(t0) {
		t.Errorf("LastListed after a scan that listed nothing = %s, want %s, the last scan that listed o/r", got, t0)
	}
}

func TestCovered_needs_a_listing_that_reaches_back_to_the_one_before(t *testing.T) {
	tests := []struct {
		name        string
		prev, cover time.Time
		want        bool
	}{
		{"none_before", time.Time{}, t0, false},
		{"overlapping", t0, t0.Add(-time.Hour), true},
		{"meeting", t0, t0, true},
		{"gap", t0, t0.Add(time.Second), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Covered(tc.prev, tc.cover); got != tc.want {
				t.Errorf("Covered(%s, %s) = %t, want %t", tc.prev, tc.cover, got, tc.want)
			}
		})
	}
}

func TestCommit_round_trips_verdicts_and_deliveries_through_the_file(t *testing.T) {
	s := open(t, t.TempDir())
	r := read(run(1, forge.RunPassing, t0.Add(-2*time.Hour)), run(2, forge.RunFailing, t0.Add(-time.Hour)))
	r.Defined = map[string][]string{"o/r": {"CI"}}
	commit(t, s, "gh", r)
	before := workflow(t, s)
	next := reload(t, s)
	if after := workflow(t, next); after != before || next.Delivered("gh") != 2 || !next.LastListed("gh", "o/r").Equal(t0) {
		t.Errorf("after a reload: verdict %+v, %d delivered, listed %s, want %+v, 2 and %s", after, next.Delivered("gh"), next.LastListed("gh", "o/r"), before, t0)
	}
}

// blockWrites makes every write of s fail: a rename onto a non-empty
// directory fails whoever runs the test.
func blockWrites(t *testing.T, s *Store) (unblock func()) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(s.Path(), "x"), 0o700); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	return func() {
		if err := os.RemoveAll(s.Path()); err != nil {
			t.Fatalf("Setup: %v", err)
		}
	}
}

func TestCommit_delivers_its_changes_before_the_write_that_stores_them(t *testing.T) {
	s := open(t, t.TempDir())
	var onDisk error
	err := s.Commit(t.Context(), "gh", read(run(1, forge.RunFailing, t0)), func([]Change) bool {
		_, onDisk = os.Stat(s.Path())
		return true
	})
	if err != nil || !errors.Is(onDisk, os.ErrNotExist) {
		t.Errorf("Commit = %v with the state file %v at delivery, want nil and no file yet: a crash after the write must not hide an undelivered change", err, onDisk)
	}
	if _, err := os.Stat(s.Path()); err != nil {
		t.Errorf("state file after Commit: %v, want it written", err)
	}
}

func TestCommit_records_nothing_of_a_read_whose_delivery_stopped_short(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunFailing, t0)))
	rerun := run(1, forge.RunPassing, t0)
	rerun.UpdatedAt = t0.Add(time.Hour)
	r := read(rerun, run(2, forge.RunPassing, t0))
	before, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	var handed int
	err = s.Commit(t.Context(), "gh", r, func(ch []Change) bool { handed = len(ch); return false })
	if !errors.Is(err, errUndelivered) || handed != 2 {
		t.Fatalf("Commit with a delivery that stopped short = %v after handing %d changes, want errUndelivered after 2", err, handed)
	}
	if after, err := os.ReadFile(s.Path()); err != nil || !bytes.Equal(after, before) {
		t.Errorf("state file after a delivery that stopped short = %v, changed %t, want it as it was", err, !bytes.Equal(after, before))
	}
	if n := s.Delivered("gh"); n != 1 {
		t.Errorf("Delivered(gh) after a delivery that stopped short = %d, want 1: no run it handed may read as delivered", n)
	}
	if got := commit(t, s, "gh", r); len(got) != 2 || got[0].Previous != forge.RunFailing {
		t.Errorf("Commit(same read) after a delivery that stopped short = %+v, want run 1's re-run from failing and run 2 handed again", got)
	}
	s = reload(t, s)
	if n := s.Delivered("gh"); n != 2 {
		t.Errorf("Delivered(gh) after the delivered read and a restart = %d, want 2", n)
	}
}

// stallWrites makes every state write hang, whatever its context, until the
// returned release is called, as a write on a stalled file system does.
func stallWrites(t *testing.T) (release func()) {
	t.Helper()
	stalled := make(chan struct{})
	writeState = func(ctx context.Context, path string, data []byte, opts ...atomicfile.Option) (atomicfile.Result, error) {
		<-stalled
		return atomicfile.WriteFile(context.WithoutCancel(ctx), path, data, opts...)
	}
	var once sync.Once
	release = func() { once.Do(func() { close(stalled) }) }
	t.Cleanup(func() {
		release()
		writeState = atomicfile.WriteFile
	})
	return release
}

// commitWithin commits r on conn under a context ending after limit and
// fails the test when Commit has not returned within a second of it.
func commitWithin(t *testing.T, s *Store, conn string, r *Read, limit time.Duration) ([]Change, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), limit)
	defer cancel()
	type result struct {
		err error
		got []Change
	}
	done := make(chan result, 1)
	go func() {
		var got []Change
		err := s.Commit(ctx, conn, r, func(ch []Change) bool { got = append(got, ch...); return true })
		done <- result{got: got, err: err}
	}()
	select {
	case res := <-done:
		return res.got, res.err
	case <-time.After(limit + time.Second):
		t.Fatalf("Commit(%s) under a %s context had not returned a second after it ended", conn, limit)
		return nil, nil
	}
}

func TestCommit_returns_by_its_context_while_a_write_stalls_and_keeps_its_changes(t *testing.T) {
	s := open(t, t.TempDir())
	release := stallWrites(t)
	got, err := commitWithin(t, s, "gh", read(run(1, forge.RunFailing, t0)), 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || len(got) != 1 || got[0].Run.ID != 1 {
		t.Errorf("Commit(gh) with its write stalled = %+v, %v, want run 1 delivered and context.DeadlineExceeded", got, err)
	}
	gl := read(run(9, forge.RunPassing, t0))
	got, err = commitWithin(t, s, "gl", gl, 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || len(got) != 0 || s.Delivered("gl") != 0 {
		t.Errorf("Commit(gl) behind the stalled write = %+v, %v with %d delivered, want nothing delivered or recorded and context.DeadlineExceeded", got, err, s.Delivered("gl"))
	}
	release()
	if got := commit(t, s, "gl", gl); len(got) != 1 || got[0].Run.ID != 9 {
		t.Errorf("Commit(gl) of the same read after the stall ended = %+v, want run 9 delivered", got)
	}
	if got := commit(t, s, "gh", read()); len(got) != 0 {
		t.Errorf("Commit after the stall ended = %+v, want no change: both runs were delivered", got)
	}
	next := reload(t, s)
	if gh, gl := next.Delivered("gh"), next.Delivered("gl"); gh != 1 || gl != 1 {
		t.Errorf("reloaded Delivered(gh), Delivered(gl) = %d, %d, want 1, 1: the write after the stall stored both", gh, gl)
	}
}

func TestCommit_a_blocked_delivery_does_not_hold_another_Commit_past_its_context(t *testing.T) {
	s := open(t, t.TempDir())
	delivering, unblock := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- s.Commit(t.Context(), "gh", read(run(1, forge.RunFailing, t0)), func([]Change) bool {
			close(delivering)
			<-unblock
			return true
		})
	}()
	<-delivering
	defer func() {
		close(unblock)
		if err := <-first; err != nil {
			t.Errorf("Commit(gh) once its delivery unblocked = %v, want nil", err)
		}
	}()
	start := time.Now()
	got, err := commitWithin(t, s, "gl", read(run(9, forge.RunPassing, t0)), 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || len(got) != 0 {
		t.Errorf("Commit(gl) while gh's delivery blocks = %+v, %v, want nothing delivered and context.DeadlineExceeded", got, err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("Commit(gl) under a 20ms context returned after %s, want it back by its context while gh's delivery blocks", took)
	}
	held := make(chan int, 1)
	go func() { held <- s.Delivered("gh") }()
	select {
	case n := <-held:
		if n != 0 {
			t.Errorf("Delivered(gh) while its delivery blocks = %d, want 0: a change is recorded only once delivered", n)
		}
	case <-time.After(time.Second):
		t.Errorf("Delivered(gh) had not returned a second into gh's blocked delivery, want reads not to wait for a delivery")
	}
}

// openWhenFree opens dir once its holder has released it, failing the test
// when that takes over two seconds.
func openWhenFree(t *testing.T, dir string) *Store {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := Open(dir)
		if err == nil {
			t.Cleanup(s.Close)
			return s
		}
		if !errors.Is(err, ErrInUse) || time.Now().After(deadline) {
			t.Fatalf("Open(%s) = %v, want the directory released once the stalled write ended", dir, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClose_holds_the_directory_until_a_stalled_write_ends(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	release := stallWrites(t)
	if _, err := commitWithin(t, s, "old", read(run(1, forge.RunFailing, t0)), 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Setup: Commit(old) with its write stalled = %v, want context.DeadlineExceeded", err)
	}
	s.Close()
	if other, err := Open(dir); !errors.Is(err, ErrInUse) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("Open after Close with a write still running = %v, want ErrInUse: that write could still replace a successor's state", err)
	}
	release()
	next := openWhenFree(t, dir)
	if d, err := next.Load(t.Context()); d != nil || err != nil {
		t.Fatalf("Load after the stalled write ended = %+v, %v, want its file accepted", d, err)
	}
	commit(t, next, "new", read(run(9, forge.RunPassing, t0)))
	last := reload(t, next)
	if old, cur := last.Delivered("old"), last.Delivered("new"); old != 1 || cur != 1 {
		t.Errorf("reloaded Delivered(old), Delivered(new) = %d, %d, want 1, 1: the stalled write landed before the successor's, never after", old, cur)
	}
}

func TestClose_a_Commit_queued_behind_a_stalled_write_writes_nothing(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	release := stallWrites(t)
	if _, err := commitWithin(t, s, "old", read(run(1, forge.RunFailing, t0)), 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Setup: Commit(old) with its write stalled = %v, want context.DeadlineExceeded", err)
	}
	var delivered []Change
	late := make(chan error, 1)
	go func() {
		late <- s.Commit(t.Context(), "late", read(run(7, forge.RunPassing, t0)), func(ch []Change) bool { delivered = ch; return true })
	}()
	s.Close()
	release()
	select {
	case err := <-late:
		if !errors.Is(err, errClosed) || len(delivered) != 0 {
			t.Errorf("Commit(late) queued behind the stalled write when Close ran = %v with %+v delivered, want errClosed and nothing delivered", err, delivered)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Commit(late) queued behind the stalled write had not returned 2s after the write ended")
	}
	next := openWhenFree(t, dir)
	if d, err := next.Load(t.Context()); d != nil || err != nil {
		t.Fatalf("Load after Close = %+v, %v, want the file accepted", d, err)
	}
	if old, l := next.Delivered("old"), next.Delivered("late"); old != 1 || l != 0 {
		t.Errorf("reloaded Delivered(old), Delivered(late) = %d, %d, want 1, 0: no write may land after Close released the directory", old, l)
	}
}

func TestCommit_after_Close_fails_and_writes_nothing(t *testing.T) {
	s := open(t, t.TempDir())
	s.Close()
	got, err := commitErr(t, s, "gh", read(run(1, forge.RunFailing, t0)))
	if !errors.Is(err, errClosed) || len(got) != 0 {
		t.Errorf("Commit after Close = %+v, %v, want nothing delivered and errClosed", got, err)
	}
	if _, err := os.Stat(s.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file after a Commit on a closed Store: %v, want none written", err)
	}
}

func TestCommit_a_failed_write_delivers_its_changes_once_and_a_later_write_stores_them(t *testing.T) {
	s := open(t, t.TempDir())
	unblock := blockWrites(t, s)
	got, err := commitErr(t, s, "gh", read(run(1, forge.RunFailing, t0)))
	if err == nil || len(got) != 1 || got[0].Run.ID != 1 || !got[0].New {
		t.Fatalf("Commit(write blocked) = %+v, %v, want the error and run 1 delivered", got, err)
	}
	unblock()
	if got := commit(t, s, "gl", read(run(9, forge.RunPassing, t0))); len(got) != 1 || got[0].Run.ID != 9 {
		t.Errorf("next Commit (another connection) = %+v, want only gl run 9", got)
	}
	if got := commit(t, s, "gh", read(run(1, forge.RunFailing, t0))); len(got) != 0 {
		t.Errorf("third Commit = %+v, want no change: run 1 was delivered", got)
	}
	if n := reload(t, s).Delivered("gh"); n != 1 {
		t.Errorf("reloaded Delivered(gh) = %d, want 1: the later write stored run 1", n)
	}
}

func TestCommit_a_change_whose_write_never_landed_is_delivered_again_after_a_restart(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunFailing, t0)))
	writeState = func(context.Context, string, []byte, ...atomicfile.Option) (atomicfile.Result, error) {
		return atomicfile.Result{}, errors.New("disk full")
	}
	t.Cleanup(func() { writeState = atomicfile.WriteFile })
	if got, err := commitErr(t, s, "gh", read(run(2, forge.RunFailing, t0))); err == nil || len(got) != 1 || got[0].Run.ID != 2 {
		t.Fatalf("Setup: Commit(write failing) = %+v, %v, want the error and run 2 delivered", got, err)
	}
	writeState = atomicfile.WriteFile
	next := reload(t, s)
	if n := next.Delivered("gh"); n != 1 {
		t.Fatalf("Delivered after the restart = %d, want the file's 1 run", n)
	}
	if got := commit(t, next, "gh", read(run(1, forge.RunFailing, t0), run(2, forge.RunFailing, t0))); len(got) != 1 || got[0].Run.ID != 2 {
		t.Errorf("Commit after the restart = %+v, want run 2 delivered again and run 1 not", got)
	}
}

func TestCommit_a_write_that_is_not_durable_delivers_its_changes_and_a_restart_does_not_replay_them(t *testing.T) {
	s := open(t, t.TempDir())
	writeState = func(ctx context.Context, path string, data []byte, opts ...atomicfile.Option) (atomicfile.Result, error) {
		res, err := atomicfile.WriteFile(ctx, path, data, opts...)
		res.Durable = false
		return res, err
	}
	t.Cleanup(func() { writeState = atomicfile.WriteFile })
	got, err := commitErr(t, s, "gh", read(run(1, forge.RunFailing, t0)))
	if !errors.Is(err, ErrNotDurable) || len(got) != 1 || got[0].Run.ID != 1 || !got[0].New {
		t.Fatalf("Commit(write not durable) = %+v, %v, want ErrNotDurable with run 1", got, err)
	}
	writeState = atomicfile.WriteFile
	if got := commit(t, s, "gh", read()); len(got) != 0 {
		t.Errorf("next durable Commit = %+v, want no change: run 1 was delivered", got)
	}
	next := reload(t, s)
	if got := commit(t, next, "gh", read(run(1, forge.RunFailing, t0))); len(got) != 0 || next.Delivered("gh") != 1 {
		t.Errorf("after restart, re-observing run 1 = %+v with %d delivered, want no change and 1 delivered: the file already held it", got, next.Delivered("gh"))
	}
}

func TestCommit_a_write_that_never_becomes_durable_delivers_each_change_once(t *testing.T) {
	s := open(t, t.TempDir())
	writeState = func(ctx context.Context, path string, data []byte, opts ...atomicfile.Option) (atomicfile.Result, error) {
		res, err := atomicfile.WriteFile(ctx, path, data, opts...)
		res.Durable = false
		return res, err
	}
	t.Cleanup(func() { writeState = atomicfile.WriteFile })
	for i, id := range []int64{1, 2} {
		got, err := commitErr(t, s, "gh", read(run(1, forge.RunFailing, t0), run(id, forge.RunFailing, t0)))
		if !errors.Is(err, ErrNotDurable) || len(got) != 1 || got[0].Run.ID != id {
			t.Errorf("Commit %d (no write durable) = %+v, %v, want ErrNotDurable with only run %d", i+1, got, err, id)
		}
	}
}

func TestLoad_reads_the_file_under_the_store_lock(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunFailing, t0)))
	unlocked := false
	readState = func(ctx context.Context, root *os.Root, name string, maxBytes int64) ([]byte, error) {
		if s.mu.TryLock() {
			unlocked = true
			s.mu.Unlock()
		}
		return atomicfile.ReadBoundedInRoot(ctx, root, name, maxBytes)
	}
	t.Cleanup(func() { readState = atomicfile.ReadBoundedInRoot })
	if d, err := s.Load(t.Context()); d != nil || err != nil {
		t.Fatalf("Load = %+v, %v, want the file accepted", d, err)
	}
	if unlocked {
		t.Error("Load read the file without holding the store's lock, so a Commit between the read and the assignment is lost")
	}
}

func TestLoad_waits_for_a_write_a_returned_Commit_left_running(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunPassing, t0)))
	release := stallWrites(t)
	if got, err := commitWithin(t, s, "gh", read(run(2, forge.RunPassing, t0)), 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) || len(got) != 1 {
		t.Fatalf("Setup: Commit(gh) of run 2 with its write stalled = %+v, %v, want run 2 delivered and context.DeadlineExceeded", got, err)
	}
	loaded := make(chan error, 1)
	go func() {
		_, err := s.Load(t.Context())
		loaded <- err
	}()
	early := false
	var loadErr error
	select {
	case loadErr = <-loaded:
		early = true
		t.Errorf("Load while run 2's write stalls returned %v, want it to wait for that write", loadErr)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if !early {
		loadErr = <-loaded
	}
	if loadErr != nil {
		t.Fatalf("Load once run 2's write landed = %v, want nil", loadErr)
	}
	got := commit(t, s, "gh", read(run(2, forge.RunPassing, t0), run(3, forge.RunPassing, t0)))
	if len(got) != 1 || got[0].Run.ID != 3 {
		t.Errorf("Commit(gh) of runs 2 and 3 after the Load = %+v, want only run 3: run 2 was delivered before the Load", got)
	}
}

func TestLoad_whose_context_ends_behind_a_running_write_keeps_the_state(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunPassing, t0)))
	release := stallWrites(t)
	if got, err := commitWithin(t, s, "gh", read(run(2, forge.RunPassing, t0)), 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) || len(got) != 1 {
		t.Fatalf("Setup: Commit(gh) of run 2 with its write stalled = %+v, %v, want run 2 delivered and context.DeadlineExceeded", got, err)
	}
	loaded := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		_, err := s.Load(ctx)
		loaded <- err
	}()
	var loadErr error
	returned := true
	select {
	case loadErr = <-loaded:
	case <-time.After(2 * time.Second):
		returned = false
		t.Error("Load whose 50ms context ended behind run 2's stalled write had not returned after 2s, want it to return the context's error")
	}
	release()
	if !returned {
		loadErr = <-loaded
	}
	if !errors.Is(loadErr, context.DeadlineExceeded) {
		t.Errorf("Load whose context ended behind a running write = %v, want context.DeadlineExceeded", loadErr)
	}
	got := commit(t, s, "gh", read(run(2, forge.RunPassing, t0), run(3, forge.RunPassing, t0)))
	if len(got) != 1 || got[0].Run.ID != 3 {
		t.Errorf("Commit(gh) of runs 2 and 3 after the abandoned Load = %+v, want only run 3: the Load must leave run 2's delivery in the state", got)
	}
}

func TestCommit_a_removed_file_is_written_again_from_memory(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", read(run(1, forge.RunFailing, t0)))
	if err := os.Remove(s.Path()); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	got := commit(t, s, "gh", read(run(1, forge.RunFailing, t0), run(2, forge.RunPassing, t0)))
	if len(got) != 1 || got[0].Run.ID != 2 {
		t.Errorf("Commit after the file was removed = %+v, want only run 2 new", got)
	}
	if n := reload(t, s).Delivered("gh"); n != 2 {
		t.Errorf("reloaded Delivered = %d, want 2", n)
	}
}

func TestCommit_honors_a_cancelled_context_and_keeps_the_state(t *testing.T) {
	s := open(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var got []Change
	if err := s.Commit(ctx, "gh", read(run(1, forge.RunFailing, t0)), func(ch []Change) bool { got = ch; return true }); !errors.Is(err, context.Canceled) || len(got) != 1 {
		t.Errorf("Commit(cancelled) = %v delivering %+v, want the context's error and run 1", err, got)
	}
	commit(t, s, "gh", read())
	if n := reload(t, s).Delivered("gh"); n != 1 {
		t.Errorf("reloaded Delivered after the next Commit = %d, want 1: the state kept run 1", n)
	}
}

func TestCommit_under_a_cancelled_context_starts_no_write(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	stallWrites(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Commit(ctx, "gh", read(run(1, forge.RunFailing, t0)), func([]Change) bool { return true }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Setup: Commit(cancelled) = %v, want context.Canceled", err)
	}
	s.Close()
	next, err := Open(dir)
	if err != nil {
		t.Fatalf("Open right after a cancelled Commit and Close = %v, want the directory free: no write may start under a done context", err)
	}
	next.Close()
}

func TestCommit_carries_the_observation_to_its_change(t *testing.T) {
	s := open(t, t.TempDir())
	got := commit(t, s, "gh", withDefault(read(run(1, forge.RunFailing, t0))))
	if len(got) != 1 || !got[0].OnDefault || got[0].Run.URL != "https://forge.test/o/r/runs/1" {
		t.Errorf("Commit changes = %+v, want run 1 with OnDefault and its URL", got)
	}
}

func TestOpen_refuses_a_directory_another_store_holds(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if other, err := Open(dir); !errors.Is(err, ErrInUse) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("Open(held directory) = %v, want ErrInUse", err)
	}
	s.Close()
	open(t, dir)
}

func TestOpen_a_missing_directory_fails(t *testing.T) {
	if s, err := Open(filepath.Join(t.TempDir(), "absent")); err == nil {
		s.Close()
		t.Error("Open(missing dir) = nil error, want the failure reported")
	}
}

func TestLoad_missing_file_is_a_silent_cold_start(t *testing.T) {
	s := open(t, t.TempDir())
	if d, err := s.Load(t.Context()); d != nil || err != nil || s.Delivered("gh") != 0 {
		t.Errorf("Load(no file) = %+v, %v, %d delivered, want an empty state and no report", d, err, s.Delivered("gh"))
	}
}

// goodDelivered, goodVerdict, goodCold, goodAfterPass and goodPassing are
// entries apply could have written.
const (
	goodDelivered = `{"created":"2026-10-01T12:00:00Z","updated":"2026-10-01T12:05:00Z","state":"failing"}`
	goodVerdict   = `{"created":"2026-10-01T12:00:00Z","state":"failing","url":"u","run_id":1,` +
		`"streak":{"since":"2026-09-30T12:00:00Z","count":2},"base":{"since":"2026-09-30T12:00:00Z","count":1}}`
	goodCold      = `{"created":"2026-10-01T12:00:00Z","state":"failing","url":"u","run_id":1,"streak":{"since":"2026-10-01T12:00:00Z","count":1,"clipped":true}}`
	goodAfterPass = `{"created":"2026-10-01T12:00:00Z","state":"failing","url":"u","run_id":1,"streak":{"since":"2026-10-01T12:00:00Z","count":1},"base":{}}`
	goodPassing   = `{"created":"2026-10-01T12:00:00Z","state":"passing","url":"u","run_id":1,"streak":{}}`
)

// stateWith is a current-version file whose gh connection holds the
// delivered entries of o/r, the CI verdict on main and o/r's listing time,
// each empty for none.
func stateWith(delivered, verdict, listed string) string {
	v := ""
	if verdict != "" {
		v = `"o/r":{"CI":{"main":` + verdict + `}}`
	}
	l := ""
	if listed != "" {
		l = `,"listed":{"o/r":` + listed + `}`
	}
	return `{"version":2,"connections":{"gh":{"delivered":{"o/r":{` + delivered + `}},"verdicts":{` + v + `}` + l + `}}}`
}

func TestLoad_accepts_entries_the_store_writes(t *testing.T) {
	for _, verdict := range []string{goodVerdict, goodCold, goodAfterPass, goodPassing} {
		dir := t.TempDir()
		body := stateWith(`"1":`+goodDelivered, verdict, `"2026-10-01T12:00:00Z"`)
		if err := os.WriteFile(filepath.Join(dir, fileName), []byte(body), 0o600); err != nil {
			t.Fatalf("Setup: %v", err)
		}
		s := open(t, dir)
		d, err := s.Load(t.Context())
		if d != nil || err != nil || s.Delivered("gh") != 1 || !s.LastListed("gh", "o/r").Equal(t0) {
			t.Errorf("Load(valid entries, verdict %s) = %+v, %v with %d delivered, want 1 delivered, o/r listed at %s and no report", verdict, d, err, s.Delivered("gh"), t0)
		}
	}
}

func TestLoad_sets_aside_a_file_it_cannot_use(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"corrupt", "{not json"},
		{"earlier_version", `{"version":1,"connections":{}}`},
		{"wrong_version", `{"version":99,"connections":{}}`},
		{"null", "null"},
		{"verdict_not_keyed_by_branch", `{"version":2,"connections":{"gh":{"delivered":{},"verdicts":{"o/r":{"CI":` + goodVerdict + `}}}}}`},
		{"run_key_not_an_id", stateWith(`"x1":`+goodDelivered, "", "")},
		{"run_key_not_canonical", stateWith(`"01":`+goodDelivered, "", "")},
		{"run_id_not_positive", stateWith(`"0":`+goodDelivered, "", "")},
		{"run_state_unknown", stateWith(`"1":`+strings.Replace(goodDelivered, `"failing"`, `"broken"`, 1), "", "")},
		{"run_without_creation_time", stateWith(`"1":`+strings.Replace(goodDelivered, `"created":"2026-10-01T12:00:00Z",`, "", 1), "", "")},
		{"run_updated_before_created", stateWith(`"1":`+strings.Replace(goodDelivered, "12:05:00Z", "11:00:00Z", 1), "", "")},
		{"verdict_state_unknown", stateWith("", strings.Replace(goodPassing, `"passing"`, `"broken"`, 1), "")},
		{"verdict_run_id_not_positive", stateWith("", strings.Replace(goodVerdict, `"run_id":1`, `"run_id":0`, 1), "")},
		{"verdict_without_creation_time", stateWith("", strings.Replace(goodPassing, `"created":"2026-10-01T12:00:00Z",`, "", 1), "")},
		{"failing_verdict_without_streak", stateWith("", strings.Replace(goodVerdict, `"count":2`, `"count":0`, 1), "")},
		{"failing_verdict_without_since", stateWith("", strings.Replace(goodVerdict, `"streak":{"since":"2026-09-30T12:00:00Z",`, `"streak":{`, 1), "")},
		{"failing_verdict_since_after_its_run", stateWith("", strings.Replace(goodVerdict, `"streak":{"since":"2026-09-30T12:00:00Z"`, `"streak":{"since":"2026-10-02T12:00:00Z"`, 1), "")},
		{"passing_verdict_with_streak", stateWith("", strings.Replace(goodPassing, `"streak":{}`, `"streak":{"count":2}`, 1), "")},
		{"passing_verdict_with_since", stateWith("", strings.Replace(goodPassing, `"streak":{}`, `"streak":{"since":"2026-09-30T12:00:00Z"}`, 1), "")},
		{"passing_verdict_clipped", stateWith("", strings.Replace(goodPassing, `"streak":{}`, `"streak":{"clipped":true}`, 1), "")},
		{"base_streak_of_no_failure", stateWith("", strings.Replace(goodVerdict, `"base":{"since":"2026-09-30T12:00:00Z","count":1}`, `"base":{"since":"2026-09-30T12:00:00Z"}`, 1), "")},
		{"base_streak_without_since", stateWith("", strings.Replace(goodVerdict, `"base":{"since":"2026-09-30T12:00:00Z","count":1}`, `"base":{"count":1}`, 1), "")},
		{"base_streak_since_after_its_run", stateWith("", strings.Replace(goodVerdict, `"base":{"since":"2026-09-30T12:00:00Z"`, `"base":{"since":"2026-10-02T12:00:00Z"`, 1), "")},
		{"streak_count_not_following_its_base", stateWith("", strings.Replace(goodVerdict, `"count":2`, `"count":3`, 1), "")},
		{"streak_since_not_its_bases", stateWith("", strings.Replace(goodVerdict, `"streak":{"since":"2026-09-30T12:00:00Z"`, `"streak":{"since":"2026-09-30T13:00:00Z"`, 1), "")},
		{"failing_verdict_without_base_not_clipped", stateWith("", strings.Replace(goodVerdict, `,"base":{"since":"2026-09-30T12:00:00Z","count":1}`, "", 1), "")},
		{"passing_verdict_with_malformed_base", stateWith("", strings.Replace(goodPassing, `"streak":{}`, `"streak":{},"base":{"count":1}`, 1), "")},
		{"listed_at_no_time", stateWith("", "", `"0001-01-01T00:00:00Z"`)},
		{"pending_at_no_time", `{"version":2,"connections":{"gh":{"delivered":{},"verdicts":{},"pending":{"o/r":"0001-01-01T00:00:00Z"}}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, fileName), []byte(tc.body), 0o600); err != nil {
				t.Fatalf("Setup: %v", err)
			}
			s := open(t, dir)
			d, err := s.Load(t.Context())
			if err != nil || d == nil || d.Reason == nil {
				t.Fatalf("Load(%s) = %+v, %v, want the file set aside with its reason", tc.name, d, err)
			}
			if body, rerr := os.ReadFile(d.Aside); rerr != nil || string(body) != tc.body || !strings.HasPrefix(d.Aside, s.Path()+".corrupt-") {
				t.Errorf("Load(%s) set aside %q (%v), want the original bytes at %s.corrupt-<unix>", tc.name, d.Aside, rerr, s.Path())
			}
			if _, serr := os.Stat(s.Path()); !errors.Is(serr, os.ErrNotExist) {
				t.Errorf("Load(%s) left %s in place (%v), want it moved", tc.name, s.Path(), serr)
			}
			if s.Delivered("gh") != 0 {
				t.Errorf("Load(%s) state holds %d delivered runs, want an empty state", tc.name, s.Delivered("gh"))
			}
		})
	}
}

func TestLoad_sets_aside_a_fifo_without_blocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	s := open(t, dir)
	type result struct {
		d   *Discarded
		err error
	}
	done := make(chan result, 1)
	go func() {
		d, err := s.Load(t.Context())
		done <- result{d, err}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		// A writer's open releases a reader parked in open(2), so the
		// goroutine ends before the test does.
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
		<-done
		t.Fatal("Load(FIFO) blocked, want the file set aside")
	}
	if got.err != nil || got.d == nil || !errors.Is(got.d.Reason, atomicfile.ErrNotRegular) || got.d.Aside == "" {
		t.Fatalf("Load(FIFO) = %+v, %v, want it set aside as not a regular file", got.d, got.err)
	}
	if fi, err := os.Lstat(got.d.Aside); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("Load(FIFO) set aside %q (%v), want the FIFO moved there", got.d.Aside, err)
	}
	if ch := commit(t, s, "gh", read(run(1, forge.RunFailing, t0))); len(ch) != 1 {
		t.Errorf("Commit after the FIFO cold start = %+v, want run 1 reported once", ch)
	}
}

func TestLoad_an_over_cap_file_is_set_aside_and_the_next_commit_succeeds(t *testing.T) {
	orig := maxStateBytes
	maxStateBytes = 1024
	t.Cleanup(func() { maxStateBytes = orig })
	dir := t.TempDir()
	body := `{"version":2,"connections":{"gh":{"delivered":{},"verdicts":{}}},"pad":"` + strings.Repeat("a", 1100) + `"}`
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(body), 0o600); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	s := open(t, dir)
	d, err := s.Load(t.Context())
	if err != nil || d == nil || !errors.Is(d.Reason, atomicfile.ErrFileTooLarge) || d.Aside == "" {
		t.Fatalf("Load(over cap) = %+v, %v, want the file set aside as too large", d, err)
	}
	if got := commit(t, s, "gh", read(run(1, forge.RunFailing, t0))); len(got) != 1 {
		t.Errorf("Commit after the cold start = %+v, want run 1 reported once", got)
	}
	if n := reload(t, s).Delivered("gh"); n != 1 {
		t.Errorf("reloaded Delivered = %d, want 1", n)
	}
}

func TestCleanupTemps_reclaims_an_abandoned_write(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	temp := filepath.Join(dir, atomicfile.TempName())
	if err := os.WriteFile(temp, []byte("x"), 0o600); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(temp, old, old); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	failed, err := s.CleanupTemps(t.Context())
	if err != nil || failed != 0 {
		t.Fatalf("CleanupTemps = %d failed, %v", failed, err)
	}
	if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("abandoned temp %s still present (%v)", temp, err)
	}
}
