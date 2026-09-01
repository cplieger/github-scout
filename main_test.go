package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/collect"
	"github.com/cplieger/github-scout/internal/ghsignal"
	"github.com/cplieger/health"
)

type panicClient struct{}

func (panicClient) ListRepos(context.Context, string) ([]ghsignal.Repo, error) {
	panic("simulated scan panic")
}

func (panicClient) ListRuns(context.Context, ghsignal.Repo, time.Time) ([]ghsignal.WorkflowRun, error) {
	return nil, nil
}

func (panicClient) SearchOpenPRs(context.Context, string, string) ([]ghsignal.PullRequest, error) {
	return nil, nil
}

func (panicClient) SearchOpenIssues(context.Context, string, string) ([]ghsignal.Issue, error) {
	return nil, nil
}

func (panicClient) ListCodeScanningAlerts(context.Context, ghsignal.Repo) ([]ghsignal.CodeScanningAlert, error) {
	return nil, nil
}

func TestRunScan_recoversPanicAsUnhealthy(t *testing.T) {
	collector := collect.New(&collect.Deps{
		Client: panicClient{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Owner:  "cplieger",
	})
	got := runScan(t.Context(), collector)
	if got {
		t.Errorf("runScan(panicking collector) = true, want false")
	}
}

type healthyClient struct{}

func (healthyClient) ListRepos(context.Context, string) ([]ghsignal.Repo, error) {
	return nil, nil
}

func (healthyClient) ListRuns(context.Context, ghsignal.Repo, time.Time) ([]ghsignal.WorkflowRun, error) {
	return nil, nil
}

func (healthyClient) SearchOpenPRs(context.Context, string, string) ([]ghsignal.PullRequest, error) {
	return nil, nil
}

func (healthyClient) SearchOpenIssues(context.Context, string, string) ([]ghsignal.Issue, error) {
	return nil, nil
}

func (healthyClient) ListCodeScanningAlerts(context.Context, ghsignal.Repo) ([]ghsignal.CodeScanningAlert, error) {
	return nil, nil
}

func TestRunScan_healthyScanReturnsTrue(t *testing.T) {
	collector := collect.New(&collect.Deps{
		Client: healthyClient{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Owner:  "cplieger",
	})
	if got := runScan(t.Context(), collector); !got {
		t.Errorf("runScan(healthy collector) = false, want true")
	}
}

func TestRunScheduled_firstScanRunsImmediatelyAndSetsMarker(t *testing.T) {
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
	marker.Set(false)
	collector := collect.New(&collect.Deps{
		Client: healthyClient{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Owner:  "cplieger",
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runScheduled(ctx, time.Hour, collector, marker)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !marker.Healthy() {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("runScheduled first scan did not run within 2s; the first iteration must fire at delay 0, not after a full interval")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runScheduled did not return after context cancellation")
	}
}

// TestRunScheduled_failingScanStillRefreshesLiveness pins the marker's
// loop-liveness semantics: a failing scan (panicking collector, recovered
// by runScan) still refreshes the marker — it asserts loop progress, not
// data health. Guards the unconditional marker.Set(true) in runScheduled.
func TestRunScheduled_failingScanStillRefreshesLiveness(t *testing.T) {
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
	marker.Set(false)
	collector := collect.New(&collect.Deps{
		Client: panicClient{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Owner:  "cplieger",
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runScheduled(ctx, time.Hour, collector, marker)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !marker.Healthy() {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("runScheduled did not refresh the liveness marker after a failing scan; liveness must not depend on scan outcome")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}
