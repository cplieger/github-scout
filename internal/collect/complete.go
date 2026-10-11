package collect

import (
	"log/slog"
	"strings"

	"github.com/cplieger/github-scout/internal/forge"
)

// completeAttrs is the scan complete line of one connection. Every family's
// read state comes from the ledger; a family not read, or withheld, carries
// no count, so a missing number is never read as zero.
func completeAttrs(sc *scan, r *connResult, b *burst) []slog.Attr {
	l := &r.ledger
	attrs := []slog.Attr{slog.Int64("scan_id", sc.id)}
	attrs = append(attrs, l.readAttrs()...)
	if l.listed() && l.state(famRepos) != readBlind {
		attrs = append(attrs, slog.Int("repos_discovered", r.sum.reposDiscovered),
			slog.Bool("repos_truncated", l.fam[famRepos].drops[dropCut] > 0),
			slog.Int("skipped", l.fam[famRepos].drops[dropExcludedRepo]))
		attrs = append(attrs, workAttrs(sc, r)...)
	}
	if l.listed() && l.fam[famRepos].drops[dropCut] == 0 {
		attrs = append(attrs, slog.String("owners_empty", strings.Join(r.ownersEmpty, ",")))
	}
	if l.fam[famRuns].begun && !l.fam[famRuns].unsupported {
		attrs = append(attrs, runAttrs(r, b)...)
	}
	if l.fam[famWorkflows].begun {
		attrs = append(attrs, workflowAttrs(r, b)...)
	}
	if l.fam[famSecurity].begun {
		attrs = append(attrs, securityAttrs(r, b)...)
	}
	attrs = append(attrs,
		slog.Int("errors", l.errCount()), slog.Bool("degraded", l.degraded()), slog.String("failed_signals", l.failedSignals()))
	attrs = append(attrs, budgetAttrs(r.budget)...)
	return append(attrs, slog.Int64("duration_ms", r.sum.duration.Milliseconds()))
}

// workAttrs counts open pull requests and issues, with their age and origin.
func workAttrs(sc *scan, r *connResult) []slog.Attr {
	l := &r.ledger
	var ages ageBuckets
	othersPRs, othersIssues, failingChecks := 0, 0, 0
	for i := range r.prs {
		pr := &r.prs[i].pr
		ages.add(sc.start, pr.CreatedAt, pr.UpdatedAt)
		if from(r, pr.Author) == fromOthers {
			othersPRs++
		}
		if r.prs[i].checks == forge.CheckFailing {
			failingChecks++
		}
	}
	for i := range r.issues {
		is := &r.issues[i].issue
		ages.add(sc.start, is.CreatedAt, is.UpdatedAt)
		if from(r, is.Author) == fromOthers {
			othersIssues++
		}
	}
	prsShown, issuesShown := !l.withheld(famPRs), !l.withheld(famIssues)
	var attrs []slog.Attr
	if prsShown {
		attrs = append(attrs, slog.Int("open_prs", len(r.prs)), slog.Int("excluded_prs", l.fam[famPRs].drops[dropExcludedItem]),
			slog.Int("from_others_prs", othersPRs))
	}
	if issuesShown {
		attrs = append(attrs, slog.Int("open_issues", len(r.issues)), slog.Int("excluded_issues", l.fam[famIssues].drops[dropExcludedItem]),
			slog.Int("from_others_issues", othersIssues))
	}
	if prsShown && issuesShown {
		attrs = append(attrs,
			slog.Int("open_age_lt7d", ages.lt7), slog.Int("open_age_7_30d", ages.d7to30), slog.Int("open_age_30_90d", ages.d30to90),
			slog.Int("open_age_90d_plus", ages.d90plus), slog.Int("open_age_unknown", ages.ageUnknown),
			slog.Int("idle_30d", ages.idle30), slog.Int("idle_unknown", ages.idleUnknown))
	}
	return append(attrs, checksAttrs(r, failingChecks)...)
}

// checksAttrs counts the pull requests whose head checks fail, absent where the
// pull requests were not all read, or where no checks read succeeded on a
// pull request whose checks the token may read.
func checksAttrs(r *connResult, failing int) []slog.Attr {
	fl := &r.ledger.fam[famChecks]
	if !fl.begun || fl.unsupported {
		return nil
	}
	unread := slog.Int("checks_unread", fl.drops[dropFailed]+fl.drops[dropCut]+fl.drops[dropNoHead]+fl.drops[dropRefused])
	token := slog.Int("checks_unsupported_for_token", fl.drops[dropTokenUnsupported])
	readable := len(r.prs) - fl.drops[dropTokenUnsupported]
	if r.ledger.state(famPRs) != readComplete || r.ledger.state(famChecks) == readBlind || (readable > 0 && fl.read() == 0) {
		return []slog.Attr{unread, token}
	}
	return []slog.Attr{slog.Int("failing_checks_prs", failing), unread, token}
}

// runAttrs counts the runs and the workflows the store derives from them.
func runAttrs(r *connResult, b *burst) []slog.Attr {
	fl := &r.ledger.fam[famRuns]
	attrs := make([]slog.Attr, 0, 12)
	attrs = append(attrs,
		slog.Int("new_runs", r.sum.newRuns), slog.Int("changed_runs", r.sum.changedRuns),
		slog.Int("new_failures", r.sum.newFailures), slog.Int("tracked", r.sum.delivered),
		slog.Int("runs_failed_repos", fl.drops[dropFailed]), slog.Int("runs_truncated_repos", fl.drops[dropCut]),
		slog.Int("runs_unread_repos", fl.drops[dropRefused]), slog.Int("runs_unmapped", len(r.unmapped)),
	)
	if r.ledger.state(famRuns) == readBlind || r.ledger.withheld(famFailing) {
		return attrs
	}
	return append(attrs,
		slog.Int("failing_workflows", len(r.failing)), slog.Int("failing_workflows_listed", b.failing[r]),
		slog.Int("slow_workflows_listed", b.slow[r]), slog.Int("run_durations_known", r.sum.runDurationsKnown),
	)
}

func workflowAttrs(r *connResult, b *burst) []slog.Attr {
	fl := &r.ledger.fam[famWorkflows]
	attrs := make([]slog.Attr, 0, 5)
	attrs = append(attrs,
		slog.Int("workflows_failed_repos", fl.drops[dropFailed]), slog.Int("workflows_truncated_repos", fl.drops[dropCut]),
		slog.Int("workflows_unread_repos", fl.drops[dropRefused]))
	if r.ledger.withheld(famWorkflows) {
		return attrs
	}
	return append(attrs, slog.Int("disabled_workflows", len(r.disabled)), slog.Int("disabled_workflows_listed", b.disabled[r]))
}

func securityAttrs(r *connResult, b *burst) []slog.Attr {
	fl := &r.ledger.fam[famSecurity]
	attrs := make([]slog.Attr, 0, 13)
	attrs = append(attrs,
		slog.Int("security_repos_read", fl.read()), slog.Int("security_skipped", fl.drops[dropSecuritySkipped]),
		slog.Int("security_unavailable", fl.noData), slog.Int("security_unreadable", fl.drops[dropFailed]),
		slog.Int("security_truncated_repos", fl.drops[dropCut]), slog.Int("security_unread_repos", fl.drops[dropRefused]),
	)
	if r.ledger.withheld(famSecurity) {
		return attrs
	}
	bySeverity := map[string]int{}
	for i := range r.alerts {
		bySeverity[severityBucket(r.alerts[i].Severity)]++
	}
	return append(attrs,
		slog.Int("security_alerts", len(r.alerts)), slog.Int("security_alerts_listed", b.topAlerts[r]),
		slog.Int("security_alerts_critical", bySeverity["critical"]), slog.Int("security_alerts_high", bySeverity["high"]),
		slog.Int("security_alerts_medium", bySeverity["medium"]), slog.Int("security_alerts_low", bySeverity["low"]),
		slog.Int("security_alerts_unrated", bySeverity["unrated"]),
	)
}
