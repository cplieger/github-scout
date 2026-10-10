package runstate

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/forge"
)

// scanBack is a scan started at start that listed o/r, and each run's
// repository, whole from start minus back.
func scanBack(start time.Time, back time.Duration, runs ...*forge.Run) *Read {
	r := scanAt(start, runs...)
	r.Window = start.Add(-back)
	if !slices.ContainsFunc(r.Listings, func(l Listing) bool { return l.Repo == "o/r" }) {
		r.Listings = append(r.Listings, Listing{Repo: "o/r"})
	}
	for i := range r.Listings {
		r.Listings[i].Cover = r.Window
	}
	return r
}

func dayOf(repo, day string, passing, failing, neutral int, clipped bool) DayCount {
	return DayCount{Repo: repo, Day: day, Passing: passing, Failing: failing, Neutral: neutral, Clipped: clipped}
}

// untimed strips the run times Days reports, for a test about the counts.
func untimed(days []DayCount) []DayCount {
	out := slices.Clone(days)
	for i := range out {
		out[i].Timed, out[i].Seconds = 0, 0
	}
	return out
}

func TestDays_count_a_rerun_once_in_its_final_state(t *testing.T) {
	s := open(t, t.TempDir())
	failed := run(1, forge.RunFailing, t0.Add(-30*time.Minute))
	commit(t, s, "gh", scanBack(t0, time.Hour, failed))
	rerun := *failed
	rerun.State, rerun.UpdatedAt = forge.RunPassing, t0.Add(10*time.Minute)
	commit(t, s, "gh", scanBack(t0.Add(15*time.Minute), time.Hour, &rerun))
	// The first listing started at 11:00, so the day holds only what was
	// created since.
	want := []DayCount{dayOf("o/r", "2026-10-01", 1, 0, 0, true)}
	if got := untimed(s.Days("gh")); !slices.Equal(got, want) {
		t.Errorf("Days after run 1 failed then passed on a re-run = %+v, want %+v", got, want)
	}
}

func TestDays_a_lookback_under_a_day_still_counts_whole_days(t *testing.T) {
	s := open(t, t.TempDir())
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	created := []*forge.Run{
		run(1, forge.RunPassing, day.Add(time.Hour)), run(2, forge.RunFailing, day.Add(6*time.Hour)),
		run(3, forge.RunNeutral, day.Add(13*time.Hour)), run(4, forge.RunPassing, day.Add(23*time.Hour+50*time.Minute)),
	}
	for start := day.Add(-30 * time.Minute); start.Before(day.Add(25 * time.Hour)); start = start.Add(30 * time.Minute) {
		var listed []*forge.Run
		for _, r := range created {
			if !r.CreatedAt.Before(start.Add(-time.Hour)) && !r.CreatedAt.After(start) {
				listed = append(listed, r)
			}
		}
		commit(t, s, "gh", scanBack(start, time.Hour, listed...))
	}
	var got []DayCount
	for _, d := range untimed(s.Days("gh")) {
		if d.Day == "2026-10-01" {
			got = append(got, d)
		}
	}
	want := []DayCount{dayOf("o/r", "2026-10-01", 2, 1, 1, false)}
	if !slices.Equal(got, want) {
		t.Errorf("Days of 2026-10-01 from 1h listings every 30 minutes = %+v, want every run of the day once, not clipped: %+v", got, want)
	}
}

func TestDays_a_cold_start_fills_only_the_days_its_listing_covers(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", scanBack(t0, 30*time.Hour,
		run(1, forge.RunFailing, t0.Add(-29*time.Hour)), run(2, forge.RunPassing, t0.Add(-4*time.Hour))))
	// The listing started at 06:00 on 09-30, so that day is a lower bound.
	want := []DayCount{dayOf("o/r", "2026-09-30", 0, 1, 0, true), dayOf("o/r", "2026-10-01", 1, 0, 0, false)}
	if got := untimed(s.Days("gh")); !slices.Equal(got, want) {
		t.Errorf("Days after a cold 30h listing at %s = %+v, want %+v", t0, got, want)
	}
}

func TestDays_a_listing_from_midnight_clips_nothing(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", scanBack(t0, 12*time.Hour, run(1, forge.RunPassing, t0.Add(-time.Hour))))
	want := []DayCount{dayOf("o/r", "2026-10-01", 1, 0, 0, false)}
	if got := untimed(s.Days("gh")); !slices.Equal(got, want) {
		t.Errorf("Days after a cold listing from midnight = %+v, want %+v", got, want)
	}
}

func TestDays_a_coverage_gap_clips_every_day_it_reaches(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", scanBack(t0, 12*time.Hour, run(1, forge.RunPassing, t0.Add(-time.Hour))))
	// Stopped for two days: the next listing starts at 10-03 10:00, after the
	// last one, at 10-01 12:00.
	later := t0.Add(48 * time.Hour)
	commit(t, s, "gh", scanBack(later, 2*time.Hour, run(2, forge.RunFailing, later.Add(-time.Hour))))
	want := []DayCount{
		dayOf("o/r", "2026-10-01", 1, 0, 0, true), dayOf("o/r", "2026-10-02", 0, 0, 0, true),
		dayOf("o/r", "2026-10-03", 0, 1, 0, true),
	}
	if got := untimed(s.Days("gh")); !slices.Equal(got, want) {
		t.Errorf("Days across a gap from %s to %s = %+v, want %+v", t0, later.Add(-2*time.Hour), got, want)
	}
}

func TestDays_a_cut_listing_above_a_pending_run_clips_its_day(t *testing.T) {
	s := open(t, t.TempDir())
	first := read(run(1, forge.RunPassing, t0.Add(-20*time.Hour)))
	first.Listings[0].Pending = t0.Add(-26 * time.Hour)
	commit(t, s, "gh", first)
	cover := t0.Add(-50 * time.Minute)
	commit(t, s, "gh", cutAt(t0.Add(44*time.Hour), cover, run(9, forge.RunFailing, cover)))
	for _, d := range s.Days("gh") {
		if d.Day == "2026-09-30" && !d.Clipped {
			t.Errorf("Days of 09-30, holding a run pending below a cut = %+v, want clipped", d)
		}
	}
}

func TestDays_keeps_a_week_and_a_day_whatever_the_lookback(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", scanBack(t0, 12*time.Hour, run(1, forge.RunPassing, t0.Add(-time.Hour))))
	commit(t, s, "gh", scanBack(t0.Add(7*24*time.Hour), time.Hour))
	if got := s.Days("gh"); len(got) == 0 || got[0].Day != "2026-10-01" || got[0].Passing != 1 {
		t.Errorf("Days seven days on under a 1h lookback = %+v, want 2026-10-01 still counting run 1", got)
	}
	commit(t, s, "gh", scanBack(t0.Add(8*24*time.Hour), time.Hour))
	for _, d := range s.Days("gh") {
		if d.Day == "2026-10-01" {
			t.Errorf("Days eight days on = %+v, want 2026-10-01 dropped", d)
		}
	}
}

func TestDays_count_run_time_of_timed_default_branch_runs(t *testing.T) {
	s := open(t, t.TempDir())
	notStarted := run(2, forge.RunPassing, t0.Add(-2*time.Hour))
	notStarted.StartedAt = time.Time{}
	r := withDefault(scanBack(t0, 12*time.Hour, run(1, forge.RunFailing, t0.Add(-time.Hour)), notStarted,
		run(3, forge.RunPassing, t0.Add(-3*time.Hour))))
	r.Listings[0].Runs[2].OnDefault = false
	commit(t, s, "gh", r)
	got := s.Days("gh")
	if len(got) != 1 || got[0].Timed != 1 || got[0].Seconds != 290 {
		t.Errorf("Days of a timed default-branch run, an untimed one and one off the default branch = %+v, want 1 timed run of 290 s", got)
	}
}

func TestDays_a_whole_discovery_forgets_the_days_of_repositories_it_did_not_keep(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", scanBack(t0, 12*time.Hour, run(1, forge.RunPassing, t0.Add(-time.Hour)), inRepo(run(2, forge.RunFailing, t0.Add(-time.Hour)), "o/s")))
	r := scanBack(t0.Add(time.Hour), 12*time.Hour)
	r.Kept = map[string]bool{"o/r": true}
	commit(t, s, "gh", r)
	for _, d := range s.Days("gh") {
		if d.Repo == "o/s" {
			t.Errorf("Days after a whole discovery that dropped o/s = %+v, want none of o/s", d)
		}
	}
}

func TestDays_a_read_whose_delivery_stopped_short_leaves_the_days_as_they_were(t *testing.T) {
	s := open(t, t.TempDir())
	failed := run(1, forge.RunFailing, t0.Add(-time.Hour))
	commit(t, s, "gh", scanBack(t0, 12*time.Hour, failed))
	rerun := *failed
	rerun.State, rerun.UpdatedAt = forge.RunPassing, t0.Add(time.Minute)
	err := s.Commit(t.Context(), "gh", scanBack(t0.Add(5*time.Minute), 12*time.Hour, &rerun), func([]Change) bool { return false })
	if err == nil {
		t.Fatal("Setup: Commit with a delivery that stopped short = nil, want errUndelivered")
	}
	want := []DayCount{dayOf("o/r", "2026-10-01", 0, 1, 0, false)}
	if got := untimed(s.Days("gh")); !slices.Equal(got, want) {
		t.Errorf("Days after an undelivered re-run = %+v, want the failure as recorded before: %+v", got, want)
	}
}

func TestDays_round_trip_through_the_file(t *testing.T) {
	s := open(t, t.TempDir())
	commit(t, s, "gh", withDefault(scanBack(t0, 30*time.Hour, run(1, forge.RunFailing, t0.Add(-time.Hour)))))
	want := s.Days("gh")
	if got := reload(t, s).Days("gh"); len(want) == 0 || !slices.Equal(got, want) {
		t.Errorf("Days after a reload = %+v, want %+v", got, want)
	}
}

func TestLoad_sets_aside_a_file_holding_a_day_the_store_never_writes(t *testing.T) {
	days := func(day string) string {
		return `{"version":2,"connections":{"gh":{"delivered":{},"verdicts":{},"days":{"o/r":` + day + `}}}}`
	}
	for name, body := range map[string]string{
		"not_a_date":      days(`{"2026-10-1":{"runs":{"1":{"state":"passing"}}}}`),
		"run_key_not_id":  days(`{"2026-10-01":{"runs":{"01":{"state":"passing"}}}}`),
		"state_unknown":   days(`{"2026-10-01":{"runs":{"1":{"state":"broken"}}}}`),
		"untimed_seconds": days(`{"2026-10-01":{"runs":{"1":{"state":"passing","seconds":5}}}}`),
		"negative_time":   days(`{"2026-10-01":{"runs":{"1":{"state":"passing","seconds":-5,"timed":true}}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, fileName), []byte(body), 0o600); err != nil {
				t.Fatalf("Setup: %v", err)
			}
			if d, err := open(t, dir).Load(t.Context()); err != nil || d == nil || d.Reason == nil {
				t.Errorf("Load(%s) = %+v, %v, want the file set aside", body, d, err)
			}
		})
	}
	good := days(`{"2026-10-01":{"runs":{"1":{"state":"passing","seconds":5,"timed":true}},"clipped":true}}`)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(good), 0o600); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	s := open(t, dir)
	want := []DayCount{{Repo: "o/r", Day: "2026-10-01", Passing: 1, Timed: 1, Seconds: 5, Clipped: true}}
	if d, err := s.Load(t.Context()); d != nil || err != nil || !slices.Equal(s.Days("gh"), want) {
		t.Errorf("Load(%s) = %+v, %v with days %+v, want it accepted as %+v", good, d, err, s.Days("gh"), want)
	}
}
