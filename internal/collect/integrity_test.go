package collect

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/model"
)

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
