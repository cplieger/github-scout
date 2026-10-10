package collect

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/cplieger/github-scout/internal/forge"
)

// family is one signal a connection's scan reads. The zero value is no
// family, so a table entry left out reads as unset, never as famRepos.
type family int

const (
	famRepos family = iota + 1
	famPRs
	famIssues
	famRuns
	famFailing
	famChecks
	famSecurity
	famWorkflows
	famState
	numFamilies
)

// families yields every family, in order.
func families(yield func(family) bool) {
	for f := famRepos; f < numFamilies; f++ {
		if !yield(f) {
			return
		}
	}
}

// familyNames spell each family in failed_signals, unsupported_signals and
// its <name>_read field.
var familyNames = [numFamilies]string{
	famRepos: "repos", famPRs: "open_prs", famIssues: "open_issues", famRuns: "runs", famFailing: "failing_workflows",
	famChecks: "pr_checks", famSecurity: "security_alerts", famWorkflows: "disabled_workflows", famState: "run_state",
}

// parent is the family each one is read through, itself for a family read
// through none: a family is never more complete than its parent.
var parent = [numFamilies]family{
	famRepos: famRepos, famPRs: famRepos, famIssues: famRepos, famRuns: famRepos, famFailing: famRuns,
	famChecks: famPRs, famSecurity: famRepos, famWorkflows: famRepos, famState: famState,
}

// reason is why something a read returned, or a whole read, is missing from
// what the scan publishes. The zero value is no reason.
type reason int

const (
	dropArchived reason = iota + 1
	dropExcludedRepo
	dropExcludedItem
	dropSecuritySkipped
	dropForkWorkflows
	dropTokenUnsupported
	dropFailed
	dropCut
	dropNoHead
	dropUnlisted
	dropUnresolved
	dropUnwritable
	dropNotDurable
	dropStopped
	dropRefused
	dropOwnerFailed
	dropNoRepos
	dropSourceBlind
	numReasons
)

// class sorts a reason by what it does to its family's read: an excluded
// drop is the operator's configuration or the signal's scope and leaves the
// read complete, a partial one leaves part of it unknown, a blind one all.
// The zero value is no class, which no reason may keep.
type class int

const (
	classExcluded class = iota + 1
	classPartial
	classBlind
)

// reasonClass gives each reason exactly one class.
var reasonClass = [numReasons]class{
	dropArchived: classExcluded, dropExcludedRepo: classExcluded, dropExcludedItem: classExcluded,
	dropSecuritySkipped: classExcluded, dropForkWorkflows: classExcluded, dropTokenUnsupported: classExcluded,
	dropFailed: classPartial, dropCut: classPartial, dropNoHead: classPartial,
	dropUnlisted: classPartial, dropUnresolved: classPartial, dropUnwritable: classPartial, dropNotDurable: classPartial,
	dropStopped: classPartial, dropRefused: classPartial,
	dropOwnerFailed: classBlind, dropNoRepos: classBlind, dropSourceBlind: classBlind,
}

// degrades reports a reason that marks the scan degraded and fails a
// trigger. A stop reports itself on scan stopped.
func (r reason) degrades() bool {
	return r != dropStopped && reasonClass[r] != classExcluded
}

// readState is a family's read on one connection, as its <name>_read field
// spells it. The zero value is unread, so a family no read reached never
// reads as complete.
type readState int

const (
	readUnread readState = iota
	readComplete
	readPartial
	readBlind
	readUnsupported
	readExcluded
)

func (s readState) String() string {
	return [...]string{"unread", "complete", "partial", "blind", "unsupported", "excluded"}[s]
}

// famLedger is one family's reads on one connection.
type famLedger struct {
	drops       [numReasons]int
	ok, noData  int
	begun, done bool
	unsupported bool
	off         bool
}

// read is the reads that returned data, whole or cut.
func (fl *famLedger) read() int { return fl.ok + fl.drops[dropCut] }

func (fl *famLedger) count(c class) int {
	n := 0
	for r, k := range fl.drops {
		if reasonClass[r] == c {
			n += k
		}
	}
	return n
}

// blind reports a family nothing it read can vouch for: a blind drop, or
// failed reads, sent or refused unsent, and none that returned data. A
// family the product does not have is never blind, nor degraded.
func (fl *famLedger) blind() bool {
	failed := fl.drops[dropFailed] + fl.drops[dropRefused]
	return !fl.unsupported && (fl.count(classBlind) > 0 || (failed > 0 && fl.read() == 0))
}

func (fl *famLedger) degraded() bool {
	if fl.unsupported {
		return false
	}
	for r, k := range fl.drops {
		if k > 0 && reason(r).degrades() {
			return true
		}
	}
	return false
}

func (fl *famLedger) state() readState {
	switch {
	case fl.unsupported:
		return readUnsupported
	case fl.off:
		return readExcluded
	case fl.blind():
		return readBlind
	case !fl.begun:
		return readUnread
	case fl.count(classPartial) > 0:
		return readPartial
	}
	return readComplete
}

// readLedger is the account of one connection's scan: every read, drop,
// exclusion, stop and failed save goes through it, and it alone answers
// what scan complete, scan stopped and scan degraded say about the reads,
// and whether the scan was complete.
type readLedger struct {
	stop       *stopInfo
	unresolved []string
	fam        [numFamilies]famLedger
	// opened is the connection opening this scan, which proves the token.
	opened        bool
	tokenRejected bool // a read answered 401; systemic only when pervasive
	rateLimited   bool
	// refused is a GitHub read answered 403 with no throttle header, after
	// which the connection's quota meter sends no request of the scan
	// (see ghquota.Meter.Admit).
	refused    bool
	connFailed bool
	timedOut   bool
	github     bool
}

func (l *readLedger) begin(f family)     { l.fam[f].begun = true }
func (l *readLedger) finish(f family)    { l.fam[f].done = true }
func (l *readLedger) unsupport(f family) { l.fam[f].unsupported = true }

func (l *readLedger) drop(f family, r reason, n int) {
	l.fam[f].begun = true
	l.fam[f].drops[r] += n
}

// classify records the families product p does not have, and security
// switched off by the operator, as soon as the product is known.
func (l *readLedger) classify(p forge.Product, securityEnabled bool) {
	if p == forge.ProductUnknown {
		return
	}
	l.github = p == forge.ProductGitHub
	if !p.PRHeads() {
		l.unsupport(famChecks)
	}
	switch {
	case p != forge.ProductGitHub:
		l.unsupport(famSecurity)
		l.unsupport(famWorkflows)
	case !securityEnabled:
		l.fam[famSecurity].off = true
	}
}

// errNoEnd is a read whose source set no End, which is never read as whole.
var errNoEnd = errors.New("listing ended without saying how")

// record classifies one read of f, ended as end when err is nil, noting the
// connection-wide failure classes. A read the quota meter did not send is a
// failed read, counted apart so its warning is the refusal's alone.
func (l *readLedger) record(f family, end forge.End, err error) outcome {
	fl := &l.fam[f]
	fl.begun = true
	if err == nil {
		switch end {
		case forge.EndWhole:
			fl.ok++
			return outcomeOK
		case forge.EndCut:
			fl.drops[dropCut]++
			return outcomePartial
		case forge.EndNone:
			fl.noData++
			return outcomeNoData
		}
		err = errNoEnd
	}
	switch {
	case errors.Is(err, forge.ErrRefused):
		fl.drops[dropRefused]++
		return outcomeRefused
	case errors.Is(err, forge.ErrTokenInvalid):
		l.tokenRejected = true
	case errors.Is(err, forge.ErrRateLimited):
		l.rateLimited = true
	case errors.Is(err, forge.ErrConnection):
		l.connFailed = true
	case errors.Is(err, forge.ErrForbidden) && l.github:
		l.refused = true
	}
	fl.drops[dropFailed]++
	return outcomeFailed
}

// halt ends the connection's reads: each family begun and not finished is
// partial, and the rest stay unread.
func (l *readLedger) halt(stopReason, phase string) {
	l.stop = &stopInfo{reason: stopReason, phase: phase}
	for f := range families {
		if fl := &l.fam[f]; fl.begun && !fl.done {
			fl.drops[dropStopped]++
		}
	}
}

// state is f's read: its own, made blind by a blind parent and partial by
// one read short.
func (l *readLedger) state(f family) readState {
	s := l.fam[f].state()
	if parent[f] == f || (s != readComplete && s != readPartial) {
		return s
	}
	switch ps := l.state(parent[f]); ps {
	case readComplete, readExcluded:
		return s
	case readBlind:
		return readBlind
	default:
		return readPartial
	}
}

// withheld reports a family that publishes no row and no count: blind, or
// cut, or read through a cut repository listing. A smaller set would read
// as items closed.
func (l *readLedger) withheld(f family) bool {
	return l.state(f) == readBlind || l.fam[f].drops[dropCut] > 0 || l.fam[famRepos].drops[dropCut] > 0
}

// listed reports a repository listing that answered.
func (l *readLedger) listed() bool { return l.fam[famRepos].read() > 0 }

func (l *readLedger) degraded() bool {
	for f := range families {
		if l.fam[f].degraded() {
			return true
		}
	}
	return false
}

// complete reports a scan that ran to its end and read every family the
// product has whole.
func (l *readLedger) complete() bool {
	if l.stop != nil {
		return false
	}
	for f := range families {
		switch l.state(f) {
		case readComplete, readUnsupported, readExcluded:
		default:
			return false
		}
	}
	return true
}

// errCount is the number of sent reads that failed, on the families the
// product has; a read refused unsent counts in its family's unread field.
func (l *readLedger) errCount() int {
	n := 0
	for f := range families {
		if !l.fam[f].unsupported {
			n += l.fam[f].drops[dropFailed]
		}
	}
	return n
}

// failedSignals names, in family order, the families with a degrading drop.
func (l *readLedger) failedSignals() string {
	return l.names(func(fl *famLedger) bool { return fl.degraded() })
}

func (l *readLedger) unsupportedSignals() string {
	return l.names(func(fl *famLedger) bool { return fl.unsupported })
}

func (l *readLedger) names(pick func(*famLedger) bool) string {
	var out []string
	for f := range families {
		if pick(&l.fam[f]) {
			out = append(out, familyNames[f])
		}
	}
	return strings.Join(out, ",")
}

// readAttrs is every family's <name>_read field and unsupported_signals,
// which scan complete and scan stopped both carry.
func (l *readLedger) readAttrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, numFamilies)
	for f := range families {
		if f != famState {
			attrs = append(attrs, slog.String(familyNames[f]+"_read", l.state(f).String()))
		}
	}
	return append(attrs, slog.String("unsupported_signals", l.unsupportedSignals()))
}

// outcome categorises one read.
type outcome int

const (
	outcomeOK      outcome = iota + 1 // read succeeded
	outcomeNoData                     // the signal does not exist here (no code-scanning analyses)
	outcomeFailed                     // read failed
	outcomePartial                    // read succeeded but was cut short
	outcomeRefused                    // read failed unsent, after GitHub refused an earlier one
)

// isShutdown reports a failed read that is the scan ending: only the scan's
// own context ending is. A request timeout under a live scan wraps
// context.DeadlineExceeded too, and is a failed read.
func isShutdown(ctx context.Context) bool { return ctx.Err() != nil }

// anyReadSucceeded is the proof the token is not rejected outright: a dead
// token fails every call, so one success makes a 401 seen elsewhere a
// transient rejection, which GitHub sends under burst on a valid token.
func (l *readLedger) anyReadSucceeded() bool {
	if l.opened {
		return true
	}
	for f := range families {
		if l.fam[f].read() > 0 {
			return true
		}
	}
	return false
}

func (l *readLedger) tokenInvalid() bool  { return l.tokenRejected && !l.anyReadSucceeded() }
func (l *readLedger) unwritable() bool    { return l.fam[famState].drops[dropUnwritable] > 0 }
func (l *readLedger) notDurable() bool    { return l.fam[famState].drops[dropNotDurable] > 0 }
func (l *readLedger) saveFailed() bool    { return l.unwritable() || l.notDurable() }
func (l *readLedger) noRepos() bool       { return l.fam[famRepos].drops[dropNoRepos] > 0 }
func (l *readLedger) blind(f family) bool { return l.fam[f].blind() }

// escalate reports a degradation worth an ERROR scan degraded line. A
// failed pull-request checks or workflows read never escalates.
func (l *readLedger) escalate() bool {
	return l.tokenInvalid() || l.rateLimited || l.refused || l.saveFailed() || l.connFailed || l.noRepos() || len(l.unresolved) > 0 ||
		l.blind(famSecurity) || l.blind(famRuns) || l.blind(famPRs) || l.blind(famIssues)
}

// timeoutHint is the remedy a scan stopped at its limit names.
const timeoutHint = "This connection's scan ran past its limit, so its open work, workflows, alerts and scan complete were not logged. " +
	"The CI run lines it logged before the limit stand. Raise scan_interval, and raise lookback to match."

// causeRule is one cause scan degraded can name: when it holds and the reason it
// gives.
type causeRule struct {
	holds  func(l *readLedger) bool
	reason func(l *readLedger) string
	cause  string
}

func always(*readLedger) bool { return true }

func says(reason string) func(*readLedger) string { return func(*readLedger) string { return reason } }

// diagnoses is every cause, most actionable first; the last always holds.
var diagnoses = []causeRule{
	{cause: "scan_timeout", holds: func(l *readLedger) bool { return l.timedOut }, reason: says(timeoutHint)},
	{cause: "token_invalid", holds: (*readLedger).tokenInvalid, reason: says("The forge rejected the token with 401, and no read succeeded on this connection. None of its signals was read.")},
	{cause: "rate_limited", holds: func(l *readLedger) bool { return l.rateLimited }, reason: says("The forge rate-limited this connection and refused further requests. The signals it did not read carry no count.")},
	{cause: "state_unwritable", holds: (*readLedger).unwritable, reason: says("The run state could not be saved. This scan's CI run lines were logged, and a restart before a save succeeds logs them again.")},
	{cause: "state_not_durable", holds: (*readLedger).notDurable, reason: says("The run state was written, but its folder could not be synced. A power loss may log this scan's CI run lines again.")},
	{cause: "connection_failed", holds: func(l *readLedger) bool { return l.connFailed }, reason: says("The connection could not be opened, or its repositories could not be listed. Nothing was read from this forge.")},
	{cause: "no_repos_visible", holds: (*readLedger).noRepos, reason: says("The instance resolves none of the configured owners, so no repository was listed and no signal was read.")},
	{cause: "owner_unresolved", holds: func(l *readLedger) bool { return len(l.unresolved) > 0 }, reason: func(l *readLedger) string {
		return "The instance does not resolve these owners: " + strings.Join(l.unresolved, ", ") + ". Their repositories were not read."
	}},
	{cause: "github_refused", holds: func(l *readLedger) bool { return l.refused }, reason: says("GitHub refused a read with 403 and no rate-limit header. " +
		"A missing permission and a secondary rate limit both answer that way, so this connection sent no further request this scan. " +
		"The warning line before this one names the read and the repository.")},
	{cause: "code_scanning_blind", holds: func(l *readLedger) bool { return l.blind(famSecurity) }, reason: says("Code scanning alerts could not be read for any repository that has them, often because the token lacks a permission. The alert count is not reported, which is not the same as no alerts.")},
	{cause: "runs_blind", holds: func(l *readLedger) bool { return l.blind(famRuns) }, reason: says("CI runs could not be read for any repository, so the CI signals were not read this scan.")},
	{cause: "signal_blind", holds: always, reason: says("An open pull request or issue list could not be read whole, so its count is not reported.")},
}

// diagnosis returns the cause and reason for scan degraded.
func (l *readLedger) diagnosis() (cause, reason string) {
	for i := range diagnoses {
		if d := &diagnoses[i]; d.holds(l) {
			return d.cause, d.reason(l)
		}
	}
	return "", ""
}
