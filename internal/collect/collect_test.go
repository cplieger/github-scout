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
	repos    []model.Repo
	reposErr error
	runs     map[string][]model.FailedRun // keyed by repo full name
	runsErr  map[string]error
	calls    int // ListFailedRuns call count
}

func (f *fakeClient) ListRepos(_ context.Context, _ string) ([]model.Repo, error) {
	return f.repos, f.reposErr
}

func (f *fakeClient) ListFailedRuns(_ context.Context, repo model.Repo, _ time.Time) ([]model.FailedRun, error) {
	f.calls++
	return f.runs[repo.FullName()], f.runsErr[repo.FullName()]
}

func fixedNow() time.Time { return time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) }

func newCollector(t *testing.T, fc *fakeClient, exclude map[string]bool) *Collector {
	t.Helper()
	return New(Deps{
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

func TestScanHealthyWithNoFailures(t *testing.T) {
	fc := &fakeClient{repos: []model.Repo{{Owner: "cplieger", Name: "clean"}}}
	if !newCollector(t, fc, nil).Scan(context.Background()) {
		t.Errorf("Scan should be healthy when discovery works, even with zero failures")
	}
}

func TestScanPartialRunFailureStillHealthy(t *testing.T) {
	fc := &fakeClient{
		repos:   []model.Repo{{Owner: "cplieger", Name: "x"}},
		runsErr: map[string]error{"cplieger/x": errors.New("one conclusion 500'd")},
	}
	if !newCollector(t, fc, nil).Scan(context.Background()) {
		t.Errorf("a per-repo run-list error must not flip the scan unhealthy")
	}
}

func TestScanExcludesRepos(t *testing.T) {
	fc := &fakeClient{repos: []model.Repo{
		{Owner: "cplieger", Name: "x"},
		{Owner: "cplieger", Name: "noisy"},
	}}
	newCollector(t, fc, map[string]bool{"noisy": true}).Scan(context.Background())
	if fc.calls != 1 {
		t.Errorf("ListFailedRuns called %d times, want 1 (noisy excluded)", fc.calls)
	}
}

func TestDedupEmitsEachRunOnce(t *testing.T) {
	run := model.FailedRun{Repo: "cplieger/x", RunID: 100, CreatedAt: fixedNow().Add(-1 * time.Hour)}
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "x"}},
		runs:  map[string][]model.FailedRun{"cplieger/x": {run}},
	}
	c := newCollector(t, fc, nil)

	rec := &recordingHandler{}
	c.logger = slog.New(rec)

	c.Scan(context.Background()) // first sighting → emit
	c.Scan(context.Background()) // second sighting → suppressed

	if rec.countMsg("workflow run failed") != 1 {
		t.Errorf("run emitted %d times, want exactly 1", rec.countMsg("workflow run failed"))
	}
}

func TestPruneDropsRunsOlderThanLookback(t *testing.T) {
	old := model.FailedRun{Repo: "cplieger/x", RunID: 1, CreatedAt: fixedNow().Add(-100 * time.Hour)} // older than 72h lookback
	fresh := model.FailedRun{Repo: "cplieger/x", RunID: 2, CreatedAt: fixedNow().Add(-1 * time.Hour)}
	fc := &fakeClient{
		repos: []model.Repo{{Owner: "cplieger", Name: "x"}},
		runs:  map[string][]model.FailedRun{"cplieger/x": {old, fresh}},
	}
	c := newCollector(t, fc, nil)
	c.Scan(context.Background())

	// The old run is pruned (outside lookback); the fresh one is retained.
	if _, ok := c.seen[1]; ok {
		t.Errorf("run 1 (older than lookback) should have been pruned")
	}
	if _, ok := c.seen[2]; !ok {
		t.Errorf("run 2 (within lookback) should be retained")
	}
}

// --- test logging helpers ---

// testWriter routes a logger's output into t.Log so failing tests show it.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// recordingHandler counts emitted records by message for assertions.
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
