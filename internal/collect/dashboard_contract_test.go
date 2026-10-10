package collect

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/cplieger/github-scout/internal/forge"
)

// The bundled grafana-dashboard.json reads the log lines this package writes,
// so every message and attribute a panel reads must be one a scan logs, and
// every value a panel maps must be one the code can produce.

const dashboardPath = "../../grafana-dashboard.json"

var (
	// pipelineRe captures one log pipeline: a stream selector, its stages,
	// and the range that closes it. No stage the dashboard uses holds a '['.
	pipelineRe   = regexp.MustCompile(`\{[^{}]*\}((?:[^\[{]|\{\{[^}]*\}\})*)\[([^\]]+)\]`)
	logQueryRe   = regexp.MustCompile(`^\{[^{}]*\}((?:[^\[{]|\{\{[^}]*\}\})*)$`)
	jsonStageRe  = regexp.MustCompile(`\|\s*json\b([^|]*)`)
	jsonParamRe  = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*=\s*"([^"]*)"`)
	msgEqRe      = regexp.MustCompile("(?:\\||\\bor)\\s*msg\\s*=\\s*`([^`]*)`")
	msgReRe      = regexp.MustCompile(`\bmsg\s*=~`)
	msgOtherRe   = regexp.MustCompile(`(?:\||\bor)\s*msg\s*(!=|!~|=\s*")`)
	lineFilterRe = regexp.MustCompile("\\|=\\s*`([^`]*)`")
	regexLineRe  = regexp.MustCompile("(^|[\\s}])(\\|~|!~|!=)\\s*`")
	overTimeRe   = regexp.MustCompile(`\b([a-z_]+)_over_time\(`)
	byRe         = regexp.MustCompile(`\bby\s*\(([^)]*)\)`)
	selectorRe   = regexp.MustCompile(`\{([^{}]*)\}`)
	backtickedRe = regexp.MustCompile("`[^`]*`")
	// timeMacroRe is Grafana's range boundary in a custom date format, which
	// Grafana replaces with a number before Loki reads the query.
	timeMacroRe = regexp.MustCompile(`\$\{__(from|to):date:[A-Za-z]+\}`)
)

// scanEnds selects the two lines that close a connection's scan.
const scanEnds = "msg=`scan complete` or msg=`scan stopped`"

// handlerKeys are the keys the JSON handler writes on every line.
var handlerKeys = []string{"time", "level", "msg"}

// allowedWindows are Grafana's range and step; a fixed window assumes a
// retention or a cadence, and hides a scan older than it.
var allowedWindows = []string{"$__range", "$__interval"}

// runKey is what makes two ci run lines the same delivery: a line is logged
// at least once, so a count of runs groups by all of it.
var runKey = []string{"connection", "repo", "run_id", "state", "updated_at"}

// identityLabels name one item, so grouping a metric by any of them makes a
// series per item.
var identityLabels = []string{"run_id", "run_number", "number", "ref", "title", "url", "rule", "path", "author", "scan_id", "created_at", "updated_at"}

// snapshotMessages are re-stated in full every scan, so a table of them
// shows one scan only by joining on the newest scan's scan_id.
var snapshotMessages = []string{
	"open pull request", "open issue", "failing workflow", "slow workflow", "disabled workflow",
	"security alert", "top security alert", "security tool",
}

// dayMessage restates, every scan, each retained day of each listed
// repository from the run state, so a day chart reads each (connection,
// repository, day) series' newest line: a re-run moves a run between results
// and a result that falls to zero reads 0.
const dayMessage = "ci day"

// dayKey is the series a day chart takes the newest line of.
var dayKey = []string{"connection", "repo", "day"}

type dashQuery struct {
	datasource any
	panel      string
	expr       string
	maxLines   float64
	instant    bool
}

type dashPanel struct {
	title      string
	viz        string
	queries    []dashQuery
	transforms []map[string]any
	el         map[string]any
}

func field(v any, keys ...string) any {
	for _, k := range keys {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}

func loadDashboard(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("Setup: parse %s: %v", dashboardPath, err)
	}
	return d
}

func dashboardPanels(d map[string]any) []dashPanel {
	elements, _ := field(d, "spec", "elements").(map[string]any)
	var out []dashPanel
	for _, name := range slices.Sorted(maps.Keys(elements)) {
		el, _ := elements[name].(map[string]any)
		p := dashPanel{el: el}
		p.title, _ = field(el, "spec", "title").(string)
		p.viz, _ = field(el, "spec", "vizConfig", "group").(string)
		queries, _ := field(el, "spec", "data", "spec", "queries").([]any)
		for _, q := range queries {
			query := field(q, "spec", "query")
			expr, _ := field(query, "spec", "expr").(string)
			maxLines, _ := field(query, "spec", "maxLines").(float64)
			p.queries = append(p.queries, dashQuery{
				panel: p.title, expr: expr, datasource: field(query, "datasource"), maxLines: maxLines,
				instant: field(query, "spec", "queryType") == "instant",
			})
		}
		transforms, _ := field(el, "spec", "data", "spec", "transformations").([]any)
		for _, tr := range transforms {
			m := map[string]any{"group": field(tr, "group")}
			opts, _ := field(tr, "spec", "options").(map[string]any)
			maps.Copy(m, opts)
			p.transforms = append(p.transforms, m)
		}
		out = append(out, p)
	}
	return out
}

// variableStreams is every query variable's log query, which the contract
// binds like a panel's.
func variableStreams(d map[string]any) []dashQuery {
	vars, _ := field(d, "spec", "variables").([]any)
	var out []dashQuery
	for _, v := range vars {
		if field(v, "kind") != "QueryVariable" {
			continue
		}
		name, _ := field(v, "spec", "name").(string)
		stream, _ := field(v, "spec", "query", "spec", "stream").(string)
		out = append(out, dashQuery{panel: "variable " + name, expr: stream, datasource: field(v, "spec", "query", "datasource"), maxLines: 1})
	}
	return out
}

// contract is the keys each message carries, recorded from scans that log
// every message the dashboard can read.
type contract map[string]map[string]bool

func (c contract) record(h *harness) {
	for _, r := range h.rec.Records() {
		keys := c[r.Message]
		if keys == nil {
			keys = map[string]bool{}
			c[r.Message] = keys
		}
		r.Attrs(func(a slog.Attr) bool {
			keys[a.Key] = true
			return true
		})
	}
}

func (c contract) allows(msg, key string) bool {
	return slices.Contains(handlerKeys, key) || c[msg][key]
}

// recordContract runs a GitHub and a Gitea connection through a scan, a
// re-run, a refused read, a budget stop and a coverage gap, and keeps what
// they logged.
func recordContract(t *testing.T) contract {
	t.Helper()
	c := contract{}
	gh, gt := githubEndpoint("gh"), giteaEndpoint("gt")
	var runs []forge.Run
	for i := range 4 {
		r := run(int64(i+1), forge.RunFailing, now.Add(-time.Duration(4-i)*time.Hour))
		r.UpdatedAt = r.StartedAt.Add(time.Duration(i+1) * time.Minute)
		runs = append(runs, r)
	}
	gh.conn.runs = map[string]listing{"o/r": {Runs: runs}}
	gh.conn.prs = map[string][]forge.PullRequest{"o": {{
		Repo: "o/r", Number: 1, Ref: "#1", Title: "t", Author: "stranger", HeadSHA: "h1", Labels: []string{"a"},
		CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-time.Hour), Draft: true,
	}}}
	gh.conn.checks = map[string]forge.CheckResult{"h1": {State: "failing"}}
	gh.conn.issues = map[string][]forge.Issue{"o": {{Repo: "o/r", Number: 2, Ref: "#2", Title: "t", Author: "me", Labels: []string{"bug"}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}}}
	gh.gh.alerts = map[string][]forge.Alert{"o/r": {
		{Repo: "o/r", Number: 1, Rule: "go/sql-injection", Severity: "critical", Tool: "CodeQL", Source: "code_scanning", URL: "https://gh.test/a/1", CreatedAt: now},
		{Repo: "o/r", Number: 2, Rule: "G104", Severity: "low", Tool: "gosec", Source: "code_scanning", URL: "https://gh.test/a/2", CreatedAt: now},
	}}
	gh.gh.workflows = map[string][]forge.Workflow{"o/r": {{Repo: "o/r", Name: "CI", State: "active"}, {Repo: "o/r", Name: "nightly", Path: ".github/workflows/n.yaml", State: "disabled_inactivity"}}}
	gt.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 3, Ref: "#3", Title: "t", Author: "me", CreatedAt: now, UpdatedAt: now}}}
	h := newHarness(t, gh, gt)
	h.scan(t)
	c.record(h)
	rerun := runs[3]
	rerun.State, rerun.UpdatedAt = forge.RunPassing, now
	gh.conn.runs["o/r"] = listing{Runs: append(runs[:3:3], rerun)}
	h.scan(t)
	c.record(h)

	refused := githubEndpoint("gh")
	refused.gh.errs = map[string]error{"alerts o/r": forge.ErrForbidden}
	h = newHarness(t, refused)
	h.scan(t)
	c.record(h)

	stopped := githubEndpoint("gh")
	stopped.conn.errs = map[string]error{"issues o": forge.ErrReadDeferred}
	h = newHarness(t, stopped)
	h.scan(t)
	c.record(h)

	gap := githubEndpoint("gh")
	gap.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunPassing, now.Add(-20*time.Hour))}, Pending: []forge.Run{run(8, forge.RunPassing, now.Add(-10*time.Hour))}}}
	h = newHarness(t, gap)
	h.scan(t)
	*h.clock = now.Add(44 * time.Hour)
	gap.conn.runs["o/r"] = listing{Runs: []forge.Run{run(9, forge.RunFailing, now.Add(-50*time.Minute))}, Truncated: true}
	h.scan(t)
	c.record(h)
	for _, msg := range slices.Concat(snapshotMessages, []string{dayMessage, "ci run", "scan complete", "scan stopped", "scan degraded", "runs coverage gap"}) {
		if len(c[msg]) == 0 {
			t.Fatalf("Setup: the recorded scans logged no %q line, so the contract cannot check it", msg)
		}
	}
	return c
}

// pipelineMessages returns the msg values one pipeline filters on. A msg
// regex is a problem: Loki 3.7.8 rewrites some into substring tests
// (lokiLabelRegex), so msg=~`scan complete|scan stopped` also selects
// trigger scan complete.
func pipelineMessages(stages string) (msgs, problems []string) {
	for _, m := range msgEqRe.FindAllStringSubmatch(stages, -1) {
		msgs = append(msgs, m[1])
	}
	if msgReRe.MatchString(stages) {
		problems = append(problems, "msg is filtered with a regex, which Loki may match as a substring; join msg=`...` equalities with or")
	}
	if msgOtherRe.MatchString(stages) {
		problems = append(problems, "msg is filtered with an operator this check cannot read; use msg=`...` or msg=`a` or msg=`b`")
	}
	if len(msgs) == 0 {
		problems = append(problems, "the pipeline filters on no msg")
	}
	return msgs, problems
}

func checkPipeline(c contract, stages, window string) []string {
	var problems []string
	if !slices.Contains(allowedWindows, window) {
		problems = append(problems, "window ["+window+"] is neither Grafana's range nor its step")
	}
	if regexLineRe.MatchString(stages) {
		problems = append(problems, "a regex or negative line filter cannot be checked; filter on msg instead")
	}
	msgs, msgProblems := pipelineMessages(stages)
	problems = append(problems, msgProblems...)
	for _, msg := range msgs {
		if len(c[msg]) == 0 {
			problems = append(problems, "msg "+msg+" is not a message forge-scout logs")
		}
	}
	for _, m := range lineFilterRe.FindAllStringSubmatch(stages, -1) {
		if !slices.ContainsFunc(msgs, func(msg string) bool { return strings.Contains(msg, m[1]) }) {
			problems = append(problems, "line filter "+m[1]+" matches none of the pipeline's messages")
		}
	}
	stagesFound := jsonStageRe.FindAllStringSubmatch(stages, -1)
	if len(stagesFound) == 0 {
		problems = append(problems, "the pipeline extracts no field")
	}
	for _, stage := range stagesFound {
		params := jsonParamRe.FindAllStringSubmatch(stage[1], -1)
		if len(params) == 0 {
			problems = append(problems, "a bare | json stage extracts every attribute; name the keys")
		}
		for _, p := range params {
			if !slices.ContainsFunc(msgs, func(msg string) bool { return c.allows(msg, p[2]) }) {
				problems = append(problems, "json key "+p[2]+" is not logged on "+strings.Join(msgs, ", "))
			}
		}
	}
	return problems
}

// checkExpr applies every expression rule: a log query is read whole, a
// metric query one range pipeline at a time.
func checkExpr(c contract, expr string) []string {
	expr = timeMacroRe.ReplaceAllString(expr, "0")
	var problems []string
	for _, m := range selectorRe.FindAllStringSubmatch(backtickedRe.ReplaceAllString(expr, "``"), -1) {
		if m[0] != `{container="forge-scout"}` {
			problems = append(problems, "stream selector "+m[0]+", want {container=\"forge-scout\"}")
		}
	}
	if m := logQueryRe.FindStringSubmatch(expr); m != nil {
		return append(problems, checkPipeline(c, m[1], "$__range")...)
	}
	pipes := pipelineRe.FindAllStringSubmatch(expr, -1)
	if len(pipes) == 0 {
		return append(problems, "no log pipeline found")
	}
	for _, p := range pipes {
		problems = append(problems, checkPipeline(c, p[1], p[2])...)
	}
	return append(problems, checkGrouping(expr)...)
}

// checkGrouping applies the series rules of a metric query: an unwrapped
// range aggregation names its grouping, no grouping names an item's
// identity unless it is the run delivery key under an outer aggregation,
// and a count of ci run lines counts each delivery once.
func checkGrouping(expr string) []string {
	var problems []string
	for _, loc := range overTimeRe.FindAllStringSubmatchIndex(expr, -1) {
		end := matchingParen(expr, loc[1]-1)
		if end < 0 {
			problems = append(problems, "an unbalanced range aggregation")
			continue
		}
		if !strings.Contains(expr[loc[1]:end], "| unwrap") {
			continue
		}
		grouped := regexp.MustCompile(`^\s*by\s*\(`).MatchString(expr[end+1:])
		if !slices.Contains([]string{"avg", "min", "max", "stddev", "stdvar", "quantile", "first", "last"}, expr[loc[2]:loc[3]]) {
			grouped = regexp.MustCompile(`sum\s+by\s*\([^)]*\)\s*\($`).MatchString(expr[:loc[0]])
		}
		if !grouped {
			problems = append(problems, "the unwrapped aggregation "+expr[loc[0]:loc[1]]+"...) names no by () grouping")
		}
	}
	var groups [][]string
	for _, m := range byRe.FindAllStringSubmatch(expr, -1) {
		var labels []string
		for l := range strings.SplitSeq(strings.ReplaceAll(m[1], "$split", ""), ",") {
			if l = strings.TrimSpace(l); l != "" {
				labels = append(labels, l)
			}
		}
		groups = append(groups, labels)
	}
	outer := slices.ContainsFunc(groups, func(g []string) bool { return !slices.ContainsFunc(g, isIdentity) })
	runCount := strings.Contains(expr, "msg=`ci run`") && regexp.MustCompile(`\b(count_over_time|rate|sum_over_time)\(`).MatchString(expr)
	keyed := false
	for _, g := range groups {
		key := !slices.ContainsFunc(runKey, func(k string) bool { return !slices.Contains(g, k) })
		keyed = keyed || key
		if !slices.ContainsFunc(g, isIdentity) {
			continue
		}
		extra := slices.ContainsFunc(g, func(l string) bool { return !slices.Contains(runKey, l) && l != "forge" && l != "msg" })
		if !runCount || !key || extra || !outer {
			problems = append(problems, "a metric grouping names an item identity: by ("+strings.Join(g, ", ")+")")
		}
	}
	if runCount && !keyed {
		problems = append(problems, "a count of ci run lines does not group by (connection, repo, run_id, state, updated_at), so a repeated line counts twice")
	}
	return problems
}

func isIdentity(l string) bool { return slices.Contains(identityLabels, l) }

func matchingParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// checkPanel applies the table rules: a log query sets its line limit, and
// a log query over a snapshot message sits beside a one-line read of the
// newest scan, joined inner on scan_id.
func checkPanel(p *dashPanel) []string {
	var problems []string
	snapshot, newest := false, false
	for _, q := range p.queries {
		if !logQueryRe.MatchString(q.expr) {
			continue
		}
		if q.maxLines <= 0 {
			problems = append(problems, "a log query with no line limit")
		}
		for _, msg := range snapshotMessages {
			snapshot = snapshot || strings.Contains(q.expr, "msg=`"+msg+"`")
		}
		newest = newest || (q.maxLines == 1 && strings.Contains(q.expr, scanEnds) && strings.Contains(q.expr, "| keep scan_id"))
	}
	problems = append(problems, checkRunRows(p)...)
	if !snapshot {
		return problems
	}
	joined := slices.ContainsFunc(p.transforms, func(tr map[string]any) bool {
		return tr["group"] == "joinByField" && tr["byField"] == "scan_id" && tr["mode"] == "inner"
	})
	if !newest || !joined {
		problems = append(problems, "a snapshot read not joined inner on scan_id with a one-line read of the newest scan")
	}
	return problems
}

// checkRunRows applies the at-least-once rule to a table of ci run lines: a
// crash can log a line twice, so the rows are sorted newest first and grouped
// by the run before anything is shown, one row per run however often it
// was logged.
func checkRunRows(p *dashPanel) []string {
	reads := slices.ContainsFunc(p.queries, func(q dashQuery) bool {
		return logQueryRe.MatchString(q.expr) && strings.Contains(q.expr, "msg=`ci run`")
	})
	if !reads {
		return nil
	}
	sorted := false
	for _, tr := range p.transforms {
		switch tr["group"] {
		case "sortBy":
			raw, _ := json.Marshal(tr["sort"])
			sorted = sorted || string(raw) == `[{"desc":true,"field":"Time"}]`
		case "groupBy":
			fields, _ := tr["fields"].(map[string]any)
			grouped := !slices.ContainsFunc([]string{"connection", "repo", "run_id"}, func(f string) bool {
				return field(fields, f, "operation") != "groupby"
			})
			if sorted && grouped {
				return nil
			}
		}
	}
	return []string{"a table of ci run lines not sorted by Time descending and then grouped by connection, repo and run_id, so a line a crash logged twice shows twice"}
}

var (
	unwrapRe = regexp.MustCompile(`\|\s*unwrap\s+([a-z0-9_]+)`)
	newestRe = regexp.MustCompile(`\[\$__range\]\) by \(connection\)`)
)

// newestPerConnection reports whether every range aggregation in expr takes
// each connection's newest line in the selected time range.
func newestPerConnection(expr string) bool {
	aggs := overTimeRe.FindAllStringSubmatch(expr, -1)
	for _, m := range aggs {
		if m[1] != "last" {
			return false
		}
	}
	return len(aggs) > 0 && len(newestRe.FindAllString(expr, -1)) == len(aggs)
}

// jsonKeys is the set of names expr's json stages extract.
func jsonKeys(expr string) map[string]bool {
	keys := map[string]bool{}
	for _, stage := range jsonStageRe.FindAllStringSubmatch(expr, -1) {
		for _, p := range jsonParamRe.FindAllStringSubmatch(stage[1], -1) {
			keys[p[1]] = true
		}
	}
	return keys
}

// checkTile applies the current-state rule: a stat over the scan summary
// lines reads each connection's newest scan complete or scan stopped line in
// the selected time range, and turns each count it unwraps into what the
// tile adds up, as checkTileFormat states.
func checkTile(expr string) []string {
	if !strings.Contains(expr, "scan complete") || !strings.Contains(expr, "| unwrap") {
		return nil
	}
	var problems []string
	if !strings.Contains(expr, scanEnds) {
		problems = append(problems, "a tile that skips scan stopped shows the scan before a stop as the newest")
	}
	if !newestPerConnection(expr) {
		problems = append(problems, "a tile not read with last_over_time(... [$__range]) by (connection) shows an older scan than each connection's newest in the selected range, or none")
	}
	logged := jsonKeys(expr)
	for _, m := range unwrapRe.FindAllStringSubmatch(expr, -1) {
		if !logged[m[1]] {
			continue
		}
		lf := regexp.MustCompile(`\|\s*label_format\s+` + m[1] + "=`([^`]*)`").FindStringSubmatch(expr)
		if lf == nil {
			problems = append(problems, "the tile unwraps "+m[1]+" with no NaN for a withheld count, so it shows an older scan's value")
			continue
		}
		problems = append(problems, checkTileFormat(m[1], lf[1])...)
	}
	return problems
}

// checkTileFormat runs a tile's label_format of the count f, a Go
// text/template as Loki runs it, over a scan line in each read state. The
// count shows only when every read of it is complete; a line with no count,
// as a stopped scan logs, or with a read in any other state reads NaN; and a
// read the forge lacks or the config turns off yields nothing to unwrap, so
// that connection adds nothing.
func checkTileFormat(f, format string) []string {
	tmpl, err := template.New(f).Option("missingkey=zero").Parse(format)
	if err != nil {
		return []string{"the tile's label_format of " + f + " does not parse: " + err.Error()}
	}
	run := func(line map[string]string) string {
		var b strings.Builder
		if tmpl.Execute(&b, line) != nil {
			return "an error"
		}
		return b.String()
	}
	reads := countReads(f)
	if reads == nil {
		return []string{"the tile unwraps " + f + ", whose read states countReads does not name"}
	}
	whole := func(count string) map[string]string {
		line := map[string]string{}
		if count != "" {
			line[f] = count
		}
		for _, r := range reads {
			line[r] = "complete"
		}
		return line
	}
	var problems []string
	if got := run(whole("3")); got != "3" {
		problems = append(problems, "the tile shows "+got+" for a whole count of 3")
	}
	if got := run(whole("")); got != "NaN" {
		problems = append(problems, "the tile shows "+got+" for a line with no "+f+", so a stopped scan lends it an older scan's value")
	}
	for _, r := range reads {
		for state, want := range map[string]string{"partial": "NaN", "blind": "NaN", "unread": "NaN", "unsupported": "", "excluded": ""} {
			line := whole("3")
			line[r] = state
			if want == "" {
				delete(line, f)
			}
			if got := run(line); got != want {
				problems = append(problems, "the tile shows "+cmp.Or(got, "nothing")+" for "+f+" when "+r+" is "+state+", want "+cmp.Or(want, "nothing"))
			}
		}
	}
	slices.Sort(problems)
	return problems
}

// countReads names the read states a scan summary count is read through, as
// completeAttrs logs it: the count is whole only when every one is complete.
// The age and idle counts add up open pull requests and issues.
func countReads(f string) []string {
	switch {
	case strings.HasPrefix(f, "open_age_") || strings.HasPrefix(f, "idle_"):
		return []string{"open_prs_read", "open_issues_read"}
	case f == "failing_checks_prs" || f == "checks_unread" || f == "checks_unsupported_for_token":
		return []string{"pr_checks_read"}
	case strings.HasSuffix(f, "_prs"):
		return []string{"open_prs_read"}
	case strings.HasSuffix(f, "_issues"):
		return []string{"open_issues_read"}
	case strings.HasPrefix(f, "security_"):
		return []string{"security_alerts_read"}
	case strings.HasPrefix(f, "failing_workflows"):
		return []string{"failing_workflows_read"}
	case strings.HasPrefix(f, "disabled_workflows"):
		return []string{"disabled_workflows_read"}
	}
	return nil
}

// checkTilePanel adds what the stat must do with that NaN: Grafana 13.2.3's
// lastNotNull skips it, so the reduction is last, and a special mapping names
// it.
func checkTilePanel(p *dashPanel) (problems []string, tile bool) {
	for _, q := range p.queries {
		problems = append(problems, checkTile(q.expr)...)
		tile = tile || strings.Contains(q.expr, "NaN")
	}
	if !tile {
		return problems, len(problems) > 0
	}
	if calcs := field(p.el, "spec", "vizConfig", "spec", "options", "reduceOptions", "calcs"); !slices.Equal(anyStrings(calcs), []string{"last"}) {
		problems = append(problems, "a count tile reduces with "+strings.Join(anyStrings(calcs), ",")+", want last, which keeps NaN")
	}
	raw, _ := json.Marshal(field(p.el, "spec", "vizConfig", "spec", "fieldConfig", "defaults", "mappings"))
	if !strings.Contains(string(raw), `"match":"nan"`) {
		problems = append(problems, "a count tile maps no NaN, so a withheld count renders as NaN")
	}
	return problems, true
}

func anyStrings(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, _ := it.(string)
		out = append(out, s)
	}
	return out
}

func TestDashboard_count_tiles_read_the_newest_scan(t *testing.T) {
	tiles := 0
	for _, p := range dashboardPanels(loadDashboard(t)) {
		if p.viz != "stat" {
			continue
		}
		problems, tile := checkTilePanel(&p)
		if tile {
			tiles++
		}
		for _, problem := range problems {
			t.Errorf("panel %q: %s", p.title, problem)
		}
	}
	if tiles == 0 {
		t.Fatalf("%s carries no count tile over scan complete", dashboardPath)
	}
}

func TestDashboard_count_tiles_read_a_stopped_scan_as_no_whole_count(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.conn.errs = map[string]error{"runs o/r": forge.ErrReadDeferred}
	h := newHarness(t, ep)
	h.scan(t)
	stopped := lineFor(t, h.rec, "scan stopped", "gh")
	if stopped["open_prs_read"] != "complete" || stopped["degraded"] != "false" {
		t.Fatalf("Setup: scan stopped = %v, want a clean stop after a whole pull request read", stopped)
	}
	text := nanText(t)
	shown := 0
	for _, p := range dashboardPanels(loadDashboard(t)) {
		if p.viz != "stat" {
			continue
		}
		for _, q := range p.queries {
			for _, m := range tileFormatRe.FindAllStringSubmatch(q.expr, -1) {
				if m[1] != "open_prs" {
					continue
				}
				shown++
				if got := formatLabels(t, m[2], []map[string]string{stopped})[0]; got != "NaN" {
					t.Errorf("panel %q shows %q from a stopped scan whose pull request read was whole, want NaN, which reads %q", p.title, got, text)
				}
			}
		}
		if desc, _ := field(p.el, "spec", "description").(string); strings.Contains(desc, text) && !strings.Contains(desc, "stopped") {
			t.Errorf("panel %q description says when it reads %q but not that a stopped scan does too: %q", p.title, text, desc)
		}
	}
	if shown == 0 {
		t.Fatalf("Setup: %s has no tile of open_prs", dashboardPath)
	}
	for _, lead := range []string{"The count tiles add up", "- `ForgeScoutScanDegraded` means"} {
		if line := docSection(t, readMonitoringDoc(t), lead)[0]; !strings.Contains(line, `"`+text+`"`) {
			t.Errorf("%s paragraph %q does not name the tile text %q", monitoringDoc, lead, text)
		}
	}
	if line := docSection(t, readMonitoringDoc(t), "The count tiles add up")[0]; !strings.Contains(line, "`scan stopped` line logs no counts") {
		t.Errorf("%s tile paragraph does not say a `scan stopped` line logs no counts, so a tile reads %q after a clean stop", monitoringDoc, text)
	}
}

var tileFormatRe = regexp.MustCompile("\\|\\s*label_format\\s+([a-z0-9_]+)=`([^`]*)`")

func TestDashboard_reads_only_what_a_scan_logs(t *testing.T) {
	c := recordContract(t)
	d := loadDashboard(t)
	n := 0
	for _, p := range dashboardPanels(d) {
		for _, q := range p.queries {
			n++
			for _, problem := range checkExpr(c, q.expr) {
				t.Errorf("panel %q: %s\nexpr: %s", p.title, problem, q.expr)
			}
		}
	}
	for _, q := range variableStreams(d) {
		for _, problem := range checkExpr(c, q.expr) {
			t.Errorf("%s: %s\nexpr: %s", q.panel, problem, q.expr)
		}
	}
	if n == 0 {
		t.Fatalf("%s carries no query", dashboardPath)
	}
}

func TestDashboard_item_tables_are_snapshots_or_bounded_event_reads(t *testing.T) {
	for _, p := range dashboardPanels(loadDashboard(t)) {
		for _, problem := range checkPanel(&p) {
			t.Errorf("panel %q: %s", p.title, problem)
		}
	}
}

// extremeReducers are the scan summary fields whose chart shows the extreme
// of each interval, as its title says. Every other summary field is a
// snapshot, so an interval shows each connection's newest scan in it.
var extremeReducers = map[string]string{"duration_ms": "max", "budget_remaining": "min"}

// checkSnapshotTrend applies the snapshot rule to a read of the scan summary
// lines: a maximum keeps an item an earlier scan in the interval still saw,
// and stacked per-band maxima can draw a total no scan logged.
func checkSnapshotTrend(expr string) []string {
	if !strings.Contains(expr, "scan complete") {
		return nil
	}
	var problems []string
	aggs := overTimeRe.FindAllStringSubmatchIndex(expr, -1)
	for i, m := range aggs {
		end := len(expr)
		if i+1 < len(aggs) {
			end = aggs[i+1][0]
		}
		unwrapped := unwrapRe.FindStringSubmatch(expr[m[1]:end])
		if unwrapped == nil {
			continue
		}
		if fn := expr[m[2]:m[3]]; fn != "last" && extremeReducers[unwrapped[1]] != fn {
			problems = append(problems, "reads "+unwrapped[1]+" with "+fn+"_over_time, want last_over_time, each connection's newest scan in the interval")
		}
	}
	return problems
}

func TestDashboard_snapshot_trends_read_each_connections_newest_scan(t *testing.T) {
	trends := 0
	for _, p := range dashboardPanels(loadDashboard(t)) {
		for _, q := range p.queries {
			if !q.instant && strings.Contains(q.expr, "scan complete") && strings.Contains(q.expr, "| unwrap") {
				trends++
			}
			for _, problem := range checkSnapshotTrend(q.expr) {
				t.Errorf("panel %q: %s", p.title, problem)
			}
		}
	}
	if trends == 0 {
		t.Fatalf("%s carries no chart over the scan summary lines", dashboardPath)
	}
	read := func(fn, key string) string {
		return "sum by (msg) (" + fn + "_over_time({container=\"forge-scout\"} |= `scan complete` | json msg=\"msg\", connection=\"connection\", " +
			key + "=\"" + key + "\" | msg=`scan complete` | unwrap " + key + " | __error__=\"\" [$__interval]) by (connection, msg))"
	}
	bad := map[string]string{
		"largest open count":   read("max", "open_prs"),
		"summed scans":         read("sum", "open_issues"),
		"smallest alert count": read("min", "security_alerts_high"),
		"largest budget":       read("max", "budget_remaining"),
		"average scan length":  read("avg", "duration_ms"),
	}
	for name, expr := range bad {
		if problems := checkSnapshotTrend(expr); len(problems) != 1 {
			t.Errorf("checkSnapshotTrend(%s) = %v, want one problem", name, problems)
		}
	}
	for name, expr := range map[string]string{
		"newest open count": read("last", "open_prs"), "longest scan": read("max", "duration_ms"), "lowest budget": read("min", "budget_remaining"),
	} {
		if problems := checkSnapshotTrend(expr); len(problems) != 0 {
			t.Errorf("checkSnapshotTrend(%s) = %v, want no problem", name, problems)
		}
	}
}

// checkTrendReads applies the read rule to a chart of a scan summary count:
// a point comes only from a scan whose reads of the count were all complete,
// and a connection that scanned in the interval without one adds NaN, so the
// stacked total leaves a gap there instead of drawing a short count.
func checkTrendReads(expr string) []string {
	var problems []string
	counted, gapped := false, false
	for _, p := range pipelineRe.FindAllStringSubmatch(expr, -1) {
		stages := p[1]
		m := unwrapRe.FindStringSubmatch(stages)
		if m == nil || countReads(m[1]) == nil || !strings.Contains(stages, "scan complete") {
			continue
		}
		counted = true
		if strings.Contains(stages, "label_format") && strings.Contains(stages, m[1]+"=`NaN`") {
			gapped = gapped || strings.Contains(stages, scanEnds)
			for _, r := range countReads(m[1]) {
				if !strings.Contains(stages, "| "+r+"!~`unsupported|excluded`") {
					problems = append(problems, "the NaN branch of "+m[1]+" keeps a line whose "+r+" is unsupported or excluded, so a forge without the signal blanks the chart")
				}
			}
			continue
		}
		for _, r := range countReads(m[1]) {
			if !strings.Contains(stages, "| "+r+"=`complete`") {
				problems = append(problems, "unwraps "+m[1]+" with no "+r+"=`complete` filter, so a partial read draws its short count")
			}
		}
	}
	if counted && !gapped {
		problems = append(problems, "no NaN branch over scan complete|scan stopped, so a connection that scanned without reading the count whole drops out and the total dips")
	}
	return problems
}

func TestDashboard_snapshot_trends_count_only_whole_reads(t *testing.T) {
	trends := 0
	for _, p := range dashboardPanels(loadDashboard(t)) {
		for _, q := range p.queries {
			if q.instant || !strings.Contains(q.expr, "scan complete") {
				continue
			}
			if m := unwrapRe.FindStringSubmatch(q.expr); m == nil || countReads(m[1]) == nil {
				continue
			}
			trends++
			for _, problem := range checkTrendReads(q.expr) {
				t.Errorf("panel %q: %s", p.title, problem)
			}
		}
	}
	if trends == 0 {
		t.Fatalf("%s carries no chart of a scan summary count", dashboardPath)
	}
	const sel = `{container="forge-scout"}`
	counted := func(field, filter string) string {
		return "last_over_time(" + sel + " |= `scan complete` | json msg=\"msg\", connection=\"connection\", " + field + "=\"" + field + "\"" +
			" | msg=`scan complete`" + filter + " | unwrap " + field + " | __error__=\"\" [$__interval]) by (connection, msg, forge)"
	}
	unread := func(field, filter string) string {
		return "last_over_time(" + sel + " |= `scan ` | json msg=\"msg\", connection=\"connection\" | " + scanEnds + filter +
			" | label_format msg=`scan complete`, " + field + "=`NaN` | unwrap " + field + " | __error__=\"\" [$__interval]) by (connection, msg, forge)"
	}
	const (
		prsWhole  = " | open_prs_read=`complete`"
		workWhole = prsWhole + " | open_issues_read=`complete`"
		prsHas    = " | open_prs_read!~`unsupported|excluded`"
		workHas   = prsHas + " | open_issues_read!~`unsupported|excluded`"
	)
	sum := func(parts ...string) string { return "sum by (msg) (" + strings.Join(parts, " or ") + ")" }
	cases := map[string]struct {
		expr string
		want int
	}{
		"every scan's count":                 {sum(counted("open_prs", "")), 2},
		"whole reads, no gap":                {sum(counted("open_prs", prsWhole)), 1},
		"age blind to the issues":            {sum(counted("open_age_lt7d", prsWhole), unread("open_age_lt7d", workHas)), 1},
		"whole reads with the gap":           {sum(counted("open_prs", prsWhole), unread("open_prs", prsHas)), 0},
		"age over both reads":                {sum(counted("open_age_lt7d", workWhole), unread("open_age_lt7d", workHas)), 0},
		"a gap where the forge lacks it":     {sum(counted("open_prs", prsWhole), unread("open_prs", "")), 1},
		"an age gap where issues are absent": {sum(counted("open_age_lt7d", workWhole), unread("open_age_lt7d", prsHas)), 1},
	}
	for name, tc := range cases {
		if problems := checkTrendReads(tc.expr); len(problems) != tc.want {
			t.Errorf("checkTrendReads(%s) = %v, want %d problems", name, problems, tc.want)
		}
	}
}

func TestDashboard_trend_charts_name_a_connection_that_could_not_be_opened(t *testing.T) {
	ep := githubEndpoint("gh")
	ep.openErr = forge.ErrConnection
	h := newHarness(t, ep)
	h.scan(t)
	line := lineFor(t, h.rec, "scan complete", "gh")
	const cause = "could not be opened"
	charts := 0
	for _, p := range dashboardPanels(loadDashboard(t)) {
		if p.viz != "timeseries" {
			continue
		}
		gapped := false
		for _, q := range p.queries {
			if q.instant || !strings.Contains(q.expr, "scan complete") {
				continue
			}
			m := unwrapRe.FindStringSubmatch(q.expr)
			if m == nil {
				continue
			}
			for _, r := range countReads(m[1]) {
				if s := line[r]; s != "complete" && s != "unsupported" && s != "excluded" {
					gapped = true
				}
			}
		}
		if !gapped {
			continue
		}
		charts++
		if desc, _ := field(p.el, "spec", "description").(string); !strings.Contains(desc, cause) {
			t.Errorf("panel %q gaps on a connection that could not be opened, but its description does not say so: %q", p.title, desc)
		}
	}
	if charts == 0 {
		t.Fatalf("Setup: no chart of %s gaps on a connection that could not be opened (scan complete = %v)", dashboardPath, line)
	}
	if para := docSection(t, readMonitoringDoc(t), "A connection whose scans in an interval")[0]; !strings.Contains(para, cause) {
		t.Errorf("%s does not name a connection that could not be opened as a cause of a chart gap: %q", monitoringDoc, para)
	}
}

// formatLabels is what a label_format template makes of each line: Loki
// runs it as a Go text/template over the line's labels, and a label the line
// lacks reads empty.
func formatLabels(t *testing.T, format string, lines []map[string]string) []string {
	t.Helper()
	tmpl, err := template.New("label").Option("missingkey=zero").Parse(format)
	if err != nil {
		t.Fatalf("Setup: parse the label_format %q: %v", format, err)
	}
	var out []string
	for _, l := range lines {
		var b strings.Builder
		if err := tmpl.Execute(&b, l); err != nil {
			t.Fatalf("Setup: run the label_format on %v: %v", l, err)
		}
		out = append(out, b.String())
	}
	return out
}

// checkToolLabels reports a tool row a reader cannot tell apart: an empty
// cell, or two rows with one label.
func checkToolLabels(labels []string) []string {
	var problems []string
	seen := map[string]bool{}
	for _, l := range labels {
		switch {
		case l == "":
			problems = append(problems, "a tool row shows no name")
		case seen[l]:
			problems = append(problems, "two tool rows both read "+l)
		}
		seen[l] = true
	}
	return problems
}

var toolFormatRe = regexp.MustCompile("\\|\\s*label_format\\s+tool=`([^`]*)`")

func TestDashboard_alerts_by_tool_tell_the_rollup_from_a_tool_named_other(t *testing.T) {
	a := githubEndpoint("a")
	var alerts []forge.Alert
	for i, tool := range []string{"other", "t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9", "t10", "t11"} {
		for range 20 - i {
			alerts = append(alerts, forge.Alert{Repo: "o/r", Number: int64(len(alerts) + 1), Severity: "low", Tool: tool})
		}
	}
	a.gh.alerts = map[string][]forge.Alert{"o/r": alerts}
	h := newHarness(t, a)
	h.scan(t)
	logged := lines(h.rec, "security tool")
	if len(logged) != 11 {
		t.Fatalf("Setup: security tool lines = %d, want 11 for twelve tools", len(logged))
	}
	panel := panelTitled(t, dashboardPanels(loadDashboard(t)), "Open alerts by tool")
	format := ""
	for _, q := range panel.queries {
		if m := toolFormatRe.FindStringSubmatch(q.expr); m != nil && strings.Contains(q.expr, "msg=`security tool`") {
			format = m[1]
		}
	}
	if format == "" {
		t.Fatalf("panel %q formats no tool label, so the Tool cell shows the raw field", panel.title)
	}
	for _, problem := range checkToolLabels(formatLabels(t, format, logged)) {
		t.Errorf("panel %q: %s", panel.title, problem)
	}
	if raw := checkToolLabels(formatLabels(t, "{{ .tool }}", logged)); len(raw) != 1 {
		t.Errorf("checkToolLabels(the raw tool field) = %v, want the unnamed rollup row", raw)
	}
	if named := checkToolLabels([]string{"other", "t1", "other"}); len(named) != 1 {
		t.Errorf("checkToolLabels(two rows named other) = %v, want one problem", named)
	}
}

// itemMessages each name one item of one connection. A repository path is
// only unique within its connection, so a table of them names the connection.
// security tool lines add up every GitHub connection of a scan and name none.
var itemMessages = []string{
	"open pull request", "open issue", "failing workflow", "slow workflow", "disabled workflow",
	"security alert", "top security alert", "ci run",
}

// connectionsVar holds every connection in scope, which a Loki query reads
// as the values joined by |.
const connectionsVar = "connections"

// repoCell is the Repository cell of a table of items: the path alone while
// the variable holds only the row's connection, else "<path> on
// <connection>", so an empty or failed variable still names it.
const repoCell = "| label_format repo=`{{ if eq .connection \"$" + connectionsVar + "\" }}{{ .repo }}{{ else }}{{ .repo }} on {{ .connection }}{{ end }}`"

func readsItem(q dashQuery) bool {
	return logQueryRe.MatchString(q.expr) && slices.ContainsFunc(itemMessages, func(msg string) bool {
		return strings.Contains(q.expr, "msg=`"+msg+"`")
	})
}

func readsItems(p *dashPanel) bool { return slices.ContainsFunc(p.queries, readsItem) }

// checkConnectionShown reports how a table of item lines loses the
// connection from its Repository cell: never read, not written into the
// cell, dropped by a transform, or the cell hidden.
func checkConnectionShown(p *dashPanel) []string {
	if !readsItems(p) {
		return nil
	}
	var problems []string
	for _, q := range p.queries {
		if !readsItem(q) {
			continue
		}
		if !jsonKeys(q.expr)["connection"] {
			problems = append(problems, "an item query does not read connection")
		} else if !strings.Contains(q.expr, repoCell) {
			problems = append(problems, "an item query does not write the connection into the Repository cell")
		}
	}
	for _, tr := range p.transforms {
		switch tr["group"] {
		case "filterFieldsByName":
			if names := anyStrings(field(tr, "include", "names")); !slices.Contains(names, "repo") {
				problems = append(problems, "a field filter drops the Repository cell")
			}
		case "groupBy":
			if field(tr, "fields", "connection", "operation") != "groupby" {
				problems = append(problems, "a Group by folds two connections together")
			}
		}
	}
	named := false
	overrides, _ := field(p.el, "spec", "vizConfig", "spec", "fieldConfig", "overrides").([]any)
	for _, o := range overrides {
		id, _ := field(o, "matcher", "id").(string)
		pattern, _ := field(o, "matcher", "options").(string)
		matches := pattern == "repo"
		if id == "byRegexp" {
			matches, _ = regexp.MatchString(pattern, "repo")
		}
		if !matches {
			continue
		}
		props, _ := field(o, "properties").([]any)
		for _, pr := range props {
			switch field(pr, "id") {
			case "custom.hidden":
				if field(pr, "value") == true {
					problems = append(problems, "the Repository column is hidden")
				}
			case "displayName":
				named = named || (id == "byName" && field(pr, "value") == "Repository")
			}
		}
	}
	if !named {
		problems = append(problems, "no column is headed Repository")
	}
	return problems
}

// checkConnectionsVariable reports how the variable the Repository cell
// compares with fails to hold every connection in scope: each one whose
// selected forge logged a scan in the time range, all of them selected.
func checkConnectionsVariable(d map[string]any) []string {
	vars, _ := field(d, "spec", "variables").([]any)
	for _, v := range vars {
		if field(v, "spec", "name") != connectionsVar {
			continue
		}
		var problems []string
		if field(v, "kind") != "QueryVariable" || field(v, "spec", "query", "spec", "label") != "connection" {
			problems = append(problems, "it is not a query for the connection field")
		}
		stream, _ := field(v, "spec", "query", "spec", "stream").(string)
		if !strings.Contains(stream, scanEnds) || !strings.Contains(stream, `| forge=~"$forge"`) {
			problems = append(problems, "it does not read the selected forges' scan lines")
		}
		if field(v, "spec", "multi") != true || field(v, "spec", "includeAll") != true || !slices.Equal(anyStrings(field(v, "spec", "current", "value")), []string{"$__all"}) {
			problems = append(problems, "it does not hold all its values")
		}
		if field(v, "spec", "refresh") != "onTimeRangeChanged" {
			problems = append(problems, "it is not refreshed with the time range")
		}
		return problems
	}
	return []string{"the dashboard declares no " + connectionsVar + " variable"}
}

func TestDashboard_item_tables_show_the_connection(t *testing.T) {
	d := loadDashboard(t)
	for _, problem := range checkConnectionsVariable(d) {
		t.Errorf("variable %s: %s, so a Repository cell cannot tell one connection in scope from several", connectionsVar, problem)
	}
	tables := 0
	for _, p := range dashboardPanels(d) {
		if p.viz != "table" || !readsItems(&p) {
			continue
		}
		tables++
		for _, problem := range checkConnectionShown(&p) {
			t.Errorf("panel %q: %s, so two connections holding one repository path show rows nobody can tell apart", p.title, problem)
		}
	}
	if tables == 0 {
		t.Fatalf("%s carries no item table", dashboardPath)
	}
	const sel = `{container="forge-scout"}`
	read := sel + " |= `slow workflow` | json msg=\"msg\", connection=\"connection\", repo=\"repo\" | msg=`slow workflow` "
	withCell := dashQuery{expr: read + repoCell, maxLines: 21}
	noConn := dashQuery{expr: sel + " |= `slow workflow` | json msg=\"msg\", repo=\"repo\" | msg=`slow workflow`", maxLines: 21}
	noCell := dashQuery{expr: read + "| label_format repo=`{{ .repo }}`", maxLines: 21}
	column := func(matcher, pattern string, props ...any) any {
		return map[string]any{"matcher": map[string]any{"id": matcher, "options": pattern}, "properties": props}
	}
	shown := column("byName", "repo", map[string]any{"id": "displayName", "value": "Repository"})
	el := func(overrides ...any) map[string]any {
		return map[string]any{"spec": map[string]any{"vizConfig": map[string]any{"spec": map[string]any{"fieldConfig": map[string]any{"overrides": overrides}}}}}
	}
	bad := map[string]dashPanel{
		"not read":     {queries: []dashQuery{noConn}, el: el(shown)},
		"not in cell":  {queries: []dashQuery{noCell}, el: el(shown)},
		"not headed":   {queries: []dashQuery{withCell}, el: el()},
		"hidden":       {queries: []dashQuery{withCell}, el: el(shown, column("byRegexp", "^(url|repo)$", map[string]any{"id": "custom.hidden", "value": true}))},
		"filtered out": {queries: []dashQuery{withCell}, el: el(shown), transforms: []map[string]any{{"group": "filterFieldsByName", "include": map[string]any{"names": []any{"workflow"}}}}},
		"grouped away": {queries: []dashQuery{withCell}, el: el(shown), transforms: []map[string]any{{"group": "groupBy", "fields": map[string]any{"repo": map[string]any{"operation": "groupby"}}}}},
	}
	for name, p := range bad {
		if problems := checkConnectionShown(&p); len(problems) != 1 {
			t.Errorf("checkConnectionShown(%s) = %v, want one problem", name, problems)
		}
	}
	good := dashPanel{queries: []dashQuery{withCell}, el: el(shown), transforms: []map[string]any{{"group": "filterFieldsByName", "include": map[string]any{"names": []any{"repo", "workflow"}}}}}
	if problems := checkConnectionShown(&good); len(problems) != 0 {
		t.Errorf("checkConnectionShown(connection written into a headed Repository cell) = %v, want no problem", problems)
	}
	variable := func(edit func(spec map[string]any)) map[string]any {
		spec := map[string]any{
			"name": connectionsVar, "multi": true, "includeAll": true, "refresh": "onTimeRangeChanged",
			"current": map[string]any{"value": []any{"$__all"}},
			"query":   map[string]any{"spec": map[string]any{"label": "connection", "stream": sel + " |= `scan ` | json msg=\"msg\", forge=\"forge\", connection=\"connection\" | " + scanEnds + " | forge=~\"$forge\""}},
		}
		edit(spec)
		return map[string]any{"spec": map[string]any{"variables": []any{map[string]any{"kind": "QueryVariable", "spec": spec}}}}
	}
	if problems := checkConnectionsVariable(variable(func(map[string]any) {})); len(problems) != 0 {
		t.Errorf("checkConnectionsVariable(every connection in scope, all selected) = %v, want no problem", problems)
	}
	badVars := map[string]func(map[string]any){
		"one value":   func(s map[string]any) { s["multi"] = false },
		"first value": func(s map[string]any) { s["current"] = map[string]any{"value": []any{"gh"}} },
		"forge field": func(s map[string]any) {
			s["query"] = map[string]any{"spec": map[string]any{"label": "forge", "stream": field(s, "query", "spec", "stream")}}
		},
		"every forge": func(s map[string]any) {
			s["query"] = map[string]any{"spec": map[string]any{"label": "connection", "stream": sel + " |= `scan ` | json msg=\"msg\", connection=\"connection\" | " + scanEnds}}
		},
		"stale on load": func(s map[string]any) { s["refresh"] = "onDashboardLoad" },
		"absent":        func(s map[string]any) { s["name"] = "connection" },
	}
	for name, edit := range badVars {
		if problems := checkConnectionsVariable(variable(edit)); len(problems) != 1 {
			t.Errorf("checkConnectionsVariable(%s) = %v, want one problem", name, problems)
		}
	}
}

// checkIdentityWraps reports a Repository or Connection column that cuts a
// long value to a prefix, which two rows can share.
func checkIdentityWraps(p *dashPanel) []string {
	var problems []string
	overrides, _ := field(p.el, "spec", "vizConfig", "spec", "fieldConfig", "overrides").([]any)
	for _, o := range overrides {
		name, _ := field(o, "matcher", "options").(string)
		if field(o, "matcher", "id") != "byName" || (name != "repo" && name != "connection") {
			continue
		}
		wraps := false
		props, _ := field(o, "properties").([]any)
		for _, pr := range props {
			wraps = wraps || (field(pr, "id") == "custom.wrapText" && field(pr, "value") == true)
		}
		if !wraps {
			problems = append(problems, "the "+name+" column cuts a long value instead of wrapping it")
		}
	}
	return problems
}

func TestDashboard_identity_columns_wrap_a_long_value(t *testing.T) {
	for _, p := range dashboardPanels(loadDashboard(t)) {
		if p.viz != "table" {
			continue
		}
		for _, problem := range checkIdentityWraps(&p) {
			t.Errorf("panel %q: %s", p.title, problem)
		}
	}
	column := func(name string, props ...any) map[string]any {
		overrides := []any{map[string]any{"matcher": map[string]any{"id": "byName", "options": name}, "properties": append([]any{map[string]any{"id": "displayName", "value": "X"}}, props...)}}
		return map[string]any{"spec": map[string]any{"vizConfig": map[string]any{"spec": map[string]any{"fieldConfig": map[string]any{"overrides": overrides}}}}}
	}
	fixed := map[string]any{"id": "custom.width", "value": 120}
	wrap := map[string]any{"id": "custom.wrapText", "value": true}
	for name, el := range map[string]map[string]any{"fixed connection": column("connection", fixed), "repository": column("repo")} {
		if problems := checkIdentityWraps(&dashPanel{el: el}); len(problems) != 1 {
			t.Errorf("checkIdentityWraps(%s) = %v, want one problem", name, problems)
		}
	}
	if problems := checkIdentityWraps(&dashPanel{el: column("connection", fixed, wrap)}); len(problems) != 0 {
		t.Errorf("checkIdentityWraps(a fixed connection column that wraps) = %v, want no problem", problems)
	}
}

// checkDayText reports a day chart whose words name a different bucket than
// the run state's: a ci day line counts the runs created on its UTC day.
func checkDayText(title, desc string) []string {
	var problems []string
	if strings.Contains(title+" "+desc, "started") {
		problems = append(problems, "it says started where the day is the one the runs were created on")
	}
	if !strings.Contains(desc, "created") {
		problems = append(problems, "its description does not say the day is the one the runs were created on")
	}
	return problems
}

func TestDashboard_day_charts_name_the_day_the_runs_were_created(t *testing.T) {
	charts := 0
	for _, p := range dashboardPanels(loadDashboard(t)) {
		if !slices.ContainsFunc(p.queries, func(q dashQuery) bool { return strings.Contains(q.expr, "msg=`"+dayMessage+"`") }) {
			continue
		}
		charts++
		desc, _ := field(p.el, "spec", "description").(string)
		for _, problem := range checkDayText(p.title, desc) {
			t.Errorf("panel %q: %s", p.title, problem)
		}
	}
	if charts == 0 {
		t.Fatalf("%s carries no chart of %s lines", dashboardPath, dayMessage)
	}
	if problems := checkDayText("Runs started per day by result", "Runs started each UTC day."); len(problems) != 2 {
		t.Errorf("checkDayText(start day) = %v, want two problems", problems)
	}
	if problems := checkDayText("Runs created per day by result", "Runs created each UTC day, timed from their start."); len(problems) != 0 {
		t.Errorf("checkDayText(created day) = %v, want no problem", problems)
	}
}

// TestDashboard_contract_check_rejects pins that the checks can fail: each
// expression or panel breaks the contract one way.
func TestDashboard_contract_check_rejects(t *testing.T) {
	c := contract{
		"ci run":        {"run_id": true, "state": true, "repo": true, "connection": true, "updated_at": true, "forge": true},
		"scan complete": {"open_prs": true, "connection": true, "forge": true, "scan_id": true},
		"open issue":    {"repo": true, "scan_id": true},
	}
	const sel = `{container="forge-scout"}`
	runs := sel + " |= `ci run` | json msg=\"msg\", connection=\"connection\", repo=\"repo\", run_id=\"run_id\", state=\"state\", updated_at=\"updated_at\" | msg=`ci run`"
	cases := map[string]string{
		"unlogged msg":       sel + " |= `made up` | json msg=\"msg\" | msg=`made up`",
		"unlogged key":       sel + " |= `scan complete` | json msg=\"msg\", open_mrs=\"open_mrs\" | msg=`scan complete`",
		"bare json":          sel + " |= `scan complete` | json | msg=`scan complete`",
		"no msg filter":      sel + " | json open_prs=\"open_prs\"",
		"regex line filter":  sel + " |~ `scan (complete|stopped)` | json msg=\"msg\" | msg=`scan complete`",
		"foreign line":       sel + " |= `ci run` | json msg=\"msg\" | msg=`scan complete`",
		"regex alternative":  sel + " | json msg=\"msg\" | msg=~`scan .*`",
		"msg alternation":    sel + " |= `scan ` | json msg=\"msg\" | msg=~`scan complete|scan stopped`",
		"other selector":     `{job="forge-scout"} |= ` + "`scan complete` | json msg=\"msg\" | msg=`scan complete`",
		"fixed window":       "sum(count_over_time(" + sel + " |= `scan complete` | json msg=\"msg\" | msg=`scan complete` [7d]))",
		"hidden window":      "sum(last_over_time(" + sel + " |= `scan complete` | json msg=\"msg\", connection=\"connection\", open_prs=\"open_prs\" | msg=`scan complete` | unwrap open_prs [$window]) by (connection))",
		"ungrouped unwrap":   "sum(last_over_time(" + sel + " |= `scan complete` | json msg=\"msg\", open_prs=\"open_prs\" | msg=`scan complete` | unwrap open_prs [$__range]))",
		"undeduped runs":     "sum by (state) (count_over_time(" + runs + " [$__interval]))",
		"identity grouping":  "sum by (run_id) (count_over_time(" + runs + " [$__interval]))",
		"key with no outer":  "count by (connection, repo, run_id, state, updated_at) (count_over_time(" + runs + " [$__interval]))",
		"key beside a title": "sum by (state) (count by (connection, repo, run_id, state, updated_at, title) (count_over_time(" + runs + " [$__interval])))",
	}
	for name, expr := range cases {
		t.Run(name, func(t *testing.T) {
			if problems := checkExpr(c, expr); len(problems) == 0 {
				t.Errorf("checkExpr(%q) reported nothing, want a contract violation", expr)
			}
		})
	}
	stale := "sum(last_over_time(" + sel + " |= `scan complete` | json msg=\"msg\", connection=\"connection\", open_prs=\"open_prs\" | msg=`scan complete`" +
		" | unwrap open_prs | __error__=\"\" [$__range]) by (connection))"
	if problems := checkTile(stale); len(problems) != 2 {
		t.Errorf("checkTile(last_over_time over scan complete alone) = %v, want the scan stopped and the NaN problems", problems)
	}
	tile := func(format string) string {
		return "sum(last_over_time(" + sel + " |= `scan ` | json msg=\"msg\", connection=\"connection\", open_prs=\"open_prs\", open_prs_read=\"open_prs_read\"" +
			" | " + scanEnds + " | label_format open_prs=`" + format + "` | unwrap open_prs | __error__=\"\" [$__range]) by (connection))"
	}
	partial := tile(`{{ if .open_prs }}{{ .open_prs }}{{ else }}NaN{{ end }}`)
	if problems := checkTile(partial); len(problems) != 5 {
		t.Errorf("checkTile(a tile showing any logged count) = %v, want the three unfinished reads shown and the two missing signals as NaN", problems)
	}
	const prsLacking = `{{ else if or (eq .open_prs_read "unsupported") (eq .open_prs_read "excluded") }}`
	blanked := tile(`{{ if and (eq .open_prs_read "complete") .open_prs }}{{ .open_prs }}{{ else }}NaN{{ end }}`)
	if problems := checkTile(blanked); len(problems) != 2 {
		t.Errorf("checkTile(a tile a forge without the signal blanks) = %v, want unsupported and excluded read as NaN", problems)
	}
	fresh := tile(`{{ if and (eq .open_prs_read "complete") .open_prs }}{{ .open_prs }}` + prsLacking + `{{ else }}NaN{{ end }}`)
	if problems := checkTile(fresh); len(problems) != 0 {
		t.Errorf("checkTile(newest-scan tile) = %v, want no problem", problems)
	}
	age := func(format string) string {
		return "sum(last_over_time(" + sel + " |= `scan ` | json msg=\"msg\", connection=\"connection\", open_age_lt7d=\"open_age_lt7d\", open_prs_read=\"open_prs_read\", open_issues_read=\"open_issues_read\"" +
			" | " + scanEnds + " | label_format open_age_lt7d=`" + format + "` | unwrap open_age_lt7d | __error__=\"\" [$__range]) by (connection))"
	}
	const workLacking = `{{ else if or (eq .open_prs_read "unsupported") (eq .open_prs_read "excluded") (eq .open_issues_read "unsupported") (eq .open_issues_read "excluded") }}`
	if problems := checkTile(age(`{{ if and (eq .open_prs_read "complete") .open_age_lt7d }}{{ .open_age_lt7d }}` + workLacking + `{{ else }}NaN{{ end }}`)); len(problems) != 3 {
		t.Errorf("checkTile(an age tile blind to the issues read) = %v, want the three unfinished issue reads shown", problems)
	}
	if problems := checkTile(age(`{{ if and (eq .open_prs_read "complete") (eq .open_issues_read "complete") .open_age_lt7d }}{{ .open_age_lt7d }}` + prsLacking + `{{ else }}NaN{{ end }}`)); len(problems) != 2 {
		t.Errorf("checkTile(an age tile a forge without issues blanks) = %v, want unsupported and excluded issues read as NaN", problems)
	}
	if problems := checkTile(age(`{{ if and (eq .open_prs_read "complete") (eq .open_issues_read "complete") .open_age_lt7d }}{{ .open_age_lt7d }}` + workLacking + `{{ else }}NaN{{ end }}`)); len(problems) != 0 {
		t.Errorf("checkTile(an age tile over both reads) = %v, want no problem", problems)
	}
	for name, window := range map[string]string{"step window": "[$__interval]) by (connection)", "every connection together": "[$__range]) by (forge)"} {
		older := strings.Replace(fresh, "[$__range]) by (connection)", window, 1)
		if problems := checkTile(older); len(problems) != 1 {
			t.Errorf("checkTile(tile over the %s) = %v, want the newest-scan problem", name, problems)
		}
	}
	if problems := checkTile(strings.Replace(fresh, "last_over_time", "max_over_time", 1)); len(problems) != 1 {
		t.Errorf("checkTile(tile with the largest count in the range) = %v, want the newest-scan problem", problems)
	}
	ok := "sum by (state) (count by (connection, repo, run_id, state, updated_at, forge) (count_over_time(" + runs + " [$__interval])))"
	if problems := checkExpr(c, ok); len(problems) != 0 {
		t.Errorf("checkExpr(deduplicated run count) = %v, want no problem", problems)
	}
	newest := dashQuery{expr: sel + " |= `scan ` | json msg=\"msg\", scan_id=\"scan_id\" | " + scanEnds + " | keep scan_id", maxLines: 1}
	issues := dashQuery{expr: sel + " |= `open issue` | json msg=\"msg\", scan_id=\"scan_id\" | msg=`open issue`", maxLines: 41}
	join := map[string]any{"group": "joinByField", "byField": "scan_id", "mode": "inner"}
	panels := map[string]dashPanel{
		"no join":       {queries: []dashQuery{newest, issues}},
		"outer join":    {queries: []dashQuery{newest, issues}, transforms: []map[string]any{{"group": "joinByField", "byField": "scan_id", "mode": "outer"}}},
		"no scan read":  {queries: []dashQuery{issues}, transforms: []map[string]any{join}},
		"no line limit": {queries: []dashQuery{newest, {expr: issues.expr}}, transforms: []map[string]any{join}},
	}
	for name, p := range panels {
		t.Run(name, func(t *testing.T) {
			if problems := checkPanel(&p); len(problems) == 0 {
				t.Errorf("checkPanel(%s) reported nothing, want a violation", name)
			}
		})
	}
	good := dashPanel{queries: []dashQuery{newest, issues}, transforms: []map[string]any{join}}
	if problems := checkPanel(&good); len(problems) != 0 {
		t.Errorf("checkPanel(joined snapshot) = %v, want no problem", problems)
	}
	runRows := dashQuery{expr: runs, maxLines: 200}
	newestFirst := map[string]any{"group": "sortBy", "sort": []any{map[string]any{"field": "Time", "desc": true}}}
	byRun := map[string]any{"group": "groupBy", "fields": map[string]any{
		"connection": map[string]any{"operation": "groupby"}, "repo": map[string]any{"operation": "groupby"}, "run_id": map[string]any{"operation": "groupby"},
	}}
	runPanels := map[string]dashPanel{
		"ungrouped run rows":       {queries: []dashQuery{runRows}, transforms: []map[string]any{newestFirst}},
		"grouped before the sort":  {queries: []dashQuery{runRows}, transforms: []map[string]any{byRun, newestFirst}},
		"grouped without the repo": {queries: []dashQuery{runRows}, transforms: []map[string]any{newestFirst, {"group": "groupBy", "fields": map[string]any{"run_id": map[string]any{"operation": "groupby"}}}}},
	}
	for name, p := range runPanels {
		t.Run(name, func(t *testing.T) {
			if problems := checkPanel(&p); len(problems) == 0 {
				t.Errorf("checkPanel(%s) reported nothing, want a violation", name)
			}
		})
	}
	grouped := dashPanel{queries: []dashQuery{runRows}, transforms: []map[string]any{newestFirst, byRun}}
	if problems := checkPanel(&grouped); len(problems) != 0 {
		t.Errorf("checkPanel(run rows grouped by run) = %v, want no problem", problems)
	}
}

// overrideMappings returns, per field name, the value keys its overrides map.
func overrideMappings(p *dashPanel) map[string][]string {
	out := map[string][]string{}
	overrides, _ := field(p.el, "spec", "vizConfig", "spec", "fieldConfig", "overrides").([]any)
	for _, o := range overrides {
		name, _ := field(o, "matcher", "options").(string)
		props, _ := field(o, "properties").([]any)
		for _, pr := range props {
			if field(pr, "id") != "mappings" {
				continue
			}
			ms, _ := field(pr, "value").([]any)
			for _, m := range ms {
				if opts, ok := field(m, "options").(map[string]any); ok && field(m, "type") == "value" {
					out[name] = append(out[name], slices.Collect(maps.Keys(opts))...)
				}
			}
		}
	}
	return out
}

func TestDashboard_maps_every_value_the_code_logs(t *testing.T) {
	panels := dashboardPanels(loadDashboard(t))
	var states []string
	for s := readUnread; s <= readExcluded; s++ {
		states = append(states, s.String())
	}
	var causes []string
	for i := range diagnoses {
		causes = append(causes, diagnoses[i].cause)
	}
	var checks []string
	for _, s := range []forge.CheckState{forge.CheckPassing, forge.CheckFailing, forge.CheckPending, forge.CheckNeutral, forge.CheckNone, forge.CheckUnknown, forge.CheckUnreadable} {
		checks = append(checks, string(s))
	}
	readColumns, causeColumns, checkColumns := 0, 0, 0
	for _, p := range panels {
		for name, keys := range overrideMappings(&p) {
			var want []string
			switch {
			case strings.HasSuffix(name, "_read"):
				want, readColumns = states, readColumns+1
			case name == "cause":
				want, causeColumns = causes, causeColumns+1
			case name == "checks":
				want, checkColumns = checks, checkColumns+1
			default:
				continue
			}
			for _, v := range want {
				if !slices.Contains(keys, v) {
					t.Errorf("panel %q column %s maps no %q, which forge-scout logs", p.title, name, v)
				}
			}
		}
	}
	if readColumns == 0 || causeColumns == 0 || checkColumns == 0 {
		t.Fatalf("read-state columns %d, cause columns %d, checks columns %d, want each mapped somewhere", readColumns, causeColumns, checkColumns)
	}
	var stoppedPatterns string
	for _, p := range panels {
		if strings.Contains(strings.Join(slices.Collect(func(yield func(string) bool) {
			for _, q := range p.queries {
				yield(q.expr)
			}
		}), ""), "msg=`scan stopped`") {
			raw, _ := json.Marshal(field(p.el, "spec", "vizConfig", "spec", "fieldConfig", "overrides"))
			stoppedPatterns += string(raw)
		}
	}
	for _, reason := range []string{stopReadReserve, stopRateLimited, stopScanTimeout} {
		if !strings.Contains(stoppedPatterns, reason) {
			t.Errorf("no stopped-scan panel names the stop reason %q", reason)
		}
	}
}

func TestDashboard_reads_every_signal_state_and_coverage_field(t *testing.T) {
	d := loadDashboard(t)
	var all strings.Builder
	for _, p := range dashboardPanels(d) {
		for _, q := range p.queries {
			all.WriteString(q.expr)
		}
	}
	text := all.String()
	var want []string
	for f := range families {
		if f != famState {
			want = append(want, familyNames[f]+"_read")
		}
	}
	want = append(want, "owners_empty", "unsupported_signals", "checks_unsupported_for_token")
	for _, key := range want {
		if !strings.Contains(text, `="`+key+`"`) {
			t.Errorf("no panel reads %s", key)
		}
	}
	if !strings.Contains(text, "msg=`runs coverage gap`") {
		t.Error("no panel reads the runs coverage gap line")
	}
}

// checkDayChart applies the day rule: a chart per day reads the ci day
// lines, never ci run lines, which Loki dates by when it received them, so a
// cold start would pile the lookback onto one day. Each read is an instant
// query of the newest line of every (connection, repository, day) over the
// range: a sum would add up scans and a maximum would keep a result a re-run
// moved.
func checkDayChart(p *dashPanel) []string {
	if !strings.Contains(p.title, " per day") {
		return nil
	}
	var problems []string
	for _, q := range p.queries {
		if strings.Contains(q.expr, "msg=`ci run`") {
			problems = append(problems, "a per-day chart counts ci run lines, which Loki dates by arrival, not by when the run started")
		}
		if !strings.Contains(q.expr, "msg=`"+dayMessage+"`") {
			continue
		}
		if !q.instant {
			problems = append(problems, "a day-line chart is not an instant query, so each step reads a different set of scans")
		}
		for _, m := range overTimeRe.FindAllStringSubmatch(q.expr, -1) {
			if m[1] != "last" {
				problems = append(problems, "a day-line chart reads "+m[1]+"_over_time, want last_over_time, each day's newest count")
			}
		}
		if strings.Count(q.expr, "[$__range]") != strings.Count(q.expr, "_over_time(") {
			problems = append(problems, "a day-line chart reads a window other than [$__range]")
		}
		for _, by := range innerByRe.FindAllStringSubmatch(q.expr, -1) {
			labels := strings.Split(strings.ReplaceAll(by[1], " ", ""), ",")
			for _, k := range dayKey {
				if !slices.Contains(labels, k) {
					problems = append(problems, "a day-line read takes its newest line by ("+by[1]+"), which leaves out "+k)
				}
			}
		}
		problems = append(problems, checkDayGrouping(q.expr)...)
		if !inRangeOnly(q.expr) {
			problems = append(problems, "a day-line read does not keep exactly the days whose UTC midnight is inside the range: the day the range starts in draws as a sliver left of the axis, and a day before the range counts in the legend")
		}
	}
	return problems
}

// checkDayGrouping applies the connection rule to every grouping outside the
// newest-line read: a series named for a repository also names its
// connection, since two connections can hold a repository at one path.
func checkDayGrouping(expr string) []string {
	var problems []string
	for _, by := range byRe.FindAllStringSubmatch(innerByRe.ReplaceAllString(expr, ""), -1) {
		labels := strings.Split(strings.ReplaceAll(strings.ReplaceAll(by[1], "$split", ""), " ", ""), ",")
		if slices.Contains(labels, "repo") && !slices.Contains(labels, "connection") {
			problems = append(problems, "a day chart sums by ("+by[1]+"), which folds one repository path on two connections into one series")
		}
	}
	return problems
}

// innerByRe captures the grouping of each range aggregation over a range.
var innerByRe = regexp.MustCompile(`\[\$__range\]\) by \(([^)]*)\)`)

// dayRangeRe matches a day-line read that turns each day into its UTC
// midnight in seconds and keeps the days at or after the range start.
var dayRangeRe = regexp.MustCompile("label_format (\\w+)=`\\{\\{ toDateInZone \"2006-01-02\" \"UTC\" \\.day \\| unixEpoch \\}\\}` \\| (\\w+) >= \\$\\{__from:date:seconds\\}")

// inRangeOnly reports whether every day-line read in expr carries the range
// filter dayRangeRe describes.
func inRangeOnly(expr string) bool {
	matches := dayRangeRe.FindAllStringSubmatch(expr, -1)
	if len(matches) != strings.Count(expr, "msg=`"+dayMessage+"`") {
		return false
	}
	for _, m := range matches {
		if m[1] != m[2] {
			return false
		}
	}
	return true
}

func TestDashboard_day_charts_read_the_day_lines(t *testing.T) {
	charts := 0
	for _, p := range dashboardPanels(loadDashboard(t)) {
		if !strings.Contains(p.title, " per day") {
			continue
		}
		charts++
		for _, problem := range checkDayChart(&p) {
			t.Errorf("panel %q: %s", p.title, problem)
		}
	}
	if charts == 0 {
		t.Fatalf("%s carries no per-day chart", dashboardPath)
	}
	const sel = `{container="forge-scout"}`
	read := sel + " |= `ci day` | json msg=\"msg\", connection=\"connection\", repo=\"repo\", day=\"day\", failing=\"failing\" | msg=`ci day` "
	const inRange = "| label_format daystart=`{{ toDateInZone \"2006-01-02\" \"UTC\" .day | unixEpoch }}` | daystart >= ${__from:date:seconds} "
	days := read + inRange + "| unwrap failing"
	bad := map[string]dashPanel{
		"ci run count":  {title: "Runs per day", queries: []dashQuery{{expr: "sum(count_over_time(" + sel + " |= `ci run` | json msg=\"msg\" | msg=`ci run` [$__interval]))"}}},
		"range query":   {title: "Runs per day", queries: []dashQuery{{expr: "sum(last_over_time(" + days + " [$__range]) by (connection, repo, day))"}}},
		"largest count": {title: "Runs per day", queries: []dashQuery{{expr: "sum(max_over_time(" + days + " [$__range]) by (connection, repo, day))", instant: true}}},
		"summed scans":  {title: "Runs per day", queries: []dashQuery{{expr: "sum(sum_over_time(" + days + " [$__range]) by (connection, repo, day))", instant: true}}},
		"interval":      {title: "Runs per day", queries: []dashQuery{{expr: "sum(last_over_time(" + days + " [$__interval]) by (connection, repo, day))", instant: true}}},
		"no repo":       {title: "Runs per day", queries: []dashQuery{{expr: "sum(last_over_time(" + days + " [$__range]) by (connection, day))", instant: true}}},
		"repo without connection": {title: "Failed runs per day", queries: []dashQuery{{
			expr: "sum by (day, repo) (last_over_time(" + days + " [$__range]) by (connection, forge, repo, day))", instant: true,
		}}},
		"no range filter": {title: "Runs per day", queries: []dashQuery{{
			expr: "sum(last_over_time(" + read + "| unwrap failing [$__range]) by (connection, forge, repo, day))", instant: true,
		}}},
		"range start day kept": {title: "Runs per day", queries: []dashQuery{{
			expr: "sum(last_over_time(" + read + "| label_format daynum=`{{ .day | replace \"-\" \"\" }}` | daynum >= ${__from:date:YYYYMMDD} | unwrap failing [$__range]) by (connection, forge, repo, day))", instant: true,
		}}},
		"midnight start day dropped": {title: "Runs per day", queries: []dashQuery{{
			expr: "sum(last_over_time(" + read + "| label_format daynum=`{{ .day | replace \"-\" \"\" }}` | daynum > ${__from:date:YYYYMMDD} | unwrap failing [$__range]) by (connection, forge, repo, day))", instant: true,
		}}},
	}
	for name, p := range bad {
		t.Run(name, func(t *testing.T) {
			if problems := checkDayChart(&p); len(problems) == 0 {
				t.Errorf("checkDayChart(%s) reported nothing, want a violation", name)
			}
		})
	}
	good := map[string]dashPanel{
		"newest line per repository day": {title: "Runs per day", queries: []dashQuery{{expr: "sum(last_over_time(" + days + " [$__range]) by (connection, forge, repo, day))", instant: true}}},
		"repository on its connection": {title: "Failed runs per day", queries: []dashQuery{{
			expr: "sum by (day, connection, repo) (last_over_time(" + days + " [$__range]) by (connection, forge, repo, day))", instant: true,
		}}},
	}
	for name, p := range good {
		if problems := checkDayChart(&p); len(problems) != 0 {
			t.Errorf("checkDayChart(%s) = %v, want no problem", name, problems)
		}
	}
}

// unfiltered is the panels the forge filter does not narrow: the scan
// loop's liveness belongs to no forge.
var unfiltered = []string{"Scout status"}

// TestDashboard_shape pins what makes the file importable anywhere: a
// schema v2 resource named forge-scout whose every query reads the
// datasource variable, a forge filter over the four products, and daily
// bars for every event count.
func TestDashboard_shape(t *testing.T) {
	d := loadDashboard(t)
	if d["apiVersion"] != "dashboard.grafana.app/v2" || d["kind"] != "Dashboard" || field(d, "metadata", "name") != "forge-scout" {
		t.Errorf("apiVersion, kind, name = %v, %v, %v, want dashboard.grafana.app/v2, Dashboard, forge-scout", d["apiVersion"], d["kind"], field(d, "metadata", "name"))
	}
	vars, _ := field(d, "spec", "variables").([]any)
	var names []string
	for _, v := range vars {
		name, _ := field(v, "spec", "name").(string)
		names = append(names, name)
		if name == "forge" {
			var got []string
			opts, _ := field(v, "spec", "options").([]any)
			for _, o := range opts {
				val, _ := field(o, "value").(string)
				got = append(got, val)
			}
			want := []string{forge.ProductGitHub.String(), forge.ProductGitLab.String(), forge.ProductGitea.String(), forge.ProductForgejo.String()}
			if !slices.Equal(got, want) {
				t.Errorf("forge variable options = %v, want %v", got, want)
			}
			// A connection that failed before its product was known logs forge
			// unknown, and All must still show it.
			all, _ := field(v, "spec", "allValue").(string)
			re, err := regexp.Compile("^(?:" + all + ")$")
			if all == "" || err != nil {
				t.Errorf("forge variable allValue = %q, want a regex matching every forge value, unknown included", all)
				continue
			}
			for _, f := range append(want, forge.ProductUnknown.String()) {
				if !re.MatchString(f) {
					t.Errorf("forge variable allValue %q does not match forge %q, so All hides its lines", all, f)
				}
			}
		}
	}
	if !slices.Equal(names, []string{"datasource", "forge", "security_forges", connectionsVar, "split"}) {
		t.Errorf("variables = %v, want [datasource forge security_forges %s split]", names, connectionsVar)
	}
	want := map[string]any{"name": "${datasource}"}
	panels := dashboardPanels(d)
	for _, p := range panels {
		for _, q := range p.queries {
			if ds, _ := q.datasource.(map[string]any); !maps.Equal(ds, want) {
				t.Errorf("panel %q query datasource = %v, want %v", p.title, q.datasource, want)
			}
			if !strings.Contains(q.expr, `| forge=~"$forge"`) && !slices.Contains(unfiltered, p.title) {
				t.Errorf("panel %q does not filter on the forge variable: %s", p.title, q.expr)
			}
		}
		if p.viz != "timeseries" || field(p.el, "spec", "vizConfig", "spec", "fieldConfig", "defaults", "custom", "drawStyle") != "bars" {
			continue
		}
		if interval := field(p.el, "spec", "data", "spec", "queryOptions", "interval"); interval != "1d" || !strings.Contains(p.title, " per day") {
			t.Errorf("bar chart %q has minimum interval %v, want 1d and a title saying per day", p.title, interval)
		}
	}
}

// panelTitled returns the dashboard's one panel titled title.
func panelTitled(t *testing.T, panels []dashPanel, title string) dashPanel {
	t.Helper()
	var found []dashPanel
	for _, p := range panels {
		if p.title == title {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s carries %d panels titled %q, want 1", dashboardPath, len(found), title)
	}
	return found[0]
}

// maxTitleRunes approximates the widest title Grafana 13.2.3 shows whole at
// a 412 px viewport: a full-width title box is 256 px there, and "Slowest
// workflows by p95 run time" (33 characters) measures 253 px. Glyph widths
// vary, so a title near the limit needs a render to prove it fits.
const maxTitleRunes = 33

func TestDashboard_panel_titles_fit_a_narrow_screen(t *testing.T) {
	panels := dashboardPanels(loadDashboard(t))
	if len(panels) == 0 {
		t.Fatalf("Setup: %s carries no panels", dashboardPath)
	}
	for _, p := range panels {
		if n := utf8.RuneCountInString(p.title); n > maxTitleRunes {
			t.Errorf("panel %q has %d characters, want at most %d so a 412 px screen shows it whole", p.title, n, maxTitleRunes)
		}
	}
}

// checkIntegrity applies the Scan integrity rule: the tile counts the
// connections whose newest scan complete or scan stopped line in the range
// says degraded, so a clean scan clears it at once, and never counts scan
// degraded lines, which a clean newest scan does not undo.
func checkIntegrity(p *dashPanel) []string {
	if len(p.queries) != 1 {
		return []string{"Scan integrity reads more or less than one query"}
	}
	expr := p.queries[0].expr
	var problems []string
	if !jsonKeys(expr)["degraded"] || !strings.Contains(expr, `eq .degraded "true"`) {
		problems = append(problems, "Scan integrity does not read the degraded field of each scan")
	}
	if !strings.Contains(expr, scanEnds) {
		problems = append(problems, "Scan integrity does not read both scan complete and scan stopped, so a stopped scan's verdict is missed")
	}
	if !newestPerConnection(expr) {
		problems = append(problems, "Scan integrity does not take each connection's newest scan in the selected range")
	}
	if strings.Contains(expr, "msg=`scan degraded`") {
		problems = append(problems, "Scan integrity counts scan degraded lines, which stay in the range after a clean scan")
	}
	return problems
}

func TestDashboard_scan_integrity_reads_each_connections_newest_verdict(t *testing.T) {
	p := panelTitled(t, dashboardPanels(loadDashboard(t)), "Scan integrity")
	for _, problem := range checkIntegrity(&p) {
		t.Errorf("panel %q: %s\nexpr: %s", p.title, problem, p.queries[0].expr)
	}
	const sel = `{container="forge-scout"}`
	bad := map[string]string{
		"degraded lines": "count(sum by (connection) (count_over_time(" + sel + " |= `scan degraded` | json msg=\"msg\", connection=\"connection\" | msg=`scan degraded` [$__range])))",
		"scan complete only": "sum(last_over_time(" + sel + " |= `scan complete` | json msg=\"msg\", connection=\"connection\", degraded=\"degraded\" | msg=`scan complete`" +
			" | label_format verdict=`{{ if eq .degraded \"true\" }}1{{ else }}0{{ end }}` | unwrap verdict | __error__=\"\" [$__range]) by (connection))",
		"any degraded scan in the range": "sum(max_over_time(" + sel + " |= `scan ` | json msg=\"msg\", connection=\"connection\", degraded=\"degraded\" | " + scanEnds +
			" | label_format verdict=`{{ if eq .degraded \"true\" }}1{{ else }}0{{ end }}` | unwrap verdict | __error__=\"\" [$__range]) by (connection))",
	}
	for name, expr := range bad {
		t.Run(name, func(t *testing.T) {
			if problems := checkIntegrity(&dashPanel{queries: []dashQuery{{expr: expr}}}); len(problems) == 0 {
				t.Errorf("checkIntegrity(%s) reported nothing, want a violation", name)
			}
		})
	}
}

// checkScoutStatus applies the staleness rule: the status compares the age
// of each connection's newest scan complete or scan stopped line at the end
// of the range with the scan_interval_s that line carries, so it needs no
// constant that assumes a cadence.
func checkScoutStatus(p *dashPanel) []string {
	if len(p.queries) != 1 {
		return []string{"Scout status reads more or less than one query"}
	}
	expr := p.queries[0].expr
	var problems []string
	if !jsonKeys(expr)["scan_interval_s"] || !strings.Contains(expr, ".scan_interval_s") {
		problems = append(problems, "Scout status does not judge the newest scan by the scan_interval_s it carries")
	}
	if !strings.Contains(expr, "${__to:date:seconds}") || !strings.Contains(expr, "unixEpoch __timestamp__") {
		problems = append(problems, "Scout status does not measure the newest scan's age at the end of the range")
	}
	if !strings.Contains(expr, scanEnds) || !newestPerConnection(expr) {
		problems = append(problems, "Scout status does not take each connection's newest scan complete or scan stopped line in the selected range")
	}
	return problems
}

func TestDashboard_scout_status_judges_the_newest_scan_by_its_interval(t *testing.T) {
	p := panelTitled(t, dashboardPanels(loadDashboard(t)), "Scout status")
	for _, problem := range checkScoutStatus(&p) {
		t.Errorf("panel %q: %s\nexpr: %s", p.title, problem, p.queries[0].expr)
	}
	const window = "sum(count_over_time({container=\"forge-scout\"} |= `scan ` | json msg=\"msg\" | " + scanEnds + " [$window])) or vector(0)"
	if problems := checkScoutStatus(&dashPanel{queries: []dashQuery{{expr: window}}}); len(problems) == 0 {
		t.Error("checkScoutStatus(a count of scans in a fixed window) reported nothing, want a violation")
	}
}

// degradedInRange is the Scout health panel that counts the degraded scans
// of the selected range per connection and cause.
const degradedInRange = "Degraded scans in this range"

func checkDegradedInRange(p *dashPanel) []string {
	if len(p.queries) != 1 {
		return []string{"the panel reads more or less than one query"}
	}
	q := p.queries[0]
	var problems []string
	if !q.instant || !strings.Contains(q.expr, "msg=`scan degraded`") || !regexp.MustCompile(`count_over_time\(.*\[\$__range\]\)`).MatchString(q.expr) {
		problems = append(problems, "the panel is not one instant count of the scan degraded lines over [$__range]")
	}
	groups := byRe.FindAllStringSubmatch(q.expr, -1)
	if len(groups) == 0 {
		return append(problems, "the panel groups by nothing")
	}
	labels := strings.Split(strings.ReplaceAll(groups[0][1], " ", ""), ",")
	for _, want := range []string{"connection", "cause"} {
		if !slices.Contains(labels, want) {
			problems = append(problems, "the panel groups by ("+groups[0][1]+"), which folds every "+want+" together")
		}
	}
	return problems
}

func TestDashboard_scout_health_counts_degraded_scans_by_connection_and_cause(t *testing.T) {
	p := panelTitled(t, dashboardPanels(loadDashboard(t)), degradedInRange)
	for _, problem := range checkDegradedInRange(&p) {
		t.Errorf("panel %q: %s", p.title, problem)
	}
	read := "count_over_time({container=\"forge-scout\"} |= `scan degraded` | json msg=\"msg\", connection=\"connection\", cause=\"cause\" | msg=`scan degraded` "
	bad := map[string]dashQuery{
		"per day":        {expr: "sum by (connection, cause) (" + read + "[$__interval]))"},
		"no connection":  {expr: "sum by (cause) (" + read + "[$__range]))", instant: true},
		"no cause":       {expr: "sum by (connection) (" + read + "[$__range]))", instant: true},
		"range of steps": {expr: "sum by (connection, cause) (" + read + "[$__range]))"},
	}
	for name, q := range bad {
		t.Run(name, func(t *testing.T) {
			if problems := checkDegradedInRange(&dashPanel{queries: []dashQuery{q}}); len(problems) == 0 {
				t.Errorf("checkDegradedInRange(%s) reported nothing, want a violation", name)
			}
		})
	}
}

// failedReads is the Scout health chart of the signals each scan failed.
const failedReads = "Failed reads per day by signal"

// signalFilterRe captures a query's regex filter on failed_signals.
var signalFilterRe = regexp.MustCompile("\\|\\s*failed_signals\\s*=~\\s*`([^`]*)`")

// checkFailedReads applies the per-signal rule: failed_signals joins every
// signal a scan failed with commas, so each query reads one signal as a whole
// member of that list, and a scan that failed two signals counts under both.
func checkFailedReads(p *dashPanel) []string {
	var problems []string
	type filter struct {
		match func(string) bool
		re    string
	}
	var filters []filter
	for _, q := range p.queries {
		grouped := slices.ContainsFunc(byRe.FindAllStringSubmatch(q.expr, -1), func(m []string) bool { return strings.Contains(m[1], "failed_signals") })
		if grouped || strings.Contains(q.expr, ".failed_signals") {
			problems = append(problems, "a query groups by the whole failed_signals list, so a scan that failed two signals counts under the pair")
		}
		found := signalFilterRe.FindAllStringSubmatch(q.expr, -1)
		if len(found) != 1 {
			problems = append(problems, "a query does not filter failed_signals with exactly one regex")
			continue
		}
		match, ok := lokiLabelRegex(found[0][1])
		if !ok {
			problems = append(problems, "the failed_signals regex "+found[0][1]+" does not parse")
			continue
		}
		filters = append(filters, filter{match, found[0][1]})
	}
	var names []string
	for _, n := range familyNames {
		if n != "" {
			names = append(names, n)
		}
	}
	for i, n := range names {
		var counting []string
		for _, f := range filters {
			if f.match(n) {
				counting = append(counting, f.re)
			}
		}
		if len(counting) != 1 {
			problems = append(problems, fmt.Sprintf("the signal %s is counted by %d queries %q, want 1", n, len(counting), counting))
		}
		next, prev := names[(i+1)%len(names)], names[(i+len(names)-1)%len(names)]
		for _, value := range []string{n + "," + next, prev + "," + n, prev + "," + n + "," + next} {
			members := strings.Split(value, ",")
			for _, f := range filters {
				if f.match(value) != slices.ContainsFunc(members, f.match) {
					problems = append(problems, "the filter "+f.re+" reads failed_signals "+value+" unlike its members, so a scan does not count under each signal it failed")
				}
			}
		}
	}
	return problems
}

func TestDashboard_failed_reads_count_each_signal_a_scan_failed(t *testing.T) {
	p := panelTitled(t, dashboardPanels(loadDashboard(t)), failedReads)
	for _, problem := range checkFailedReads(&p) {
		t.Errorf("panel %q: %s", p.title, problem)
	}
	read := "count_over_time({container=\"forge-scout\"} |= `scan complete` | json msg=\"msg\", degraded=\"degraded\", failed_signals=\"failed_signals\" | msg=`scan complete` | degraded=`true` "
	perSignal := func(filter func(string) string) []dashQuery {
		var qs []dashQuery
		for _, n := range familyNames {
			if n != "" {
				qs = append(qs, dashQuery{expr: "sum by (signal) (" + read + "| failed_signals=~`" + filter(n) + "` | label_format signal=`" + n + "` [$__interval]))"})
			}
		}
		return qs
	}
	member := perSignal(func(n string) string { return "(.*,)?" + n + "(,.*)?" })
	bad := map[string][]dashQuery{
		"the whole list":         {{expr: "sum by (signals) (" + read + "| label_format signals=`{{ .failed_signals }}` [$__interval]))"}},
		"grouped by the list":    {{expr: "sum by (failed_signals) (" + read + "[$__interval]))"}},
		"one signal only":        perSignal(func(n string) string { return n }),
		"a missing signal":       member[1:],
		"a signal counted twice": append(member[:1:1], member...),
	}
	for name, qs := range bad {
		t.Run(name, func(t *testing.T) {
			if problems := checkFailedReads(&dashPanel{queries: qs}); len(problems) == 0 {
				t.Errorf("checkFailedReads(%s) reported nothing, want a violation", name)
			}
		})
	}
	if problems := checkFailedReads(&dashPanel{queries: member}); len(problems) != 0 {
		t.Errorf("checkFailedReads(a query per signal matching a whole member) = %v, want no violation", problems)
	}
}

// signalLabelRe captures the signal a failed reads query counts a line under.
var signalLabelRe = regexp.MustCompile("label_format signal=`([a-z_]+)`")

// countedSignals is the signals the queries of p count a msg line with
// attrs under, each query's filters evaluated as Loki does up to its first
// label_format. The forge filter is left out: its All value matches every
// forge.
func countedSignals(t *testing.T, p *dashPanel, msg string, attrs map[string]string) []string {
	t.Helper()
	line := maps.Clone(attrs)
	line["msg"] = msg
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	var out []string
	for _, q := range p.queries {
		m := pipelineRe.FindStringSubmatch(timeMacroRe.ReplaceAllString(q.expr, "0"))
		if m == nil {
			t.Fatalf("Setup: panel %q query has no log pipeline: %s", p.title, q.expr)
		}
		filters, _, _ := strings.Cut(strings.Replace(m[1], `| forge=~"$forge"`, "", 1), "| label_format")
		stages, err := parseLogQLPipeline(filters)
		if err != nil {
			t.Fatalf("Setup: panel %q query: %v", p.title, err)
		}
		if selectsLine(stages, string(raw)) {
			signal := "an unnamed signal"
			if s := signalLabelRe.FindStringSubmatch(q.expr); s != nil {
				signal = s[1]
			}
			out = append(out, signal)
		}
	}
	return out
}

func TestDashboard_failed_reads_count_a_stopped_scans_failed_signal(t *testing.T) {
	p := panelTitled(t, dashboardPanels(loadDashboard(t)), failedReads)
	limited := githubEndpoint("gh")
	limited.conn.errs = map[string]error{"issues o": forge.ErrRateLimited}
	h := newHarness(t, limited)
	h.scan(t)
	stopped := lineFor(t, h.rec, "scan stopped", "gh")
	if stopped["reason"] != "rate_limited" || stopped["failed_signals"] != "open_issues" {
		t.Errorf("scan stopped reason, failed_signals = %q, %q, want rate_limited, open_issues", stopped["reason"], stopped["failed_signals"])
	}
	if got := countedSignals(t, &p, "scan stopped", stopped); !slices.Equal(got, []string{"open_issues"}) {
		t.Errorf("panel %q counts the rate-limited stop under %v, want [open_issues]", p.title, got)
	}
	deferred := githubEndpoint("gh")
	deferred.conn.errs = map[string]error{"issues o": forge.ErrReadDeferred}
	h = newHarness(t, deferred)
	h.scan(t)
	if got := countedSignals(t, &p, "scan stopped", lineFor(t, h.rec, "scan stopped", "gh")); len(got) != 0 {
		t.Errorf("panel %q counts a budget stop after whole reads under %v, want nothing", p.title, got)
	}
}

// utcDayRe names each line's UTC day from its timestamp. Loki's date
// function formats in the server's time zone, so it is not used for a day.
var utcDayRe = regexp.MustCompile("label_format day=`\\{\\{ \\(__timestamp__\\)\\.UTC\\.Format \"2006-01-02\" \\}\\}`")

// convertsDay reports a transform that turns the day label into a time.
func convertsDay(p *dashPanel) bool {
	for _, tr := range p.transforms {
		if tr["group"] != "convertFieldType" {
			continue
		}
		conversions, _ := tr["conversions"].([]any)
		for _, c := range conversions {
			if field(c, "targetField") == "day" && field(c, "destinationType") == "time" && field(c, "dateFormat") == "YYYY-MM-DD" {
				return true
			}
		}
	}
	return false
}

// checkDayBars applies the day rule of a chart per day: each point stands at
// the UTC midnight that starts the day it counts, and a bar starts there, so
// the current day draws inside a range that ends now. A range query over
// [$__interval] stamps each day at its end, past the range for today.
func checkDayBars(p *dashPanel) []string {
	if field(p.el, "spec", "data", "spec", "queryOptions", "interval") != "1d" {
		return nil
	}
	var problems []string
	custom := field(p.el, "spec", "vizConfig", "spec", "fieldConfig", "defaults", "custom")
	if field(custom, "drawStyle") == "bars" && field(custom, "barAlignment") != 1.0 {
		problems = append(problems, "a bar does not start at its point, so it covers the day before the one it counts")
	}
	if !convertsDay(p) || !slices.ContainsFunc(p.transforms, func(tr map[string]any) bool { return tr["group"] == "prepareTimeSeries" }) {
		problems = append(problems, "the day label does not become the time of each point")
	}
	for _, q := range p.queries {
		if !q.instant || strings.Contains(q.expr, "[$__interval]") {
			problems = append(problems, "a query is not one instant read of the range, so Loki stamps each day at its end")
		}
		if !slices.ContainsFunc(byRe.FindAllStringSubmatch(q.expr, -1), func(m []string) bool {
			return slices.Contains(strings.Split(strings.ReplaceAll(strings.ReplaceAll(m[1], "$split", ""), " ", ""), ","), "day")
		}) {
			problems = append(problems, "a query does not group by day")
		}
		if !strings.Contains(q.expr, "msg=`"+dayMessage+"`") && !utcDayRe.MatchString(q.expr) {
			problems = append(problems, "a query of event lines does not name each line's UTC day from its timestamp")
		}
		if len(dayRangeRe.FindAllString(q.expr, -1)) != len(pipelineRe.FindAllString(timeMacroRe.ReplaceAllString(q.expr, "0"), -1)) {
			problems = append(problems, "a read does not keep only the days whose UTC midnight is inside the range")
		}
	}
	return problems
}

func TestDashboard_day_charts_stand_each_point_on_the_day_it_counts(t *testing.T) {
	var checked []string
	for _, p := range dashboardPanels(loadDashboard(t)) {
		if field(p.el, "spec", "data", "spec", "queryOptions", "interval") != "1d" {
			continue
		}
		checked = append(checked, p.title)
		for _, problem := range checkDayBars(&p) {
			t.Errorf("panel %q: %s", p.title, problem)
		}
	}
	for _, title := range []string{"Runs created per day by result", "Stopped scans per day by reason", failedReads} {
		if !slices.Contains(checked, title) {
			t.Errorf("Setup: %q is not a chart per day, so the rule never reads it", title)
		}
	}
	const inRange = "| label_format daystart=`{{ toDateInZone \"2006-01-02\" \"UTC\" .day | unixEpoch }}` | daystart >= ${__from:date:seconds} "
	stops := "{container=\"forge-scout\"} |= `scan stopped` | json msg=\"msg\", reason=\"reason\" | msg=`scan stopped` "
	utcDay := "| label_format day=`{{ (__timestamp__).UTC.Format \"2006-01-02\" }}` "
	dayTransforms := []map[string]any{
		{"group": "convertFieldType", "conversions": []any{map[string]any{"targetField": "day", "destinationType": "time", "dateFormat": "YYYY-MM-DD"}}},
		{"group": "prepareTimeSeries", "format": "multi"},
	}
	chart := func(expr string, instant bool, align float64, transforms []map[string]any) dashPanel {
		el := map[string]any{"spec": map[string]any{
			"data":      map[string]any{"spec": map[string]any{"queryOptions": map[string]any{"interval": "1d"}}},
			"vizConfig": map[string]any{"spec": map[string]any{"fieldConfig": map[string]any{"defaults": map[string]any{"custom": map[string]any{"drawStyle": "bars", "barAlignment": align}}}}},
		}}
		return dashPanel{title: "Stopped scans per day", el: el, transforms: transforms, queries: []dashQuery{{expr: expr, instant: instant}}}
	}
	bad := map[string]dashPanel{
		"bucket stamped at its end": chart("sum by (reason) (count_over_time("+stops+"[$__interval]))", false, -1, nil),
		"bar ending at its point":   chart("sum by (day, reason) (count_over_time("+stops+utcDay+inRange+"[$__range]))", true, -1, dayTransforms),
		"day in the server's zone":  chart("sum by (day, reason) (count_over_time("+stops+"| label_format day=`{{ date \"2006-01-02\" __timestamp__ }}` "+inRange+"[$__range]))", true, 1, dayTransforms),
		"no day grouping":           chart("sum by (reason) (count_over_time("+stops+utcDay+inRange+"[$__range]))", true, 1, dayTransforms),
		"range start day kept":      chart("sum by (day, reason) (count_over_time("+stops+utcDay+"[$__range]))", true, 1, dayTransforms),
		"day never a time":          chart("sum by (day, reason) (count_over_time("+stops+utcDay+inRange+"[$__range]))", true, 1, nil),
	}
	for name, p := range bad {
		t.Run(name, func(t *testing.T) {
			if problems := checkDayBars(&p); len(problems) == 0 {
				t.Errorf("checkDayBars(%s) reported nothing, want a violation", name)
			}
		})
	}
	good := chart("sum by (day, reason) (count_over_time("+stops+utcDay+inRange+"[$__range]))", true, 1, dayTransforms)
	if problems := checkDayBars(&good); len(problems) != 0 {
		t.Errorf("checkDayBars(each stop counted on its UTC day) = %v, want no violation", problems)
	}
}

// lastScanRe is a range aggregation that ends in each connection's newest
// line of the selected range.
var lastScanRe = regexp.MustCompile(`\[\$__range\]\) by \(connection(, forge)?\)`)

// checkLastScan applies the one-line rule of Last scan per connection: every
// column reads each connection's newest scan complete or scan stopped line in
// the range, and a field that line lacks reads NaN rather than an older
// scan's value, so a row never mixes two scans.
func checkLastScan(p *dashPanel) []string {
	var problems []string
	read := map[string]bool{}
	for _, q := range p.queries {
		expr := q.expr
		if !q.instant {
			problems = append(problems, "a column is a range query, so the row shows a step rather than the newest scan")
		}
		if !strings.Contains(expr, scanEnds) || strings.Contains(expr, "scan degraded") {
			problems = append(problems, "a column reads other lines than scan complete and scan stopped: "+expr)
		}
		aggs := overTimeRe.FindAllStringSubmatch(expr, -1)
		for _, m := range aggs {
			if m[1] != "last" {
				problems = append(problems, "a column takes "+m[1]+"_over_time, not the newest line: "+expr)
			}
		}
		if len(aggs) == 0 || len(lastScanRe.FindAllString(expr, -1)) != len(aggs) {
			problems = append(problems, "a column does not take each connection's newest line over [$__range]: "+expr)
		}
		for _, m := range unwrapRe.FindAllStringSubmatch(expr, -1) {
			lf := regexp.MustCompile(`\|\s*label_format\s+` + m[1] + "=`([^`]*)`").FindStringSubmatch(expr)
			if lf == nil || !strings.Contains(lf[1], "NaN") {
				problems = append(problems, "a column unwraps "+m[1]+" with no NaN where the newest line lacks it, so it reaches back to an older scan")
			}
		}
		for k := range jsonKeys(expr) {
			read[k] = true
		}
	}
	for _, want := range []string{"scan_id", "repos_discovered", "skipped", "budget_remaining", "degraded"} {
		if !read[want] {
			problems = append(problems, "no column reads "+want)
		}
	}
	return problems
}

func TestDashboard_last_scan_per_connection_reads_one_scan_line(t *testing.T) {
	p := panelTitled(t, dashboardPanels(loadDashboard(t)), "Last scan per connection")
	for _, problem := range checkLastScan(&p) {
		t.Errorf("panel %q: %s", p.title, problem)
	}
	const sel = `{container="forge-scout"}`
	verdict := sel + " |= `scan ` | json msg=\"msg\", connection=\"connection\", "
	good := []dashQuery{
		{instant: true, expr: "last_over_time(" + verdict + "scan_id=\"scan_id\" | " + scanEnds + " | label_format scan_id=`{{ if .scan_id }}{{ .scan_id }}{{ else }}NaN{{ end }}` | unwrap scan_id [$__range]) by (connection)"},
		{instant: true, expr: "last_over_time(" + verdict + "repos_discovered=\"repos_discovered\", skipped=\"skipped\", budget_remaining=\"budget_remaining\", degraded=\"degraded\" | " + scanEnds + " | label_format repos_discovered=`{{ if .repos_discovered }}{{ .repos_discovered }}{{ else }}NaN{{ end }}` | unwrap repos_discovered [$__range]) by (connection)"},
	}
	bad := map[string]dashQuery{
		"scan complete only":         {instant: true, expr: "last_over_time(" + sel + " |= `scan complete` | json msg=\"msg\", connection=\"connection\", repos_discovered=\"repos_discovered\" | msg=`scan complete` | unwrap repos_discovered | __error__=\"\" [$__range]) by (connection)"},
		"no NaN for an absent field": {instant: true, expr: "last_over_time(" + verdict + "repos_discovered=\"repos_discovered\" | " + scanEnds + " | unwrap repos_discovered | __error__=\"\" [$__range]) by (connection)"},
		"degraded scans counted":     {instant: true, expr: "sum by (connection, forge) (count_over_time(" + sel + " |= `scan degraded` | json msg=\"msg\", connection=\"connection\" | msg=`scan degraded` [$__range]))"},
		"largest scan id":            {instant: true, expr: "max_over_time(" + verdict + "scan_id=\"scan_id\" | " + scanEnds + " | label_format scan_id=`{{ if .scan_id }}{{ .scan_id }}{{ else }}NaN{{ end }}` | unwrap scan_id [$__range]) by (connection)"},
		"range query":                {expr: good[0].expr},
	}
	if problems := checkLastScan(&dashPanel{queries: good}); len(problems) != 0 {
		t.Errorf("checkLastScan(one line per connection) = %v, want no problem", problems)
	}
	for name, q := range bad {
		t.Run(name, func(t *testing.T) {
			if problems := checkLastScan(&dashPanel{queries: append([]dashQuery{q}, good...)}); len(problems) == 0 {
				t.Errorf("checkLastScan(%s) reported nothing, want a violation", name)
			}
		})
	}
}
