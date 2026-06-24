package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/collect"
	"github.com/cplieger/github-scout/internal/model"
	"pgregory.net/rapid"
)

type panicClient struct{}

func (panicClient) ListRepos(context.Context, string) ([]model.Repo, error) {
	panic("simulated scan panic")
}

func (panicClient) ListRuns(context.Context, model.Repo, time.Time) ([]model.WorkflowRun, error) {
	return nil, nil
}

func (panicClient) SearchOpenPRs(context.Context, string, string) ([]model.PullRequest, error) {
	return nil, nil
}

func (panicClient) SearchOpenIssues(context.Context, string, string) ([]model.Issue, error) {
	return nil, nil
}

func (panicClient) ListCodeScanningAlerts(context.Context, model.Repo) ([]model.CodeScanningAlert, error) {
	return nil, nil
}

func TestRunScan_recoversPanicAsUnhealthy(t *testing.T) {
	collector := collect.New(&collect.Deps{
		Client: panicClient{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Owner:  "cplieger",
	})
	got := runScan(context.Background(), collector)
	if got {
		t.Errorf("runScan(panicking collector) = true, want false")
	}
}

type healthyClient struct{}

func (healthyClient) ListRepos(context.Context, string) ([]model.Repo, error) {
	return nil, nil
}

func (healthyClient) ListRuns(context.Context, model.Repo, time.Time) ([]model.WorkflowRun, error) {
	return nil, nil
}

func (healthyClient) SearchOpenPRs(context.Context, string, string) ([]model.PullRequest, error) {
	return nil, nil
}

func (healthyClient) SearchOpenIssues(context.Context, string, string) ([]model.Issue, error) {
	return nil, nil
}

func (healthyClient) ListCodeScanningAlerts(context.Context, model.Repo) ([]model.CodeScanningAlert, error) {
	return nil, nil
}

func TestRunScan_healthyScanReturnsTrue(t *testing.T) {
	collector := collect.New(&collect.Deps{
		Client: healthyClient{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Owner:  "cplieger",
	})
	if got := runScan(context.Background(), collector); !got {
		t.Errorf("runScan(healthy collector) = false, want true")
	}
}

func TestJitteredDelay_staysWithin10PercentBand(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		interval := time.Duration(rapid.Int64Range(int64(time.Second), int64(24*time.Hour)).Draw(t, "interval"))
		got := jitteredDelay(interval)
		lower := interval - interval/10
		upper := interval + interval/10
		if got < lower || got > upper {
			t.Fatalf("jitteredDelay(%v) = %v, want within [%v, %v]", interval, got, lower, upper)
		}
		if got <= 0 {
			t.Fatalf("jitteredDelay(%v) = %v, want positive", interval, got)
		}
	})
}

func TestJitteredDelay_appliesJitter(t *testing.T) {
	const interval = time.Second
	seen := make(map[time.Duration]bool)
	for range 1000 {
		seen[jitteredDelay(interval)] = true
	}
	if len(seen) < 2 {
		t.Errorf("jitteredDelay(%v) produced %d distinct value(s) over 1000 draws, want >= 2", interval, len(seen))
	}
}
