package collect

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/model"
)

// fakeClient is a scripted apiClient for driving the collector without HTTP.
type fakeClient struct {
	reposErr  error
	prsErr    error
	issuesErr error
	runs      map[string][]model.WorkflowRun
	runsErr   map[string]error
	alerts    map[string][]model.CodeScanningAlert
	alertsErr map[string]error
	prs       []model.PullRequest
	issues    []model.Issue
	repos     []model.Repo
	runCalls  int
}

func (f *fakeClient) ListRepos(context.Context, string) ([]model.Repo, error) {
	return f.repos, f.reposErr
}

func (f *fakeClient) ListRuns(_ context.Context, repo model.Repo, _ time.Time) ([]model.WorkflowRun, error) {
	f.runCalls++
	return f.runs[repo.FullName()], f.runsErr[repo.FullName()]
}

func (f *fakeClient) SearchOpenPRs(context.Context, string, string) ([]model.PullRequest, error) {
	return f.prs, f.prsErr
}

func (f *fakeClient) SearchOpenIssues(context.Context, string, string) ([]model.Issue, error) {
	return f.issues, f.issuesErr
}

func (f *fakeClient) ListCodeScanningAlerts(_ context.Context, repo model.Repo) ([]model.CodeScanningAlert, error) {
	return f.alerts[repo.FullName()], f.alertsErr[repo.FullName()]
}

func fixedNow() time.Time { return time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) }

func newCollector(t *testing.T, fc *fakeClient, exclude map[string]bool) *Collector {
	t.Helper()
	return New(&Deps{
		Client:   fc,
		Logger:   slog.New(slog.NewTextHandler(testWriter{t}, nil)),
		Now:      fixedNow,
		Owner:    "cplieger",
		Lookback: 72 * time.Hour,
		Exclude:  exclude,
	})
}

func TestScanDiscoveryFailureUnhealthy(t *testing.T) {
	fc := &fakeClient{reposErr: errors.New("bad token")}
	if newCollector(t, fc, nil).Scan(context.Background()) {
		t.Errorf("Scan should be unhealthy when repo discovery fails")
	}
}

func TestScanHealthyWithNoSignals(t *testing.T) {
	fc := &fakeClient{repos: []model.Repo{{Owner: "cplieger", Name: "clean"}}}
	if !newCollector(t, fc, nil).Scan(context.Background()) {
		t.Errorf("Scan should be healthy when discovery works, even with zero signals")
	}
}

func TestScanPartialFailuresStillHealthy(t *testing.T) {
	// PR search, issue search, run listing, and alert listing all error, but
	// discovery succeeded — the scan must stay healthy while REPORTING the
	// degradation: every signal is blind, so degraded=true, errors=4, all four
	// names in failed_signals, and exactly one ERROR-level "scan degraded".
	fc := &fakeClient{
		repos:     []model.Repo{{Owner: "cplieger", Name: "x"}},
		prsErr:    errors.New("pr search 500"),
		issuesErr: errors.New("issue search 500"),
		runsErr:   map[string]error{"cplieger/x": errors.New("runs 500")},
		alertsErr: map[string]error{"cplieger/x": errors.New("alerts 500")},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	if !c.Scan(context.Background()) {
		t.Errorf("partial per-signal failures must not flip the scan unhealthy")
	}
	if d, ok := rec.boolAttr("scan complete", "degraded"); !ok || !d {
		t.Errorf("degraded = %v (found=%v), want true when every signal failed", d, ok)
	}
	if n, ok := rec.intAttr("scan complete", "errors"); !ok || n != 4 {
		t.Errorf("errors = %d (found=%v), want 4 (PRs, issues, runs, alerts)", n, ok)
	}
	if got, _ := rec.strAttr("scan complete", "failed_signals"); got != "open_prs,open_issues,runs,code_scanning" {
		t.Errorf("failed_signals = %q, want all four signals", got)
	}
	if rec.countMsg("scan degraded") != 1 {
		t.Errorf("a fully-blind scan must emit exactly one ERROR-level \"scan degraded\" line, got %d", rec.countMsg("scan degraded"))
	}
	// All four signals are blind here with no systemic flag, so the diagnosis
	// ladder resolves to code_scanning_blind — pinning that it outranks
	// runs_blind / signal_blind (the security signal is the most actionable).
	if cause, _ := rec.strAttr("scan degraded", "cause"); cause != "code_scanning_blind" {
		t.Errorf("cause = %q, want code_scanning_blind (it outranks runs_blind/signal_blind)", cause)
	}
}

func TestScanEmitsAllFourSignals(t *testing.T) {
	fc := &fakeClient{
		repos:  []model.Repo{{Owner: "cplieger", Name: "x"}},
		prs:    []model.PullRequest{{Repo: "cplieger/x", Number: 1, Title: "feat"}},
		issues: []model.Issue{{Repo: "cplieger/x", Number: 2, Title: "bug"}},
		runs:   map[string][]model.WorkflowRun{"cplieger/x": {{Repo: "cplieger/x", RunID: 9, Conclusion: "failure", CreatedAt: fixedNow().Add(-1 * time.Hour)}}},
		alerts: map[string][]model.CodeScanningAlert{"cplieger/x": {{Repo: "cplieger/x", Number: 3, Rule: "go/sql-injection"}}},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	for _, msg := range []string{"open pull request", "open issue", "workflow run", "code scanning alert"} {
		if rec.countMsg(msg) != 1 {
			t.Errorf("msg %q emitted %d times, want 1", msg, rec.countMsg(msg))
		}
	}

	// The "scan complete" summary must count the one item of each signal
	// (pins the open_prs / open_issues counters).
	if n, ok := rec.intAttr("scan complete", "open_prs"); !ok || n != 1 {
		t.Errorf("open_prs = %d (found=%v), want 1", n, ok)
	}
	if n, ok := rec.intAttr("scan complete", "open_issues"); !ok || n != 1 {
		t.Errorf("open_issues = %d (found=%v), want 1", n, ok)
	}
	// No run-list error occurred, so the partial-failure warning must not fire.
	if n := rec.countMsg("partial failure listing runs"); n != 0 {
		t.Errorf("successful run listing emitted %d partial-failure warnings, want 0", n)
	}
}

func TestRunsDedupButSnapshotsRepeat(t *testing.T) {
	// Workflow runs are event-once; PRs/issues/alerts are snapshots emitted
	// every scan. Two scans => run once, but PR/issue/alert twice.
	fc := &fakeClient{
		repos:  []model.Repo{{Owner: "cplieger", Name: "x"}},
		prs:    []model.PullRequest{{Repo: "cplieger/x", Number: 1}},
		issues: []model.Issue{{Repo: "cplieger/x", Number: 2}},
		runs:   map[string][]model.WorkflowRun{"cplieger/x": {{Repo: "cplieger/x", RunID: 9, Conclusion: "success", CreatedAt: fixedNow().Add(-1 * time.Hour)}}},
		alerts: map[string][]model.CodeScanningAlert{"cplieger/x": {{Repo: "cplieger/x", Number: 3}}},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())
	c.Scan(context.Background())

	if got := rec.countMsg("workflow run"); got != 1 {
		t.Errorf("workflow run emitted %d times, want 1 (event-once dedup)", got)
	}
	for _, msg := range []string{"open pull request", "open issue", "code scanning alert"} {
		if got := rec.countMsg(msg); got != 2 {
			t.Errorf("snapshot %q emitted %d times over 2 scans, want 2", msg, got)
		}
	}
}

func TestScanEmitsEveryRunAndCountsFailures(t *testing.T) {
	// Every completed run is emitted as msg="workflow run" (not just
	// failures); the scan summary reports new_runs (all) and new_failures
	// (only the failureConclusions set — success and cancelled are excluded).
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "x"}},
		runs: map[string][]model.WorkflowRun{"cplieger/x": {
			{Repo: "cplieger/x", RunID: 1, Conclusion: "success", CreatedAt: fixedNow().Add(-1 * time.Hour)},
			{Repo: "cplieger/x", RunID: 2, Conclusion: "failure", CreatedAt: fixedNow().Add(-2 * time.Hour)},
			{Repo: "cplieger/x", RunID: 3, Conclusion: "timed_out", CreatedAt: fixedNow().Add(-3 * time.Hour)},
			{Repo: "cplieger/x", RunID: 4, Conclusion: "cancelled", CreatedAt: fixedNow().Add(-4 * time.Hour)},
		}},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if got := rec.countMsg("workflow run"); got != 4 {
		t.Errorf("emitted %d workflow-run lines, want 4 (every completed run, not just failures)", got)
	}
	if n, ok := rec.intAttr("scan complete", "new_runs"); !ok || n != 4 {
		t.Errorf("new_runs = %d (found=%v), want 4", n, ok)
	}
	if n, ok := rec.intAttr("scan complete", "new_failures"); !ok || n != 2 {
		t.Errorf("new_failures = %d (found=%v), want 2 (failure + timed_out)", n, ok)
	}
}

func TestExcludeReposSkipsAllSignals(t *testing.T) {
	// Excluded repo must be skipped for runs/alerts (per-repo loop) AND
	// filtered from the cross-repo PR/issue snapshots.
	fc := &fakeClient{
		repos:  []model.Repo{{Owner: "cplieger", Name: "x"}, {Owner: "cplieger", Name: "noisy"}},
		prs:    []model.PullRequest{{Repo: "cplieger/noisy", Number: 1}, {Repo: "cplieger/x", Number: 2}},
		issues: []model.Issue{{Repo: "cplieger/noisy", Number: 3}},
		alerts: map[string][]model.CodeScanningAlert{"cplieger/x": {{Repo: "cplieger/x", Number: 4}}},
	}
	c := newCollector(t, fc, map[string]bool{"noisy": true})
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if fc.runCalls != 1 {
		t.Errorf("ListRuns called %d times, want 1 (noisy excluded)", fc.runCalls)
	}
	if rec.countMsg("open pull request") != 1 {
		t.Errorf("expected only the non-noisy PR, got %d", rec.countMsg("open pull request"))
	}
	if rec.countMsg("open issue") != 0 {
		t.Errorf("noisy repo's issue should be filtered, got %d", rec.countMsg("open issue"))
	}

	// The per-repo loop counters surfaced in the summary: one repo scanned
	// ("x"), one skipped ("noisy"). Pins the scanned++/skipped++ increments.
	if n, ok := rec.intAttr("scan complete", "scanned"); !ok || n != 1 {
		t.Errorf("scanned = %d (found=%v), want 1 (only the non-excluded repo)", n, ok)
	}
	if n, ok := rec.intAttr("scan complete", "skipped"); !ok || n != 1 {
		t.Errorf("skipped = %d (found=%v), want 1 (the excluded repo)", n, ok)
	}
}

func TestPruneDropsRunsOlderThanLookback(t *testing.T) {
	old := model.WorkflowRun{Repo: "cplieger/x", RunID: 1, CreatedAt: fixedNow().Add(-100 * time.Hour)}
	fresh := model.WorkflowRun{Repo: "cplieger/x", RunID: 2, CreatedAt: fixedNow().Add(-1 * time.Hour)}
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "x"}},
		runs:  map[string][]model.WorkflowRun{"cplieger/x": {old, fresh}},
	}
	c := newCollector(t, fc, nil)
	c.Scan(context.Background())
	if _, ok := c.seen[1]; ok {
		t.Errorf("run 1 (older than lookback) should have been pruned")
	}
	if _, ok := c.seen[2]; !ok {
		t.Errorf("run 2 (within lookback) should be retained")
	}
}

// TestStatePersistsDedupAcrossProcesses simulates two separate Ofelia
// trigger processes sharing the same /tmp: the first scan emits the run and
// persists the dedup set; a fresh collector loading the same state file must
// NOT re-emit it. This is the property that makes trigger-mode dedup work.
func TestStatePersistsDedupAcrossProcesses(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "seen-runs.json")
	runs := map[string][]model.WorkflowRun{"cplieger/x": {
		{Repo: "cplieger/x", RunID: 9, Conclusion: "failure", CreatedAt: fixedNow().Add(-1 * time.Hour)},
	}}
	mk := func() (*Collector, *recordingHandler) {
		rec := &recordingHandler{}
		fc := &fakeClient{repos: []model.Repo{{Owner: "cplieger", Name: "x"}}, runs: runs}
		c := New(&Deps{Client: fc, Logger: slog.New(rec), Now: fixedNow, Owner: "cplieger", Lookback: 72 * time.Hour, StatePath: statePath})
		return c, rec
	}

	c1, rec1 := mk()
	c1.Scan(context.Background())
	if got := rec1.countMsg("workflow run"); got != 1 {
		t.Fatalf("first trigger emitted %d workflow-run lines, want 1", got)
	}
	// A successful atomic save must not log the save-failure warning.
	if got := rec1.countMsg("dedup state save failed"); got != 0 {
		t.Errorf("successful state save emitted %d save-failure warnings, want 0", got)
	}

	c2, rec2 := mk() // fresh "process", same state file
	c2.Scan(context.Background())
	if got := rec2.countMsg("workflow run"); got != 0 {
		t.Errorf("second trigger emitted %d workflow-run lines, want 0 (dedup state reloaded)", got)
	}
	if _, ok := c2.seen[9]; !ok {
		t.Errorf("run 9 should be present in the reloaded dedup set")
	}
}

// TestStateCorruptStartsCold verifies a corrupt/garbage state file is
// tolerated: the collector starts with an empty set (re-emitting the
// lookback window once) rather than panicking, and rewrites valid state.
func TestStateCorruptStartsCold(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "seen-runs.json")
	if err := os.WriteFile(statePath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recordingHandler{}
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "x"}},
		runs:  map[string][]model.WorkflowRun{"cplieger/x": {{Repo: "cplieger/x", RunID: 9, Conclusion: "failure", CreatedAt: fixedNow().Add(-1 * time.Hour)}}},
	}
	c := New(&Deps{Client: fc, Logger: slog.New(rec), Now: fixedNow, Owner: "cplieger", Lookback: 72 * time.Hour, StatePath: statePath})
	c.Scan(context.Background()) // must not panic
	if got := rec.countMsg("workflow run"); got != 1 {
		t.Errorf("corrupt state should start cold and emit the run; got %d", got)
	}
	if _, ok := c.seen[9]; !ok {
		t.Errorf("run 9 should be in the set after the cold scan")
	}
}

func TestNewDefaultsNowToWallClock(t *testing.T) {
	// New with no Now must fall back to time.Now, not leave the clock nil:
	// a scan reads c.now() up front and would panic on a nil func.
	fc := &fakeClient{repos: []model.Repo{{Owner: "cplieger", Name: "x"}}}
	c := New(&Deps{
		Client:   fc,
		Logger:   slog.New(slog.NewTextHandler(testWriter{t}, nil)),
		Owner:    "cplieger",
		Lookback: 72 * time.Hour,
	}) // Now intentionally omitted

	if !c.Scan(context.Background()) {
		t.Errorf("Scan with the default wall-clock now should be healthy")
	}
}

func TestNewDefaultsLoggerWhenNil(t *testing.T) {
	c := New(&Deps{
		Client:   &fakeClient{},
		Now:      fixedNow,
		Owner:    "cplieger",
		Lookback: 72 * time.Hour,
	})
	if c.logger == nil {
		t.Errorf("New with nil Logger left c.logger nil; want slog.Default fallback")
	}
}

func TestExcludedRepo(t *testing.T) {
	c := &Collector{exclude: map[string]bool{"b": true, "noisy": true}}

	// Excluded by the bare name after the last slash.
	if !c.excludedRepo("owner/noisy") {
		t.Errorf("excludedRepo(owner/noisy) = false, want true")
	}
	// Slash at index 1 still strips to the bare name "b" (pins i != -1,
	// distinguishing it from an i != 1 mutant that would keep "a/b").
	if !c.excludedRepo("a/b") {
		t.Errorf("excludedRepo(a/b) = false, want true (bare name b excluded)")
	}
	// A non-excluded repo passes through.
	if c.excludedRepo("owner/keep") {
		t.Errorf("excludedRepo(owner/keep) = true, want false")
	}
	// Case-insensitive lookup: a lowercased exclude key ("noisy") matches a
	// mixed-case repo name. GitHub repo names are case-insensitive, so the
	// lookup lowercases before probing the set; this kills the strings.ToLower
	// mutant in excludedRepo that every lowercase-only case above survives.
	if !c.excludedRepo("owner/NOISY") {
		t.Errorf("excludedRepo(owner/NOISY) = false, want true (case-insensitive lookup)")
	}
	// A nil exclude set excludes nothing.
	var none Collector
	if none.excludedRepo("owner/anything") {
		t.Errorf("excludedRepo with nil set = true, want false")
	}
}

// --- test logging helpers ---

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

type recordingHandler struct{ records []slog.Record }

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) countMsg(msg string) int {
	n := 0
	for _, r := range h.records {
		if r.Message == msg {
			n++
		}
	}
	return n
}

// intAttr returns the int64 value of attribute key on the first record with
// the given message, used to assert scan-summary counts (new_runs, etc.).
func (h *recordingHandler) intAttr(msg, key string) (int64, bool) {
	for _, r := range h.records {
		if r.Message != msg {
			continue
		}
		var (
			out   int64
			found bool
		)
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				out, found = a.Value.Int64(), true
				return false
			}
			return true
		})
		return out, found
	}
	return 0, false
}

// attrKeys returns the set of attribute keys on the first record whose
// message is msg, or nil if no such record exists. Used to assert a signal's
// emitted slog field names against the matching model type's JSON tag set.
func (h *recordingHandler) attrKeys(msg string) map[string]bool {
	for _, r := range h.records {
		if r.Message != msg {
			continue
		}
		keys := make(map[string]bool)
		r.Attrs(func(a slog.Attr) bool {
			keys[a.Key] = true
			return true
		})
		return keys
	}
	return nil
}

// TestPruneBoundaryRetainsRunsAtCutoff pins prune's inclusive lower edge: a
// run created at exactly the cutoff is retained, one created a nanosecond
// earlier is dropped. fixedNow sits on a whole-second boundary, so Scan's
// Truncate(time.Second) leaves the cutoff unchanged and the nanosecond delta
// survives. Kills the created.Before(cutoff) -> !created.After(cutoff)
// boundary mutant that the existing -100h/-1h cases both survive.
func TestPruneBoundaryRetainsRunsAtCutoff(t *testing.T) {
	cutoff := fixedNow().Add(-72 * time.Hour)
	atCutoff := model.WorkflowRun{Repo: "cplieger/x", RunID: 1, CreatedAt: cutoff}
	justBefore := model.WorkflowRun{Repo: "cplieger/x", RunID: 2, CreatedAt: cutoff.Add(-time.Nanosecond)}
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "x"}},
		runs:  map[string][]model.WorkflowRun{"cplieger/x": {atCutoff, justBefore}},
	}
	c := newCollector(t, fc, nil)
	c.Scan(context.Background())
	if _, ok := c.seen[1]; !ok {
		t.Errorf("run 1 (created at exactly cutoff) should be retained, not pruned")
	}
	if _, ok := c.seen[2]; ok {
		t.Errorf("run 2 (one nanosecond before cutoff) should have been pruned")
	}
}

// TestStateOversizedStartsCold exercises loadState's maxStateBytes cap: a
// state file larger than the limit trips atomicfile.ReadBounded's
// ErrFileTooLarge, which loadState must tolerate by warning and starting cold
// rather than OOMing or failing. The within-lookback run is then re-emitted.
func TestStateOversizedStartsCold(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "seen-runs.json")
	if err := os.WriteFile(statePath, make([]byte, maxStateBytes+100), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recordingHandler{}
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "x"}},
		runs:  map[string][]model.WorkflowRun{"cplieger/x": {{Repo: "cplieger/x", RunID: 9, Conclusion: "failure", CreatedAt: fixedNow().Add(-1 * time.Hour)}}},
	}
	c := New(&Deps{Client: fc, Logger: slog.New(rec), Now: fixedNow, Owner: "cplieger", Lookback: 72 * time.Hour, StatePath: statePath})
	c.Scan(context.Background()) // must not OOM or panic
	if got := rec.countMsg("workflow run"); got != 1 {
		t.Errorf("oversized state should start cold and emit the run; got %d", got)
	}
	if _, ok := c.seen[9]; !ok {
		t.Errorf("run 9 should be in the set after the cold scan")
	}
}

// TestStateNullJSONDoesNotNilMap guards loadState's "if seen != nil" check. A
// state file of literal JSON "null" unmarshals successfully into a nil map;
// without the guard that nil would replace the initialized c.seen and the
// next collectRuns insert would panic with "assignment to entry in nil map".
// Distinct from the corrupt-JSON case, which errors before reaching the guard.
func TestStateNullJSONDoesNotNilMap(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "seen-runs.json")
	if err := os.WriteFile(statePath, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recordingHandler{}
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "x"}},
		runs:  map[string][]model.WorkflowRun{"cplieger/x": {{Repo: "cplieger/x", RunID: 9, Conclusion: "failure", CreatedAt: fixedNow().Add(-1 * time.Hour)}}},
	}
	c := New(&Deps{Client: fc, Logger: slog.New(rec), Now: fixedNow, Owner: "cplieger", Lookback: 72 * time.Hour, StatePath: statePath})
	c.Scan(context.Background()) // must not panic on a nil-map insert
	if got := rec.countMsg("workflow run"); got != 1 {
		t.Errorf("null state should start cold and emit the run; got %d", got)
	}
	if _, ok := c.seen[9]; !ok {
		t.Errorf("run 9 should be in the set after the cold scan")
	}
}

// TestSavePersistsOnlyPrunedSet verifies saveState persists the post-prune
// set: a run beyond the lookback window is pruned before saveState, so it is
// absent from the reloaded set, while a within-lookback run survives. This
// pins the Scan ordering (prune runs before saveState) and exercises the
// saveState marshal+write success branch (otherwise uncovered).
func TestSavePersistsOnlyPrunedSet(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "seen-runs.json")
	runs := map[string][]model.WorkflowRun{"cplieger/x": {
		{Repo: "cplieger/x", RunID: 200, Conclusion: "failure", CreatedAt: fixedNow().Add(-200 * time.Hour)},
		{Repo: "cplieger/x", RunID: 1, Conclusion: "failure", CreatedAt: fixedNow().Add(-1 * time.Hour)},
	}}
	fc := &fakeClient{repos: []model.Repo{{Owner: "cplieger", Name: "x"}}, runs: runs}
	c1 := New(&Deps{
		Client: fc, Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil)),
		Now: fixedNow, Owner: "cplieger", Lookback: 72 * time.Hour, StatePath: statePath,
	})
	c1.Scan(context.Background())

	// A fresh "process" reloads the persisted set; only the post-prune set
	// was saved, so the beyond-lookback run must not reappear.
	fc2 := &fakeClient{repos: []model.Repo{{Owner: "cplieger", Name: "x"}}}
	c2 := New(&Deps{
		Client: fc2, Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil)),
		Now: fixedNow, Owner: "cplieger", Lookback: 72 * time.Hour, StatePath: statePath,
	})

	if _, ok := c2.seen[200]; ok {
		t.Errorf("run 200 (beyond lookback, pruned before saveState) must NOT be in the persisted set")
	}
	if _, ok := c2.seen[1]; !ok {
		t.Errorf("run 1 (within lookback) must be in the persisted set")
	}
}

// TestLogKeysMatchModelTags pins the REAL Loki field-name contract. The four
// signals reach Loki as slog.Info lines whose field names are the literal keys
// emitted in this package — the model structs are never JSON-marshaled on the
// emit path — so the model JSON tags only document those keys, and the two are
// kept in sync by hand. This test runs one Scan with all four signals present
// and asserts each msg's recorded slog attribute key set equals the matching
// model type's JSON tag set; a rename on either side (a model tag or a
// collect.go slog key) fails the build.
func TestLogKeysMatchModelTags(t *testing.T) {
	fc := &fakeClient{
		repos:  []model.Repo{{Owner: "cplieger", Name: "x"}},
		prs:    []model.PullRequest{{Repo: "cplieger/x", Number: 1, Title: "feat"}},
		issues: []model.Issue{{Repo: "cplieger/x", Number: 2, Title: "bug"}},
		runs:   map[string][]model.WorkflowRun{"cplieger/x": {{Repo: "cplieger/x", RunID: 9, Conclusion: "failure", CreatedAt: fixedNow().Add(-1 * time.Hour)}}},
		alerts: map[string][]model.CodeScanningAlert{"cplieger/x": {{Repo: "cplieger/x", Number: 3, Rule: "go/sql-injection"}}},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	cases := []struct {
		signal any
		msg    string
	}{
		{model.WorkflowRun{}, "workflow run"},
		{model.PullRequest{}, "open pull request"},
		{model.Issue{}, "open issue"},
		{model.CodeScanningAlert{}, "code scanning alert"},
	}
	for _, tc := range cases {
		t.Run(tc.msg, func(t *testing.T) {
			got := rec.attrKeys(tc.msg)
			if got == nil {
				t.Fatalf("no %q line was emitted", tc.msg)
			}
			want := jsonTagSet(tc.signal)
			if !maps.Equal(got, want) {
				t.Errorf("slog keys for %q do not match %T JSON tags:\n slog keys: %v\n json tags: %v",
					tc.msg, tc.signal, sortedKeys(got), sortedKeys(want))
			}
		})
	}
}

// jsonTagSet returns the set of JSON field names declared by v's struct tags
// (the name before any comma; untagged fields and those tagged "-" are skipped).
func jsonTagSet(v any) map[string]bool {
	t := reflect.TypeOf(v)
	tags := make(map[string]bool, t.NumField())
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			tags[name] = true
		}
	}
	return tags
}

// sortedKeys returns m's keys in sorted order, for deterministic failure output.
func sortedKeys(m map[string]bool) []string {
	return slices.Sorted(maps.Keys(m))
}

// boolAttr returns the bool value of attribute key on the first record with
// the given message, used to assert scan-summary flags (degraded,
// auth_or_ratelimit).
func (h *recordingHandler) boolAttr(msg, key string) (value, found bool) {
	for _, r := range h.records {
		if r.Message != msg {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				value, found = a.Value.Bool(), true
				return false
			}
			return true
		})
		return value, found
	}
	return false, false
}

// strAttr returns the string value of attribute key on the first record with
// the given message (e.g. failed_signals).
func (h *recordingHandler) strAttr(msg, key string) (value string, found bool) {
	for _, r := range h.records {
		if r.Message != msg {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				value, found = a.Value.String(), true
				return false
			}
			return true
		})
		return value, found
	}
	return "", false
}

// TestScanCleanScanNotDegraded pins the happy path: when every signal is
// collected, the summary reports degraded=false / errors=0 / failed_signals=""
// and no "scan degraded" line fires. This is the baseline the integrity
// fields must distinguish a real blackout from.
func TestScanCleanScanNotDegraded(t *testing.T) {
	fc := &fakeClient{
		repos:  []model.Repo{{Owner: "cplieger", Name: "x"}},
		prs:    []model.PullRequest{{Repo: "cplieger/x", Number: 1}},
		alerts: map[string][]model.CodeScanningAlert{"cplieger/x": {{Repo: "cplieger/x", Number: 2}}},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if d, ok := rec.boolAttr("scan complete", "degraded"); !ok || d {
		t.Errorf("degraded = %v (found=%v), want false on a clean scan", d, ok)
	}
	if n, ok := rec.intAttr("scan complete", "errors"); !ok || n != 0 {
		t.Errorf("errors = %d (found=%v), want 0 on a clean scan", n, ok)
	}
	if got, _ := rec.strAttr("scan complete", "failed_signals"); got != "" {
		t.Errorf("failed_signals = %q, want empty on a clean scan", got)
	}
	if rec.countMsg("scan degraded") != 0 {
		t.Errorf("a clean scan must not emit a \"scan degraded\" line, got %d", rec.countMsg("scan degraded"))
	}
}

// TestScanDegradedReportsBlindCodeScanning verifies that when code-scanning
// fails for the only repo, the security signal is blind: the scan stays
// healthy but emits degraded telemetry AND an ERROR "scan degraded" line whose
// cause is the code-scanning blackout (not auth/rate-limit).
func TestScanDegradedReportsBlindCodeScanning(t *testing.T) {
	fc := &fakeClient{
		repos:     []model.Repo{{Owner: "cplieger", Name: "x"}},
		alertsErr: map[string]error{"cplieger/x": errors.New("alerts 500")},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	if !c.Scan(context.Background()) {
		t.Errorf("a blind code-scanning read must not flip health unhealthy")
	}
	if got, _ := rec.strAttr("scan complete", "failed_signals"); got != "code_scanning" {
		t.Errorf("failed_signals = %q, want \"code_scanning\"", got)
	}
	if rec.countMsg("scan degraded") != 1 {
		t.Fatalf("blind code-scanning must emit one \"scan degraded\" line, got %d", rec.countMsg("scan degraded"))
	}
	if cause, _ := rec.strAttr("scan degraded", "cause"); cause != "code_scanning_blind" {
		t.Errorf("cause = %q, want code_scanning_blind for an all-repos code-scanning failure", cause)
	}
}

// TestScanTokenInvalidEscalates: a 401 (rejected token) is systemic — it
// poisons every call — so it escalates with cause=token_invalid regardless of
// which signal hit it, and never flips health (a restart cannot fix a dead
// token).
func TestScanTokenInvalidEscalates(t *testing.T) {
	fc := &fakeClient{
		repos:     []model.Repo{{Owner: "cplieger", Name: "x"}},
		alertsErr: map[string]error{"cplieger/x": model.ErrTokenInvalid},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	if !c.Scan(context.Background()) {
		t.Errorf("a 401 must not flip health (a restart cannot fix a dead token)")
	}
	if rec.countMsg("scan degraded") != 1 {
		t.Fatalf("a 401 must emit one \"scan degraded\" line, got %d", rec.countMsg("scan degraded"))
	}
	if cause, _ := rec.strAttr("scan degraded", "cause"); cause != "token_invalid" {
		t.Errorf("cause = %q, want token_invalid for a 401", cause)
	}
}

// TestScanIncidentalRepoFailureNotEscalated pins the "incidental tolerated,
// systemic loud" line: with two repos and code-scanning failing for only one
// (a transient 500), the signal is incomplete (degraded=true, listed in
// failed_signals) but NOT blind across the board, so it must NOT escalate to
// an ERROR "scan degraded".
func TestScanIncidentalRepoFailureNotEscalated(t *testing.T) {
	fc := &fakeClient{
		repos:     []model.Repo{{Owner: "cplieger", Name: "a"}, {Owner: "cplieger", Name: "b"}},
		alertsErr: map[string]error{"cplieger/a": errors.New("alerts 500")},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if d, ok := rec.boolAttr("scan complete", "degraded"); !ok || !d {
		t.Errorf("degraded = %v (found=%v), want true (one repo's alerts were unreadable)", d, ok)
	}
	if got, _ := rec.strAttr("scan complete", "failed_signals"); got != "code_scanning" {
		t.Errorf("failed_signals = %q, want \"code_scanning\"", got)
	}
	if rec.countMsg("scan degraded") != 0 {
		t.Errorf("an incidental single-repo failure must NOT escalate to ERROR; got %d \"scan degraded\" lines", rec.countMsg("scan degraded"))
	}
}

// TestScanPerRepo403NotEscalated is the key calibration test: a single repo
// returning a per-repo failure on code scanning (the normal case is a 403 from
// a private repo without GitHub Advanced Security, which the github client
// leaves unmapped as a plain error) is a "couldn't read" that reddens the
// integrity tile but must NOT page, because another repo's code scanning read
// fine. Such a per-repo failure is not a systemic token/quota problem. (The
// 403-stays-unmapped half of the contract is pinned in client_test.go.)
func TestScanPerRepo403NotEscalated(t *testing.T) {
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "a"}, {Owner: "cplieger", Name: "b"}},
		alerts: map[string][]model.CodeScanningAlert{
			"cplieger/b": {{Repo: "cplieger/b", Number: 1}},
		},
		alertsErr: map[string]error{"cplieger/a": errors.New("alerts 403")},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if d, ok := rec.boolAttr("scan complete", "degraded"); !ok || !d {
		t.Errorf("degraded = %v (found=%v), want true (one repo's code scanning was unreadable)", d, ok)
	}
	if got, _ := rec.strAttr("scan complete", "failed_signals"); got != "code_scanning" {
		t.Errorf("failed_signals = %q, want code_scanning", got)
	}
	if n := rec.countMsg("scan degraded"); n != 0 {
		t.Errorf("a per-repo 403 with another repo readable must NOT escalate; got %d \"scan degraded\" lines", n)
	}
}

// TestScanCodeScanningScopeBlindEscalates: when EVERY repo that has code
// scanning fails to read (a missing token scope returns 403 for all of them,
// not one repo lacking GHAS), the security signal is dark across the board and
// must escalate.
func TestScanCodeScanningScopeBlindEscalates(t *testing.T) {
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "a"}, {Owner: "cplieger", Name: "b"}},
		alertsErr: map[string]error{
			"cplieger/a": errors.New("alerts 403"),
			"cplieger/b": errors.New("alerts 403"),
		},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if rec.countMsg("scan degraded") != 1 {
		t.Fatalf("code scanning blind for every repo must escalate once, got %d", rec.countMsg("scan degraded"))
	}
	if cause, _ := rec.strAttr("scan degraded", "cause"); cause != "code_scanning_blind" {
		t.Errorf("cause = %q, want code_scanning_blind", cause)
	}
}

// TestScanNoCodeScanningExcludedFromBlind: a repo with no code scanning (404 ->
// ErrNoCodeScanning) is excluded from the "blind" denominator, so when the only
// repo that HAS code scanning returns 403, the signal is still blind and
// escalates — the 404 repo does not mask it.
func TestScanNoCodeScanningExcludedFromBlind(t *testing.T) {
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "nogha"}, {Owner: "cplieger", Name: "gha"}},
		alertsErr: map[string]error{
			"cplieger/nogha": model.ErrNoCodeScanning,
			"cplieger/gha":   errors.New("alerts 403"),
		},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if rec.countMsg("scan degraded") != 1 {
		t.Fatalf("the only code-scanning-capable repo being blind must escalate; the 404 repo must not dilute it, got %d", rec.countMsg("scan degraded"))
	}
	if cause, _ := rec.strAttr("scan degraded", "cause"); cause != "code_scanning_blind" {
		t.Errorf("cause = %q, want code_scanning_blind", cause)
	}
}

// TestScanNoCodeScanningCleanWhenOthersRead: a 404 (no code scanning) repo is
// benign — when every other repo's code scanning reads fine, the scan is not
// degraded at all (the 404 is neither a failure nor counted as a read).
func TestScanNoCodeScanningCleanWhenOthersRead(t *testing.T) {
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "nogha"}, {Owner: "cplieger", Name: "gha"}},
		alerts: map[string][]model.CodeScanningAlert{
			"cplieger/gha": {{Repo: "cplieger/gha", Number: 1}},
		},
		alertsErr: map[string]error{"cplieger/nogha": model.ErrNoCodeScanning},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if d, _ := rec.boolAttr("scan complete", "degraded"); d {
		t.Errorf("a repo with no code scanning is benign; scan must not be degraded")
	}
	if rec.countMsg("scan degraded") != 0 {
		t.Errorf("no code scanning on one repo must not escalate; got %d", rec.countMsg("scan degraded"))
	}
}

// TestScanRateLimitedEscalates: a 429 is systemic (the quota is shared across
// every call), so it escalates with cause=rate_limited.
func TestScanRateLimitedEscalates(t *testing.T) {
	fc := &fakeClient{
		repos:     []model.Repo{{Owner: "cplieger", Name: "x"}},
		alertsErr: map[string]error{"cplieger/x": model.ErrRateLimited},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if rec.countMsg("scan degraded") != 1 {
		t.Fatalf("a 429 must escalate once, got %d", rec.countMsg("scan degraded"))
	}
	if cause, _ := rec.strAttr("scan degraded", "cause"); cause != "rate_limited" {
		t.Errorf("cause = %q, want rate_limited for a 429", cause)
	}
}

// TestScanRunsBlindEscalates: when workflow-run listing fails for every scanned
// repo, the CI-failure signal is dark and escalates with cause=runs_blind. Pins
// the runs-blind path (otherwise reachable only incidentally).
func TestScanRunsBlindEscalates(t *testing.T) {
	fc := &fakeClient{
		repos:   []model.Repo{{Owner: "cplieger", Name: "x"}},
		runsErr: map[string]error{"cplieger/x": errors.New("runs 500")},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if got, _ := rec.strAttr("scan complete", "failed_signals"); got != "runs" {
		t.Errorf("failed_signals = %q, want runs", got)
	}
	if rec.countMsg("scan degraded") != 1 {
		t.Fatalf("runs blind for every repo must escalate once, got %d", rec.countMsg("scan degraded"))
	}
	if cause, _ := rec.strAttr("scan degraded", "cause"); cause != "runs_blind" {
		t.Errorf("cause = %q, want runs_blind", cause)
	}
}

// TestScanSearchAuthEscalates: a 401 on the cross-repo PR search both blinds
// that single-call signal and flags the systemic token problem; cause is
// token_invalid (the most actionable diagnosis).
func TestScanSearchAuthEscalates(t *testing.T) {
	fc := &fakeClient{
		repos:  []model.Repo{{Owner: "cplieger", Name: "x"}},
		prsErr: model.ErrTokenInvalid,
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if got, _ := rec.strAttr("scan complete", "failed_signals"); !strings.Contains(got, "open_prs") {
		t.Errorf("failed_signals = %q, want it to include open_prs", got)
	}
	if rec.countMsg("scan degraded") != 1 {
		t.Fatalf("a 401 on the PR search must escalate once, got %d", rec.countMsg("scan degraded"))
	}
	if cause, _ := rec.strAttr("scan degraded", "cause"); cause != "token_invalid" {
		t.Errorf("cause = %q, want token_invalid", cause)
	}
}

// TestScanContextCancelNotDegraded: a SIGTERM mid-scan cancels in-flight reads;
// context cancellation is a clean shutdown, not a data failure, so the scan is
// not marked degraded and does not escalate.
func TestScanContextCancelNotDegraded(t *testing.T) {
	fc := &fakeClient{
		repos:     []model.Repo{{Owner: "cplieger", Name: "x"}},
		prsErr:    context.Canceled,
		issuesErr: context.Canceled,
		runsErr:   map[string]error{"cplieger/x": context.Canceled},
		alertsErr: map[string]error{"cplieger/x": context.Canceled},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	if !c.Scan(context.Background()) {
		t.Errorf("discovery succeeded, so the scan stays healthy even when reads are cancelled")
	}
	if d, _ := rec.boolAttr("scan complete", "degraded"); d {
		t.Errorf("context cancellation is a clean shutdown, not degradation")
	}
	if rec.countMsg("scan degraded") != 0 {
		t.Errorf("a cancelled scan must not escalate; got %d", rec.countMsg("scan degraded"))
	}
}

// TestScanScannedZeroNotDegraded: when every repo is excluded (scanned==0) and
// the searches succeed, there are no per-repo reads to fail, so the scan is
// clean — the blind checks are guarded against a spurious zero-repo escalation.
func TestScanScannedZeroNotDegraded(t *testing.T) {
	fc := &fakeClient{repos: []model.Repo{{Owner: "cplieger", Name: "skip"}}}
	c := newCollector(t, fc, map[string]bool{"skip": true})
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if d, _ := rec.boolAttr("scan complete", "degraded"); d {
		t.Errorf("no repos scanned and searches OK -> not degraded")
	}
	if rec.countMsg("scan degraded") != 0 {
		t.Errorf("scanned==0 must not escalate; got %d", rec.countMsg("scan degraded"))
	}
}

// TestScanSearchFailureNonSystemicEscalatesSignalBlind pins two things the
// other escalation tests leave uncovered: that a failed cross-repo search
// escalates ON ITS OWN (a single-call signal — one failure blinds it), and
// that with no systemic flag and the per-repo signals reading fine, the
// diagnosis falls to the generic signal_blind cause. Without this, a mutant
// deleting `|| sc.prsFailed` / `|| sc.issuesFailed` from escalate() survives
// (every other search-failure test also trips a systemic flag), and the
// diagnosis() default branch is never exercised.
func TestScanSearchFailureNonSystemicEscalatesSignalBlind(t *testing.T) {
	clean := func() *fakeClient {
		return &fakeClient{
			repos:  []model.Repo{{Owner: "cplieger", Name: "x"}},
			runs:   map[string][]model.WorkflowRun{"cplieger/x": {{Repo: "cplieger/x", RunID: 1, Conclusion: "success", CreatedAt: fixedNow().Add(-time.Hour)}}},
			alerts: map[string][]model.CodeScanningAlert{"cplieger/x": {{Repo: "cplieger/x", Number: 2}}},
		}
	}
	cases := []struct {
		set    func(*fakeClient)
		name   string
		signal string
	}{
		{func(fc *fakeClient) { fc.prsErr = errors.New("pr search 500") }, "pr search", "open_prs"},
		{func(fc *fakeClient) { fc.issuesErr = errors.New("issue search 500") }, "issue search", "open_issues"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := clean()
			tc.set(fc)
			c := newCollector(t, fc, nil)
			rec := &recordingHandler{}
			c.logger = slog.New(rec)
			c.Scan(context.Background())

			if got, _ := rec.strAttr("scan complete", "failed_signals"); got != tc.signal {
				t.Errorf("failed_signals = %q, want %q", got, tc.signal)
			}
			if rec.countMsg("scan degraded") != 1 {
				t.Fatalf("a failed cross-repo %s must escalate on its own, got %d", tc.name, rec.countMsg("scan degraded"))
			}
			if cause, _ := rec.strAttr("scan degraded", "cause"); cause != "signal_blind" {
				t.Errorf("cause = %q, want signal_blind for a non-systemic %s failure", cause, tc.name)
			}
		})
	}
}

// TestScanErrCountSumsPerRepoFailures pins errCount's per-repo accumulation:
// runs and code scanning count once PER failing repo, while a cross-repo search
// counts once total. Two repos both failing runs AND alerts, plus a failed PR
// search, must report errors = 2 + 2 + 1 = 5. A "= 1" mutant on the per-repo
// counters would read 4 here (and the single-repo tests can't tell the
// difference), and failed_signals must name every failing signal in order.
func TestScanErrCountSumsPerRepoFailures(t *testing.T) {
	fc := &fakeClient{
		repos:  []model.Repo{{Owner: "cplieger", Name: "a"}, {Owner: "cplieger", Name: "b"}},
		prsErr: errors.New("pr search 500"),
		runsErr: map[string]error{
			"cplieger/a": errors.New("runs 500"),
			"cplieger/b": errors.New("runs 500"),
		},
		alertsErr: map[string]error{
			"cplieger/a": errors.New("alerts 500"),
			"cplieger/b": errors.New("alerts 500"),
		},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if n, ok := rec.intAttr("scan complete", "errors"); !ok || n != 5 {
		t.Errorf("errors = %d (found=%v), want 5 (runs x2 + code_scanning x2 + open_prs x1)", n, ok)
	}
	if got, _ := rec.strAttr("scan complete", "failed_signals"); got != "open_prs,runs,code_scanning" {
		t.Errorf("failed_signals = %q, want open_prs,runs,code_scanning", got)
	}
}

// TestScanTokenInvalidBeatsRateLimited pins the top diagnosis precedence rung:
// when one scan trips BOTH systemic flags (a 401 on one signal, a 429 on
// another), the most-actionable cause wins — token_invalid outranks
// rate_limited. Without this a rung-swap mutant in diagnosis() survives (no
// other test sets both systemic flags at once).
func TestScanTokenInvalidBeatsRateLimited(t *testing.T) {
	fc := &fakeClient{
		repos:     []model.Repo{{Owner: "cplieger", Name: "x"}},
		prsErr:    model.ErrTokenInvalid,
		issuesErr: model.ErrRateLimited,
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if rec.countMsg("scan degraded") != 1 {
		t.Fatalf("both systemic flags set must still emit exactly one \"scan degraded\", got %d", rec.countMsg("scan degraded"))
	}
	if cause, _ := rec.strAttr("scan degraded", "cause"); cause != "token_invalid" {
		t.Errorf("cause = %q, want token_invalid (it outranks rate_limited)", cause)
	}
}

// TestScanZeroReposVisibleEscalates: discovery SUCCEEDS but returns zero repos
// (a valid token that has lost repo visibility, or a fine-grained PAT scoped to
// nothing). Nothing was scanned, so a reported "0 alerts / 0 issues" is
// unverified, not confirmed clean — the same false-negative the per-signal
// blind paths guard against, one level up. The scan stays healthy (discovery
// did not fail, so a restart cannot restore visibility) but is degraded AND
// escalates with cause=no_repos_visible. Contrast TestScanScannedZeroNotDegraded,
// where discovery returns repos that are all operator-excluded (intentional, so
// not flagged).
func TestScanZeroReposVisibleEscalates(t *testing.T) {
	fc := &fakeClient{repos: nil} // discovery OK, but the token sees zero repos
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	if !c.Scan(context.Background()) {
		t.Errorf("zero-repo discovery succeeded, so the scan stays healthy (a restart won't restore visibility)")
	}
	if d, ok := rec.boolAttr("scan complete", "degraded"); !ok || !d {
		t.Errorf("degraded = %v (found=%v), want true when nothing was scanned", d, ok)
	}
	if rec.countMsg("scan degraded") != 1 {
		t.Fatalf("a zero-repo scan must escalate once, got %d", rec.countMsg("scan degraded"))
	}
	if cause, _ := rec.strAttr("scan degraded", "cause"); cause != "no_repos_visible" {
		t.Errorf("cause = %q, want no_repos_visible", cause)
	}
}

// TestScanDiscoveryCancelIsCleanShutdown: a context cancellation during repo
// discovery (a SIGTERM or deadline landing on the first API call) is a clean
// shutdown, not a token failure — it must NOT log an ERROR "repo discovery
// failed" (which reads like a dead token and bumps the fleet error panel) and
// must NOT flip health unhealthy, mirroring how the per-signal collectors treat
// cancellation. Distinct from TestScanDiscoveryFailureUnhealthy, where a real
// error correctly flips health.
func TestScanDiscoveryCancelIsCleanShutdown(t *testing.T) {
	for _, cancelErr := range []error{context.Canceled, context.DeadlineExceeded} {
		fc := &fakeClient{reposErr: cancelErr}
		c := newCollector(t, fc, nil)
		rec := &recordingHandler{}
		c.logger = slog.New(rec)
		if !c.Scan(context.Background()) {
			t.Errorf("a discovery %v is a clean shutdown; health must not flip unhealthy", cancelErr)
		}
		if n := rec.countMsg("repo discovery failed"); n != 0 {
			t.Errorf("discovery %v must not log a discovery-failure ERROR, got %d", cancelErr, n)
		}
	}
}

// TestSnapshotsFilterForeignOwnerRepos pins the foreign-owner defensive filter
// in collectPRs/collectIssues: the cross-repo search may return a repo outside
// the configured owner (or a look-alike login like cplieger-evil that shares a
// prefix), and both must be dropped before emission. Only the cplieger/x items
// survive; someoneelse/y and cplieger-evil/z are filtered.
func TestSnapshotsFilterForeignOwnerRepos(t *testing.T) {
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "x"}},
		prs: []model.PullRequest{
			{Repo: "cplieger/x", Number: 1},
			{Repo: "someoneelse/y", Number: 2},
			{Repo: "cplieger-evil/z", Number: 3},
		},
		issues: []model.Issue{
			{Repo: "cplieger/x", Number: 4},
			{Repo: "someoneelse/y", Number: 5},
			{Repo: "cplieger-evil/z", Number: 6},
		},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if got := rec.countMsg("open pull request"); got != 1 {
		t.Errorf("emitted %d PR lines, want 1 (foreign-owner PRs must be filtered)", got)
	}
	if got := rec.countMsg("open issue"); got != 1 {
		t.Errorf("emitted %d issue lines, want 1 (foreign-owner issues must be filtered)", got)
	}
}

// TestSaveStateWriteFailureToleratedAndWarns exercises saveState's best-effort
// contract: a write failure is logged once and never flips the scan unhealthy.
// Making statePath's parent a regular file means atomicfile.WriteFile cannot
// create its temp file there (ENOTDIR), which fails deterministically on every
// OS including as root (a chmod-based approach would be bypassed by root in CI
// containers).
func TestSaveStateWriteFailureToleratedAndWarns(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(notADir, "seen-runs.json")
	rec := &recordingHandler{}
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "x"}},
		runs: map[string][]model.WorkflowRun{"cplieger/x": {
			{
				Repo: "cplieger/x", RunID: 9, Conclusion: "failure",
				CreatedAt: fixedNow().Add(-1 * time.Hour),
			},
		}},
	}
	c := New(&Deps{
		Client: fc, Logger: slog.New(rec), Now: fixedNow,
		Owner: "cplieger", Lookback: 72 * time.Hour, StatePath: statePath,
	})
	if !c.Scan(context.Background()) {
		t.Errorf("a best-effort state-save failure must not flip the scan unhealthy")
	}
	if got := rec.countMsg("dedup state save failed"); got != 1 {
		t.Errorf("save failure should warn once; got %d", got)
	}
}

// TestScanStopsScanningOnContextCancel pins the per-repo loop's shutdown guard:
// a context already cancelled (a SIGTERM landing mid-scan) must break the loop
// at its `if ctx.Err() != nil` check before any repo is processed, so ListRuns
// is never called even though discovery returned two repos. The existing
// TestScanContextCancelNotDegraded injects context.Canceled as fake-returned
// errors but never passes a cancelled context to Scan, leaving the break uncovered.
func TestScanStopsScanningOnContextCancel(t *testing.T) {
	fc := &fakeClient{repos: []model.Repo{{Owner: "cplieger", Name: "a"}, {Owner: "cplieger", Name: "b"}}}
	c := newCollector(t, fc, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if !c.Scan(ctx) {
		t.Errorf("discovery succeeded, so the scan stays healthy even when the context is cancelled")
	}
	if fc.runCalls != 0 {
		t.Errorf("ListRuns called %d times, want 0 (the per-repo loop must break at its ctx.Err() guard)", fc.runCalls)
	}
}

// TestKeepIsCaseInsensitiveOnOwner pins keep()'s case-insensitive owner match:
// GitHub's search can return the owner in a different case than the configured
// GITHUB_OWNER, so keep() lowercases both sides; without that, a case mismatch
// would silently filter every result. A configured owner of "Cplieger" must
// still keep "cplieger/x" and "CPLIEGER/x" while dropping a foreign owner.
func TestKeepIsCaseInsensitiveOnOwner(t *testing.T) {
	c := &Collector{owner: "Cplieger"}

	if !c.keep("cplieger/x") {
		t.Errorf("keep(cplieger/x) with owner Cplieger = false, want true (owner match is case-insensitive)")
	}
	if !c.keep("CPLIEGER/x") {
		t.Errorf("keep(CPLIEGER/x) with owner Cplieger = false, want true (owner match is case-insensitive)")
	}
	if c.keep("someoneelse/x") {
		t.Errorf("keep(someoneelse/x) = true, want false (foreign owner must be dropped)")
	}
}

// TestCollectRunsEmitsPartialRunsOnError pins collectRuns's partial-emit
// contract: when ListRuns returns the runs it managed to collect ALONGSIDE an
// error (a mid-pagination failure), the collector still emits those runs while
// folding the error into the integrity verdict.
func TestCollectRunsEmitsPartialRunsOnError(t *testing.T) {
	fc := &fakeClient{
		repos:   []model.Repo{{Owner: "cplieger", Name: "x"}},
		runs:    map[string][]model.WorkflowRun{"cplieger/x": {{Repo: "cplieger/x", RunID: 9, Conclusion: "failure", CreatedAt: fixedNow().Add(-1 * time.Hour)}}},
		runsErr: map[string]error{"cplieger/x": errors.New("list runs page 2: server error 500")},
	}
	c := newCollector(t, fc, nil)
	rec := &recordingHandler{}
	c.logger = slog.New(rec)
	c.Scan(context.Background())

	if got := rec.countMsg("workflow run"); got != 1 {
		t.Errorf("emitted %d workflow-run lines, want 1 (the partial set must be emitted even though the list errored)", got)
	}
	if n, ok := rec.intAttr("scan complete", "new_failures"); !ok || n != 1 {
		t.Errorf("new_failures = %d (found=%v), want 1", n, ok)
	}
	if got, _ := rec.strAttr("scan complete", "failed_signals"); got != "runs" {
		t.Errorf("failed_signals = %q, want \"runs\"", got)
	}
	if d, ok := rec.boolAttr("scan complete", "degraded"); !ok || !d {
		t.Errorf("degraded = %v (found=%v), want true", d, ok)
	}
}
