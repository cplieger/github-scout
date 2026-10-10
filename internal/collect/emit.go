package collect

import (
	"context"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/github-scout/internal/runstate"
	"github.com/cplieger/runesafe/v2"
)

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// maxValueBytes bounds each forge-controlled value a line carries, once
// sanitized. JSON escaping at most doubles it and no line carries a dozen
// such values, so every line stays well under the 256 KiB entry limit Loki
// applies by default (limits_config max_line_size).
const maxValueBytes = 4 << 10

// cutMark ends a value cut at maxValueBytes, and counts inside the bound.
const cutMark = "…"

// bounded is s sanitized and cut at maxValueBytes, for a diagnostic line.
func bounded(s string) string {
	v, _ := runesafe.SanitizeCapped(s, maxValueBytes, cutMark)
	return v
}

// errText is err's message, bounded as a forge-controlled value: a read's
// error can quote what the forge answered.
func errText(err error) string { return bounded(err.Error()) }

// lineAttrs builds one item line, bounding every forge-controlled value and
// naming in truncated each one it cut or, for a link, left out.
type lineAttrs struct {
	attrs []slog.Attr
	cut   []string
}

func (l *lineAttrs) add(attrs ...slog.Attr) { l.attrs = append(l.attrs, attrs...) }

// text adds key's forge-controlled value s, sanitized and bounded.
func (l *lineAttrs) text(key, s string) {
	v, cut := runesafe.SanitizeCapped(s, maxValueBytes, cutMark)
	if cut {
		l.cut = append(l.cut, key)
	}
	l.attrs = append(l.attrs, slog.String(key, v))
}

// link adds the line's url s unchanged, or leaves it out when it is over the
// bound, is not an http or https URL with a host, or holds a rune the
// sanitizer would replace: a cut or altered link opens another page.
func (l *lineAttrs) link(s string) {
	if s != "" && (len(s) > maxValueBytes || !webURL(s) || runesafe.SanitizeSingleLine(s) != s) {
		l.cut = append(l.cut, "url")
		return
	}
	l.attrs = append(l.attrs, slog.String("url", s))
}

// webURL reports an http or https URL with a host, its scheme in any case
// (RFC 3986 section 3.1).
func webURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Host != "" && (strings.EqualFold(u.Scheme, "https") || strings.EqualFold(u.Scheme, "http"))
}

// done is the line's attributes, ending in truncated when a value was cut.
func (l *lineAttrs) done() []slog.Attr {
	if len(l.cut) > 0 {
		return append(l.attrs, slog.String("truncated", strings.Join(l.cut, ",")))
	}
	return l.attrs
}

// emitRun logs one observed run state and reports whether out logged it.
func emitRun(out emitter, log *slog.Logger, run *forge.Run, previous forge.RunState, isDefault bool) bool {
	var l lineAttrs
	l.text("repo", run.Repo)
	l.add(slog.String("state", string(run.State)))
	if previous != "" {
		l.add(slog.String("previous_state", string(previous)))
	}
	l.text("workflow", run.Workflow)
	l.text("branch", run.Branch)
	l.add(slog.Bool("default_branch", isDefault), slog.String("trigger", run.Trigger),
		slog.Int64("run_id", run.ID), slog.Int64("run_number", run.Number))
	l.link(run.URL)
	l.add(slog.String("created_at", stamp(run.CreatedAt)))
	if !run.StartedAt.IsZero() {
		l.add(slog.String("started_at", stamp(run.StartedAt)))
	}
	l.add(slog.String("updated_at", stamp(run.UpdatedAt)))
	if d, ok := run.Duration(); ok {
		l.add(slog.Int64("duration_seconds", int64(d/time.Second)))
	}
	return out.log(log, slog.LevelInfo, "ci run", l.done()...)
}

// emitter logs one phase's integrity and product lines until done closes,
// and refuses every line once it has, so no line follows a cut.
type emitter struct {
	done <-chan struct{}
}

// log logs one line and reports whether it did.
func (e emitter) log(l *slog.Logger, level slog.Level, msg string, attrs ...slog.Attr) bool {
	if e.interrupted() {
		return false
	}
	l.LogAttrs(context.Background(), level, msg, attrs...)
	return true
}

// interrupted reports whether the emitter refuses lines.
func (e emitter) interrupted() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// listed counts each connection's lines of one capped family.
func listed[T any](rows []ranked[T]) map[*connResult]int {
	out := map[*connResult]int{}
	for _, row := range rows {
		out[row.r]++
	}
	return out
}

// burst is the counts of capped lines each connection got.
type burst struct {
	failing, slow, disabled, topAlerts map[*connResult]int
}

// emitBurst emits every snapshot line of the scan, then each connection's
// verdict lines, all through the scan's emitter, so a shutdown at any line
// leaves no line after it and no scan complete committing a cut burst.
func (c *Collector) emitBurst(sc *scan, results []*connResult) {
	now := sc.start
	prs := rank(results, func(r *connResult) []prRow { return r.prs }, lessPR, 0)
	issues := rank(results, func(r *connResult) []issueRow { return r.issues }, lessIssue, 0)
	failing := rank(results, func(r *connResult) []failingRow { return r.failing }, lessFailing, maxFailingWorkflows)
	slow := rank(results, func(r *connResult) []slowRow { return r.slow }, lessSlow, maxSlowWorkflows)
	disabled := rank(results, func(r *connResult) []forge.Workflow { return r.disabled }, lessDisabled, maxDisabledWorkflows)
	for _, r := range results {
		r.days = c.listedDays(r)
	}
	days := rank(results, func(r *connResult) []runstate.DayCount { return r.days }, lessDay, 0)
	alerts := rank(results, func(r *connResult) []forge.Alert { return r.alerts }, lessAlert, 0)
	var top []ranked[forge.Alert]
	for _, row := range alerts {
		if row.within(maxTopAlerts) {
			top = append(top, row)
		}
	}
	for _, row := range prs {
		emitPR(sc, now, row)
	}
	for _, row := range issues {
		emitIssue(sc, now, row)
	}
	for _, row := range failing {
		emitFailing(sc, row)
	}
	for _, row := range slow {
		emitSlow(sc, row)
	}
	for _, row := range disabled {
		emitDisabled(sc, row)
	}
	emitDays(sc, days)
	c.emitSecurity(sc, alerts, top, tools(results))
	b := burst{failing: listed(failing), slow: listed(slow), disabled: listed(disabled), topAlerts: listed(top)}
	for _, r := range results {
		c.emitVerdict(sc, r, &b)
	}
}

func snapshotAttrs[T any](sc *scan, row ranked[T]) *lineAttrs {
	return &lineAttrs{attrs: []slog.Attr{slog.Int64("scan_id", sc.id), slog.Int("rank", row.rank), slog.Int("forge_rank", row.forgeRank)}}
}

func emitPR(sc *scan, now time.Time, row ranked[prRow]) {
	pr := &row.row.pr
	l := snapshotAttrs(sc, row)
	l.text("repo", pr.Repo)
	l.add(slog.Int("number", pr.Number))
	l.text("ref", pr.Ref)
	l.text("title", pr.Title)
	l.text("author", pr.Author)
	l.add(slog.String("from", from(row.r, pr.Author)), slog.Bool("draft", pr.Draft))
	l.text("labels", strings.Join(pr.Labels, ","))
	if row.row.checks != "" {
		l.add(slog.String("checks", string(row.row.checks)))
	}
	l.link(pr.URL)
	l.add(itemTimeAttrs(now, pr.CreatedAt, pr.UpdatedAt)...)
	sc.out.log(row.r.log, slog.LevelInfo, "open pull request", l.done()...)
}

func emitIssue(sc *scan, now time.Time, row ranked[issueRow]) {
	is := &row.row.issue
	l := snapshotAttrs(sc, row)
	l.text("repo", is.Repo)
	l.add(slog.Int("number", is.Number))
	l.text("ref", is.Ref)
	l.text("title", is.Title)
	l.text("author", is.Author)
	l.add(slog.String("from", from(row.r, is.Author)))
	l.text("labels", strings.Join(is.Labels, ","))
	l.link(is.URL)
	l.add(itemTimeAttrs(now, is.CreatedAt, is.UpdatedAt)...)
	sc.out.log(row.r.log, slog.LevelInfo, "open issue", l.done()...)
}

// itemTimeAttrs is an open item's times and the ages derived from them,
// each left out where the forge reported no time, so an unknown age never
// reads as new work.
func itemTimeAttrs(now, created, updated time.Time) []slog.Attr {
	var attrs []slog.Attr
	if !created.IsZero() {
		attrs = append(attrs, slog.String("created_at", stamp(created)), slog.Int("age_days", days(now, created)))
	}
	if !updated.IsZero() {
		attrs = append(attrs, slog.String("updated_at", stamp(updated)), slog.Int("idle_days", days(now, updated)))
	}
	return attrs
}

func emitFailing(sc *scan, row ranked[failingRow]) {
	f := &row.row
	state := string(forge.RunFailing)
	if f.unknown {
		state = "unknown"
	}
	l := snapshotAttrs(sc, row)
	l.text("repo", f.repo)
	l.text("workflow", f.name)
	l.text("branch", f.branch)
	l.add(slog.String("state", state), slog.Int64("run_id", f.runID))
	l.link(f.url)
	l.add(slog.String("last_run_at", stamp(f.last)), slog.String("failing_since", stamp(f.since)),
		slog.Bool("failing_since_clipped", f.clipped), slog.Int("consecutive_failures", f.streak))
	sc.out.log(row.r.log, slog.LevelInfo, "failing workflow", l.done()...)
}

func emitSlow(sc *scan, row ranked[slowRow]) {
	s := &row.row
	l := snapshotAttrs(sc, row)
	l.text("repo", s.repo)
	l.text("workflow", s.name)
	l.add(slog.Int("runs", s.runs),
		slog.Int64("p50_seconds", int64(s.p50/time.Second)), slog.Int64("p95_seconds", int64(s.p95/time.Second)))
	l.link(s.url)
	sc.out.log(row.r.log, slog.LevelInfo, "slow workflow", l.done()...)
}

// listedDays is the stored day counts of each repository r listed this scan:
// a repository whose listing failed keeps its last logged counts.
func (c *Collector) listedDays(r *connResult) []runstate.DayCount {
	listed := make(map[string]bool, len(r.runLists))
	for i := range r.runLists {
		listed[r.runLists[i].repo.Path] = true
	}
	all := c.store.Days(r.cfg.Name)
	return slices.DeleteFunc(all, func(d runstate.DayCount) bool { return !listed[d.Repo] })
}

// emitDays logs one ci day line per repository and retained day, which the
// day charts read in place of the ci run lines.
func emitDays(sc *scan, days []ranked[runstate.DayCount]) {
	for _, row := range days {
		d := &row.row
		l := snapshotAttrs(sc, row)
		l.text("repo", d.Repo)
		l.add(slog.String("day", d.Day),
			slog.Int("passing", d.Passing), slog.Int("failing", d.Failing), slog.Int("neutral", d.Neutral),
			slog.Int("timed_runs", d.Timed), slog.Int64("run_seconds", d.Seconds), slog.Bool("clipped", d.Clipped))
		sc.out.log(row.r.log, slog.LevelInfo, "ci day", l.done()...)
	}
}

func emitDisabled(sc *scan, row ranked[forge.Workflow]) {
	w := &row.row
	l := snapshotAttrs(sc, row)
	l.text("repo", w.Repo)
	l.text("workflow", w.Name)
	l.text("path", w.Path)
	l.add(slog.String("state", w.State))
	l.link(w.URL)
	sc.out.log(row.r.log, slog.LevelInfo, "disabled workflow", l.done()...)
}

func alertAttrs(l *lineAttrs, a *forge.Alert) []slog.Attr {
	l.text("repo", a.Repo)
	l.add(slog.String("source", a.Source), slog.Int64("number", a.Number))
	l.text("rule", a.Rule)
	l.text("severity", a.Severity)
	l.text("tool", a.Tool)
	l.link(a.URL)
	if !a.CreatedAt.IsZero() {
		l.add(slog.String("created_at", stamp(a.CreatedAt)))
	}
	return l.done()
}

// emitSecurity emits every alert, the top alerts of the same ranking and the
// scan's per-tool counts, which belong to no single connection.
func (c *Collector) emitSecurity(sc *scan, alerts, top []ranked[forge.Alert], counts []toolCount) {
	for _, row := range alerts {
		sc.out.log(row.r.log, slog.LevelInfo, "security alert", alertAttrs(snapshotAttrs(sc, row), &row.row)...)
	}
	for _, row := range top {
		sc.out.log(row.r.log, slog.LevelInfo, "top security alert", alertAttrs(snapshotAttrs(sc, row), &row.row)...)
	}
	log := Scope(c.logger, forge.ProductGitHub, "")
	for i, t := range counts {
		l := lineAttrs{attrs: []slog.Attr{slog.Int64("scan_id", sc.id), slog.Int("rank", i+1), slog.Int("forge_rank", i+1)}}
		if !t.rollup {
			l.text("tool", t.tool)
		}
		l.add(slog.Int("alerts", t.alerts), slog.Bool("rollup", t.rollup))
		sc.out.log(log, slog.LevelInfo, "security tool", l.done()...)
	}
}

// emitVerdict emits a connection's scan stopped or scan complete, with a scan
// degraded line when the scan escalates.
func (c *Collector) emitVerdict(sc *scan, r *connResult, b *burst) {
	l := &r.ledger
	if l.stop != nil {
		escalated := l.stop.reason != stopReadReserve || l.escalate()
		c.emitStopped(sc, r, escalated)
		if escalated {
			c.emitDegraded(sc, r)
		}
		return
	}
	if l.escalate() {
		c.emitDegraded(sc, r)
	}
	sc.out.log(r.log, slog.LevelInfo, "scan complete", append(completeAttrs(sc, r, b), c.intervalAttr())...)
}

// intervalAttr is the configured gap between scans, against which a reader
// judges how old the newest scan line may grow.
func (c *Collector) intervalAttr() slog.Attr {
	return slog.Int64("scan_interval_s", int64(c.interval/time.Second))
}

func (*Collector) emitDegraded(sc *scan, r *connResult) {
	cause, reason := r.ledger.diagnosis()
	sc.out.log(r.log, slog.LevelError, "scan degraded", slog.Int64("scan_id", sc.id),
		slog.String("cause", cause), slog.String("reason", reason),
		slog.String("failed_signals", r.ledger.failedSignals()), slog.Int("errors", r.ledger.errCount()))
}

// emitStopped logs a stopped connection, with the read state of every
// family. The first budget stop logged in a budget window is WARN and the
// repeats until it renews INFO, the level of scan complete, so each stays
// the stall alert's heartbeat. A budget stop with no window known, and a
// stop at the limit, are always WARN. A stop is degraded when it escalates
// or a read before it degraded; a clean budget stop works as intended.
func (c *Collector) emitStopped(sc *scan, r *connResult, escalated bool) {
	stop := r.ledger.stop
	level := slog.LevelWarn
	budgetStop := stop.reason != stopScanTimeout
	last, ok := c.lastStops[r.cfg.Name]
	if budgetStop && ok && !r.budget.Reset.IsZero() && last.Equal(r.budget.Reset) {
		level = slog.LevelInfo
	}
	attrs := make([]slog.Attr, 0, 10+numFamilies)
	attrs = append(attrs, slog.Int64("scan_id", sc.id), slog.String("reason", stop.reason), slog.String("phase", stop.phase))
	if stop.reason == stopScanTimeout {
		attrs = append(attrs, slog.String("limit", c.limit.String()), slog.String("hint", timeoutHint))
	}
	attrs = append(attrs, r.ledger.readAttrs()...)
	attrs = append(attrs, slog.Bool("degraded", escalated || r.ledger.degraded()), slog.String("failed_signals", r.ledger.failedSignals()))
	attrs = append(attrs, budgetAttrs(r.budget)...)
	attrs = append(attrs, c.intervalAttr())
	if sc.out.log(r.log, level, "scan stopped", attrs...) && budgetStop {
		c.lastStops[r.cfg.Name] = r.budget.Reset
	}
}

func budgetAttrs(b forge.Budget) []slog.Attr {
	var attrs []slog.Attr
	if b.Known() {
		attrs = append(attrs, slog.Int("budget_remaining", b.Remaining))
	}
	if !b.Reset.IsZero() {
		attrs = append(attrs, slog.String("budget_reset", stamp(b.Reset)))
	}
	return attrs
}
