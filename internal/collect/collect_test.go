package collect

import (
	"context"
	"errors"
	"log/slog"
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
	// PR search, issue search, run listing, and alert listing all error,
	// but discovery succeeded — the scan must stay healthy (degraded, logged).
	fc := &fakeClient{
		repos:     []model.Repo{{Owner: "cplieger", Name: "x"}},
		prsErr:    errors.New("pr search 500"),
		issuesErr: errors.New("issue search 500"),
		runsErr:   map[string]error{"cplieger/x": errors.New("runs 500")},
		alertsErr: map[string]error{"cplieger/x": errors.New("alerts 500")},
	}
	if !newCollector(t, fc, nil).Scan(context.Background()) {
		t.Errorf("partial per-signal failures must not flip the scan unhealthy")
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
	// (only the FailureConclusions set — success and cancelled are excluded).
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
