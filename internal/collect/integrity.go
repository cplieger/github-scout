package collect

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/cplieger/github-scout/internal/ghsignal"
)

// scanIntegrity keeps unread signals distinct from confirmed-empty signals.
type scanIntegrity struct {
	runsOK        int
	runsFailed    int
	csOK          int  // code-scanning reads that succeeded
	csFailed      int  // code-scanning reads that failed (excludes ErrNoCodeScanning)
	prsOK         bool // the cross-repo PR search returned (proves the token can read)
	prsFailed     bool
	issuesOK      bool // the cross-repo issue search returned (proves the token can read)
	issuesFailed  bool
	tokenRejected bool // >=1 read returned 401; systemic ONLY if pervasive (see tokenInvalid)
	rateLimited   bool
	noRepos       bool // discovery succeeded but returned zero repos: nothing was scanned
}

// outcome categorises one signal read for the integrity tally.
type outcome int

const (
	outcomeShutdown outcome = iota // context cancellation: a clean shutdown, not a data problem
	outcomeOK                      // read succeeded (zero or more items)
	outcomeNoData                  // signal not present for this repo (code-scanning 404): benign
	outcomeFailed                  // read failed
)

// classify records systemic status candidates without importing transport types.
func (sc *scanIntegrity) classify(err error) outcome {
	switch {
	case err == nil:
		return outcomeOK
	case isShutdown(err):
		return outcomeShutdown
	case errors.Is(err, ghsignal.ErrNoCodeScanning):
		return outcomeNoData
	}
	// A real read failure: flag the org-wide credential / quota classes.
	switch {
	case errors.Is(err, ghsignal.ErrTokenInvalid):
		sc.tokenRejected = true
	case errors.Is(err, ghsignal.ErrRateLimited):
		sc.rateLimited = true
	}
	return outcomeFailed
}

// recordDiscovery notes that repo discovery succeeded but returned zero repos.
// A valid token that can see no repositories means nothing was scanned, so
// every signal's reported 0 is "couldn't check" — the same false-negative the
// per-signal blind paths guard against, one level up. It is both degraded and
// escalated (see diagnosis: no_repos_visible). Discovery FAILURE is handled
// separately by Scan (it flips health); this is the success-with-nothing case.
func (sc *scanIntegrity) recordDiscovery(repoCount int) { sc.noRepos = repoCount == 0 }

func (sc *scanIntegrity) recordPRs(err error) {
	switch sc.classify(err) {
	case outcomeOK:
		sc.prsOK = true
	case outcomeFailed:
		sc.prsFailed = true
	}
}

func (sc *scanIntegrity) recordIssues(err error) {
	switch sc.classify(err) {
	case outcomeOK:
		sc.issuesOK = true
	case outcomeFailed:
		sc.issuesFailed = true
	}
}

func (sc *scanIntegrity) recordRuns(err error) {
	switch sc.classify(err) {
	case outcomeOK:
		sc.runsOK++
	case outcomeFailed:
		sc.runsFailed++
	}
}

func (sc *scanIntegrity) recordAlerts(err error) {
	switch sc.classify(err) {
	case outcomeOK:
		sc.csOK++
	case outcomeFailed:
		sc.csFailed++
	}
}

// errCount is the number of failed signal reads this scan: a search counts once,
// the per-repo signals count per failing repo. No-data and shutdown reads are
// excluded.
func (sc *scanIntegrity) errCount() int {
	n := sc.runsFailed + sc.csFailed
	if sc.prsFailed {
		n++
	}
	if sc.issuesFailed {
		n++
	}
	return n
}

// degraded reports whether this scan's data cannot be fully trusted — any
// signal read failed, or discovery returned zero repos so nothing was checked.
func (sc *scanIntegrity) degraded() bool { return sc.noRepos || sc.errCount() > 0 }

// failedSignals is the comma-joined list of signals with at least one failed
// read, in a stable order, for the `scan complete` summary.
func (sc *scanIntegrity) failedSignals() string {
	signals := make([]string, 0, 4)
	if sc.prsFailed {
		signals = append(signals, "open_prs")
	}
	if sc.issuesFailed {
		signals = append(signals, "open_issues")
	}
	if sc.runsFailed > 0 {
		signals = append(signals, "runs")
	}
	if sc.csFailed > 0 {
		signals = append(signals, "code_scanning")
	}
	return strings.Join(signals, ",")
}

// runsBlind / codeScanningBlind report a per-repo signal that could not be read
// from ANY repo that has it (every attempt failed, none succeeded) — the
// org-wide blackout worth paging on, as opposed to an incidental single-repo
// failure.
func (sc *scanIntegrity) runsBlind() bool         { return sc.runsFailed > 0 && sc.runsOK == 0 }
func (sc *scanIntegrity) codeScanningBlind() bool { return sc.csFailed > 0 && sc.csOK == 0 }

// anyReadSucceeded reports whether at least one signal read returned data this
// scan (any repo's runs or code scanning, or either cross-repo search). It is
// the proof the token is not globally rejected: a genuinely invalid token 401s
// every call, so one successful read means a 401 seen elsewhere this scan was
// transient (a per-call secondary-rate-limit rejection under burst), not a dead
// credential.
func (sc *scanIntegrity) anyReadSucceeded() bool {
	return sc.runsOK > 0 || sc.csOK > 0 || sc.prsOK || sc.issuesOK
}

// tokenInvalid requires a rejected token and no successful signal read.
func (sc *scanIntegrity) tokenInvalid() bool {
	return sc.tokenRejected && !sc.anyReadSucceeded()
}

// systemic reports a credential / quota failure that affects the whole scan: a
// pervasive token rejection, or any rate limit (a 429 surfaces only after httpx
// exhausts its backoff retries, so it signals a sustained quota problem worth
// surfacing even on a single signal — unlike a sparse 401).
func (sc *scanIntegrity) systemic() bool { return sc.tokenInvalid() || sc.rateLimited }

// escalate reports whether this scan's degradation warrants an ERROR-level
// `scan degraded` line (and thus an alert): a systemic failure, a scan that
// discovered zero repos (nothing checked), a blind per-repo signal, or a failed
// single-call search.
func (sc *scanIntegrity) escalate() bool {
	return sc.systemic() || sc.noRepos || sc.prsFailed || sc.issuesFailed || sc.runsBlind() || sc.codeScanningBlind()
}

// diagnosis returns a machine-readable cause and a human reason for the
// `scan degraded` line, most-actionable first.
func (sc *scanIntegrity) diagnosis() (cause, reason string) {
	switch {
	case sc.tokenInvalid():
		return "token_invalid", "the GitHub token was rejected (401) and no signal could be read this scan; every reported 0 is unverified, not confirmed empty"
	case sc.rateLimited:
		return "rate_limited", "GitHub rate-limited the scan (429); some signals could not be read, so their reported 0 is unverified"
	case sc.noRepos:
		return "no_repos_visible", "repo discovery succeeded but returned zero repositories; the token may have lost repo visibility, so every signal's reported 0 this scan is unverified, not confirmed empty"
	case sc.codeScanningBlind():
		return "code_scanning_blind", "code-scanning alerts were unreadable for every repo that has the feature (likely a missing token scope); the security signal is dark, not necessarily clean"
	case sc.runsBlind():
		return "runs_blind", "workflow runs were unreadable for every scanned repo; the CI-failure signal is dark this scan"
	default:
		return "signal_blind", "a monitored signal could not be read for any source this scan; its reported 0 is unverified"
	}
}

// emit logs the ERROR-level `scan degraded` line when escalation is warranted.
// The `scan complete` summary (emitted by Scan) always carries the degraded /
// errors / failed_signals fields regardless.
func (sc *scanIntegrity) emit(logger *slog.Logger) {
	if !sc.escalate() {
		return
	}
	cause, reason := sc.diagnosis()
	logger.Error("scan degraded",
		"cause", cause, "reason", reason,
		"failed_signals", sc.failedSignals(), "errors", sc.errCount())
}
