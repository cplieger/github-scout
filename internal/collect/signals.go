package collect

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/github-scout/internal/runstate"
)

// Caps on the ranked lines whose full set is not otherwise emitted. The row
// caps are each paired with a *_listed count on scan complete; the tools past
// maxTools fold into one rollup line.
const (
	maxFailingWorkflows  = 25
	maxSlowWorkflows     = 10
	maxDisabledWorkflows = 25
	maxTopAlerts         = 25
	maxTools             = 10
	// minDurationRuns is the fewest timed runs a workflow needs for a p95.
	minDurationRuns = 3
)

// The from field's values.
const (
	fromYou    = "you"
	fromBot    = "bot"
	fromOthers = "someone_else"
)

type prRow struct {
	// check is the head checks read, nil when it failed or the row names no
	// head; checks is the field approve settles from it, empty where the
	// product's rows name no head.
	check  *forge.CheckResult
	checks forge.CheckState
	pr     forge.PullRequest
}

type issueRow struct {
	issue forge.Issue
}

type failingRow struct {
	since  time.Time
	last   time.Time
	repo   string
	name   string
	branch string
	url    string
	runID  int64
	streak int
	// clipped reports a streak that may have begun before since: its start
	// reaches back past what any listing covered.
	clipped bool
	// unknown reports a verdict a cut listing does not vouch for (see
	// runstate.Vouched): the row is its last judged verdict.
	unknown bool
}

type slowRow struct {
	repo     string
	name     string
	url      string
	runs     int
	p50, p95 time.Duration
}

// derive computes the connection's state-derived signals over the
// repositories whose runs this scan listed: the store alone cannot say what
// is failing now in a repository it could not list. A verdict of a workflow
// a whole GitHub workflow listing proves gone is failing nowhere.
func (*Collector) derive(sc *scan, r *connResult) {
	runs := &r.ledger.fam[famRuns]
	if !runs.begun || runs.unsupported {
		return
	}
	r.sum.delivered = sc.state.Delivered(r.cfg.Name)
	if runs.blind() {
		r.ledger.drop(famFailing, dropSourceBlind, 1)
		return
	}
	r.ledger.begin(famFailing)
	defer r.ledger.finish(famFailing)
	// Read through a cut repository listing, the rows would be a prefix.
	if r.ledger.withheld(famFailing) {
		return
	}
	listed := make(map[string]*runListing, len(r.runLists))
	for i := range r.runLists {
		listed[strings.ToLower(r.runLists[i].repo.Path)] = &r.runLists[i]
	}
	for k, w := range sc.state.Workflows(r.cfg.Name) {
		key := strings.ToLower(k.Repo)
		l, ok := listed[key]
		if !ok || w.State != forge.RunFailing || k.Branch != l.repo.DefaultBranch {
			continue
		}
		if d, defined := r.defined[key]; defined && d.gone(k.Name, w.Defined) {
			continue
		}
		r.failing = append(r.failing, failingRow{
			since: w.Since, last: w.Last, repo: k.Repo, name: k.Name, branch: k.Branch, url: w.URL,
			runID: w.RunID, streak: w.Streak, clipped: w.Clipped, unknown: !runstate.Vouched(l.cut, l.cover, w.Last),
		})
	}
	r.slow, r.sum.runDurationsKnown = slowWorkflows(r)
}

// onDefault reports a run of repo's current default branch itself: a run a
// pull request started tests the proposal, whatever branch name it reports.
func onDefault(repo forge.Repo, branch, trigger string) bool {
	return branch == repo.DefaultBranch && trigger != forge.TriggerPullRequest
}

type timed struct {
	created time.Time
	url     string
	id      int64
	d       time.Duration
}

// slowWorkflows is each workflow's run time percentiles over its timed
// runs this scan listed whole on the repository's current default branch,
// for workflows with enough of them: a cut listing's newest rows are no
// sample of the window.
func slowWorkflows(r *connResult) (rows []slowRow, known int) {
	byWorkflow := map[[2]string][]timed{}
	for i := range r.runLists {
		l := &r.runLists[i]
		if l.cut {
			continue
		}
		for j := range l.runs {
			run := &l.runs[j]
			d, ok := run.Duration()
			if !ok || !onDefault(l.repo, run.Branch, run.Trigger) {
				continue
			}
			known++
			key := [2]string{run.Repo, run.Workflow}
			byWorkflow[key] = append(byWorkflow[key], timed{created: run.CreatedAt, url: run.URL, id: run.ID, d: d})
		}
	}
	for key, runs := range byWorkflow {
		if len(runs) < minDurationRuns {
			continue
		}
		newest := slices.MaxFunc(runs, func(a, b timed) int { return cmp.Or(a.created.Compare(b.created), cmp.Compare(a.id, b.id)) })
		ds := make([]time.Duration, len(runs))
		for i, t := range runs {
			ds[i] = t.d
		}
		slices.Sort(ds)
		rows = append(rows, slowRow{
			repo: key[0], name: key[1], url: newest.url, runs: len(ds),
			p50: nearestRank(ds, 50), p95: nearestRank(ds, 95),
		})
	}
	return rows, known
}

// nearestRank is the p-th percentile of sorted by the nearest-rank method.
func nearestRank(sorted []time.Duration, p int) time.Duration {
	rank := (p*len(sorted) + 99) / 100
	return sorted[max(rank, 1)-1]
}

// from classifies an author against the connection's own account.
func from(r *connResult, author string) string {
	switch {
	case author != "" && strings.EqualFold(author, r.login):
		return fromYou
	case strings.HasSuffix(strings.ToLower(author), "[bot]") || r.cfg.BotAuthors[strings.ToLower(author)]:
		return fromBot
	default:
		return fromOthers
	}
}

// days is the whole days from t to now, 0 for a t after now.
func days(now, t time.Time) int {
	if now.Before(t) {
		return 0
	}
	return int(now.Sub(t) / (24 * time.Hour))
}

// ageBuckets counts open work by age, plus how much has been idle 30 days.
// An item with no creation or update time counts as unknown there, never in
// a bucket, so the buckets and the unknowns add up to the open items.
type ageBuckets struct {
	lt7, d7to30, d30to90, d90plus, ageUnknown int
	idle30, idleUnknown                       int
}

func (b *ageBuckets) add(now, created, updated time.Time) {
	switch age := days(now, created); {
	case created.IsZero():
		b.ageUnknown++
	case age < 7:
		b.lt7++
	case age < 30:
		b.d7to30++
	case age < 90:
		b.d30to90++
	default:
		b.d90plus++
	}
	switch {
	case updated.IsZero():
		b.idleUnknown++
	case days(now, updated) >= 30:
		b.idle30++
	}
}

// severityRank orders code-scanning severities, most severe first.
func severityRank(s string) int {
	switch strings.ToLower(s) {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	default:
		return 4
	}
}

// severityBucket is the scan complete field suffix for a severity.
func severityBucket(s string) string {
	return [...]string{"critical", "high", "medium", "low", "unrated"}[severityRank(s)]
}

// ranked is one row of a snapshot family with its connection.
type ranked[T any] struct {
	r         *connResult
	row       T
	rank      int
	forgeRank int
}

// rank orders every connection's rows of one family, assigns rank from 1
// across the scan and forge_rank from 1 within each product, and keeps each
// row whose rank or forge_rank is within limit (all when limit is 0), so a
// table filtered to one forge still holds that forge's first limit rows.
func rank[T any](results []*connResult, rows func(*connResult) []T, less func(a, b *T) int, limit int) []ranked[T] {
	var all []ranked[T]
	for _, r := range results {
		if r.ledger.stop != nil {
			continue
		}
		for _, row := range rows(r) {
			all = append(all, ranked[T]{r: r, row: row})
		}
	}
	slices.SortStableFunc(all, func(a, b ranked[T]) int {
		return cmp.Or(less(&a.row, &b.row), cmp.Compare(a.r.cfg.Name, b.r.cfg.Name))
	})
	perForge := map[forge.Product]int{}
	out := all[:0]
	for i := range all {
		row := all[i]
		row.rank = i + 1
		perForge[row.r.product]++
		row.forgeRank = perForge[row.r.product]
		if limit == 0 || row.within(limit) {
			out = append(out, row)
		}
	}
	return out
}

// within reports a row inside a cap of limit by rank or by forge_rank.
func (row *ranked[T]) within(limit int) bool { return row.rank <= limit || row.forgeRank <= limit }

func byIdle(a, b time.Time, ra, rb string, na, nb int) int {
	return cmp.Or(oldestKnownFirst(a, b), cmp.Compare(ra, rb), cmp.Compare(na, nb))
}

// oldestKnownFirst orders times oldest first and an unknown (zero) time
// after every known one: an item the forge gave no time is not the oldest.
func oldestKnownFirst(a, b time.Time) int {
	return cmp.Or(cmp.Compare(zeroRank(a), zeroRank(b)), a.Compare(b))
}

func zeroRank(t time.Time) int {
	if t.IsZero() {
		return 1
	}
	return 0
}

func lessPR(a, b *prRow) int {
	return byIdle(a.pr.UpdatedAt, b.pr.UpdatedAt, a.pr.Repo, b.pr.Repo, a.pr.Number, b.pr.Number)
}

func lessIssue(a, b *issueRow) int {
	return byIdle(a.issue.UpdatedAt, b.issue.UpdatedAt, a.issue.Repo, b.issue.Repo, a.issue.Number, b.issue.Number)
}

func lessFailing(a, b *failingRow) int {
	return cmp.Or(a.since.Compare(b.since), cmp.Compare(a.repo, b.repo), cmp.Compare(a.name, b.name))
}

func lessSlow(a, b *slowRow) int {
	return cmp.Or(cmp.Compare(b.p95, a.p95), cmp.Compare(a.repo, b.repo), cmp.Compare(a.name, b.name))
}

// lessDay orders day rows newest day first, then by repository.
func lessDay(a, b *runstate.DayCount) int {
	return cmp.Or(cmp.Compare(b.Day, a.Day), cmp.Compare(a.Repo, b.Repo))
}

func lessDisabled(a, b *forge.Workflow) int {
	inactivity := func(w *forge.Workflow) int {
		if w.State == "disabled_inactivity" {
			return 0
		}
		return 1
	}
	return cmp.Or(cmp.Compare(inactivity(a), inactivity(b)), cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Name, b.Name))
}

func lessAlert(a, b *forge.Alert) int {
	return cmp.Or(cmp.Compare(severityRank(a.Severity), severityRank(b.Severity)), oldestKnownFirst(a.CreatedAt, b.CreatedAt),
		cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Number, b.Number))
}

func lessTool(a, b *toolCount) int {
	return cmp.Or(cmp.Compare(b.alerts, a.alerts), cmp.Compare(a.tool, b.tool))
}

// toolCount is one tool's alerts across the scan; rollup marks the one row
// folding every tool past the cap, which names no tool.
type toolCount struct {
	tool   string
	alerts int
	rollup bool
}

// tools is the scan's alert count per tool over every connection that
// publishes its alerts: the ten with the most, then one rollup row.
func tools(results []*connResult) []toolCount {
	counts := map[string]int{}
	for _, r := range results {
		if r.ledger.stop != nil {
			continue
		}
		for i := range r.alerts {
			counts[r.alerts[i].Tool]++
		}
	}
	out := make([]toolCount, 0, len(counts))
	for t, n := range counts {
		out = append(out, toolCount{tool: t, alerts: n})
	}
	slices.SortFunc(out, func(a, b toolCount) int { return lessTool(&a, &b) })
	if len(out) <= maxTools {
		return out
	}
	rest := toolCount{rollup: true}
	for _, t := range out[maxTools:] {
		rest.alerts += t.alerts
	}
	return append(out[:maxTools], rest)
}
