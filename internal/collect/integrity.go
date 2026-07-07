package collect

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/cplieger/github-scout/internal/model"
)

// scanIntegrity accumulates per-signal collection outcomes during one scan and
// renders the data-integrity verdict. Its job is to keep a reported "0" from a
// signal that could NOT be read distinct from a confirmed-empty "0" — for code
// scanning, the difference between "no open alerts" and a silent security
// false-negative.
//
// Two tiers:
//
//   - degraded (WARN-level concern): any signal read failed this scan. Surfaced
//     as the degraded / errors / failed_signals fields on `scan complete`, so
//     the dashboard's Scan Integrity tile reddens. A single incidental per-repo
//     failure — a transient 5xx, or a private repo without GitHub Advanced
//     Security returning 403 — lands here and is tolerated, not paged.
//   - escalated (`scan degraded`, ERROR-level): a SYSTEMIC failure — a token
//     rejected on EVERY read this scan (a pervasive 401, see tokenInvalid) or a
//     rate limit (429), which poison the whole scan — OR discovery returned
//     zero repos (a valid token that can see nothing, so nothing was scanned)
//     — OR a signal blind across the board (every repo that has it failed, or a
//     single-call search failed). This is what an alert fires on.
//
// A SPARSE 401 — one rejected among reads that otherwise succeeded — is NOT
// systemic: GitHub returns intermittent 401s under a secondary-rate-limit burst
// even on a valid token (and discovery, on the same token, already succeeded to
// reach here). Such a 401 reddens the integrity tile via failed_signals but
// does NOT page as a dead credential. It still escalates if it happens to blind
// an entire signal — via the runsBlind / codeScanningBlind / signal_blind paths
// (cause named by signal), not as token_invalid.
//
// A repo with no code scanning (model.ErrNoCodeScanning, GitHub's 404) counts
// as neither readable nor failed, so it never dilutes the "code scanning blind
// for every repo" test: a genuine missing-scope failure (403 on every repo that
// DOES have code scanning) still escalates even when other repos simply lack
// the feature.
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

// classify maps a collection error to an outcome and, for a real failure,
// records whether it is a systemic (org-wide) credential or rate-limit
// problem. The github client (the adapter) has already translated the
// org-wide HTTP statuses into domain sentinels — 401 to model.ErrTokenInvalid,
// 429 to model.ErrRateLimited — so this classifies on meaning and never sees an
// HTTP transport type. A 403 is deliberately NOT systemic: the client leaves it
// as a plain error, so on code scanning (where it usually means one private
// repo lacks GitHub Advanced Security) it is a per-repo failure that escalates
// only via the "blind for every repo" path.
func (sc *scanIntegrity) classify(err error) outcome {
	switch {
	case err == nil:
		return outcomeOK
	case isShutdown(err):
		return outcomeShutdown
	case errors.Is(err, model.ErrNoCodeScanning):
		return outcomeNoData
	}
	// A real read failure: flag the org-wide credential / quota classes.
	switch {
	case errors.Is(err, model.ErrTokenInvalid):
		sc.tokenRejected = true
	case errors.Is(err, model.ErrRateLimited):
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

// tokenInvalid reports a PERVASIVE 401: a read returned 401 AND nothing was read
// this scan. Gating on "nothing read" (rather than firing on any single 401) is
// the fix for transient-401 false alarms — GitHub returns intermittent 401s
// under a secondary-rate-limit burst even on a valid token, and discovery, which
// runs first on the same token, already succeeded to reach the per-signal reads.
// A sparse 401 among successful reads is left to the per-signal blind paths
// (which still escalate a fully-dark signal, by signal), while a truly dead
// token 401s discovery itself, handled in Scan (which flips health).
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
