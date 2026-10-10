package collect

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/config"
)

// monitoringDoc is the operator page that documents the log contract.
const monitoringDoc = "../../docs/monitoring.md"

var backticked = regexp.MustCompile("`([a-z0-9_]+)`")

// docSection is the lines of the page from the first that starts with lead
// to the blank line that ends its block, or the block after it when lead is
// a paragraph of its own.
func docSection(t *testing.T, page, lead string) []string {
	t.Helper()
	lines := strings.Split(page, "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, lead) })
	if start < 0 {
		t.Fatalf("%s has no line starting %q", monitoringDoc, lead)
	}
	blockEnd := func(from int) int {
		for from < len(lines) && lines[from] != "" {
			from++
		}
		return from
	}
	end := blockEnd(start + 1)
	if end == start+1 {
		end = blockEnd(end + 1)
	}
	return lines[start:end]
}

// names is every backticked name of lines, sorted and once each.
func names(lines []string, keep func(string) bool) []string {
	var out []string
	for _, l := range lines {
		for _, m := range backticked.FindAllStringSubmatch(l, -1) {
			if keep(m[1]) && !slices.Contains(out, m[1]) {
				out = append(out, m[1])
			}
		}
	}
	slices.Sort(out)
	return out
}

func readMonitoringDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(monitoringDoc)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	return string(raw)
}

func TestMonitoringDoc_cause_table_is_every_cause_scan_degraded_names(t *testing.T) {
	rows := docSection(t, readMonitoringDoc(t), "| `cause` | Meaning |")
	var documented []string
	for _, row := range rows[2:] {
		if m := backticked.FindStringSubmatch(row); m != nil {
			documented = append(documented, m[1])
		}
	}
	slices.Sort(documented)
	var coded []string
	for i := range diagnoses {
		coded = append(coded, diagnoses[i].cause)
	}
	slices.Sort(coded)
	if !slices.Equal(documented, coded) {
		t.Errorf("%s cause table = %v, want the causes scan degraded names, %v", monitoringDoc, documented, coded)
	}
}

func TestMonitoringDoc_read_fields_are_every_family_scan_complete_reports(t *testing.T) {
	lines := docSection(t, readMonitoringDoc(t), "- The read state of each signal:")
	documented := names(lines[:1], func(n string) bool { return strings.HasSuffix(n, "_read") })
	var coded []string
	for f := range families {
		if f != famState {
			coded = append(coded, familyNames[f]+"_read")
		}
	}
	slices.Sort(coded)
	if !slices.Equal(documented, coded) {
		t.Errorf("%s read fields = %v, want one per signal, %v", monitoringDoc, documented, coded)
	}
}

func TestMonitoringDoc_scan_complete_fields_are_the_keys_a_scan_logs(t *testing.T) {
	lines := docSection(t, readMonitoringDoc(t), "When a connection's repositories are listed, forge-scout logs `scanning`")
	var bullets []string
	for _, l := range lines {
		if strings.HasPrefix(l, "- ") {
			bullets = append(bullets, l)
		}
	}
	documented := names(append(bullets, "`scan_id`"), func(string) bool { return true })
	h := newHarness(t, readStateEndpoint())
	h.scan(t)
	var logged []string
	for k := range lineFor(t, h.rec, "scan complete", "gh") {
		if k != "level" && k != "forge" && k != "connection" {
			logged = append(logged, k)
		}
	}
	slices.Sort(logged)
	if !slices.Equal(documented, logged) {
		t.Errorf("%s scan complete fields = %v\nwant the keys a GitHub scan logs, %v", monitoringDoc, documented, logged)
	}
}

func TestMonitoringDoc_names_the_value_bound_and_the_truncated_field(t *testing.T) {
	lines := docSection(t, readMonitoringDoc(t), "Each value a line takes from the forge")
	want := strconv.Itoa(maxValueBytes >> 10)
	sizes := regexp.MustCompile(`(\d+) KiB`).FindAllStringSubmatch(lines[0], -1)
	bounds := 0
	for _, m := range sizes {
		switch m[1] {
		case strconv.Itoa(lokiLineLimit >> 10):
		case want:
			bounds++
		default:
			t.Errorf("%s value bound paragraph names %s KiB, want the %s KiB bound", monitoringDoc, m[1], want)
		}
	}
	if bounds == 0 || !strings.Contains(lines[0], "`"+cutMark+"`") || !strings.Contains(lines[0], "`truncated`") {
		t.Errorf("%s value bound paragraph = %q, want it to name %s KiB, the %q mark and `truncated`", monitoringDoc, lines[0], want, cutMark)
	}
}

var (
	msgCell  = regexp.MustCompile("^ `([a-z][a-z ]*)` $")
	otherRow = regexp.MustCompile("^ the `([a-z][a-z ]*)` fields $")
)

// rowKeys are the fields every line of the message table carries beside the
// ones its row names: the handler's, the scope's and the snapshot's.
var rowKeys = []string{"level", "forge", "connection", "repo", "scan_id", "rank", "forge_rank"}

func TestMonitoringDoc_message_table_names_the_fields_each_line_carries(t *testing.T) {
	rows := docSection(t, readMonitoringDoc(t), "| `msg` | Logged | Other fields |")
	documented := map[string][]string{}
	var order []string
	for _, row := range rows[2:] {
		cells := strings.Split(row, "|")
		msg := msgCell.FindStringSubmatch(cells[1])
		if msg == nil || len(cells) < 4 {
			t.Fatalf("Setup: %s message row %q has no msg or no field cell", monitoringDoc, row)
		}
		fields := names([]string{cells[3]}, func(string) bool { return true })
		// "the `security alert` fields" names another row's fields.
		if other := otherRow.FindStringSubmatch(cells[3]); other != nil {
			fields = slices.Clone(documented[other[1]])
		}
		documented[msg[1]] = fields
		order = append(order, msg[1])
	}
	logged := recordContract(t)
	for _, msg := range order {
		var keys []string
		for k := range logged[msg] {
			if !slices.Contains(rowKeys, k) {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		if len(logged[msg]) == 0 {
			t.Errorf("%s documents %q, which the recorded scans never log", monitoringDoc, msg)
			continue
		}
		if !slices.Equal(documented[msg], keys) {
			t.Errorf("%s %q fields = %v\nwant the keys its lines carry, %v", monitoringDoc, msg, documented[msg], keys)
		}
	}
}

// nanText is the text the dashboard's count tiles show for a withheld count.
func nanText(t *testing.T) string {
	t.Helper()
	for _, p := range dashboardPanels(loadDashboard(t)) {
		mappings, _ := field(p.el, "spec", "vizConfig", "spec", "fieldConfig", "defaults", "mappings").([]any)
		for _, m := range mappings {
			if field(m, "options", "match") == "nan" {
				text, _ := field(m, "options", "result", "text").(string)
				return text
			}
		}
	}
	t.Fatalf("Setup: %s maps no NaN", dashboardPath)
	return ""
}

// checkDegradedAlertText reports where the degraded alert's description and
// note say a withheld count reads 0, which the tiles never show.
func checkDegradedAlertText(description, note, tileText string) []string {
	var problems []string
	if strings.Contains(description, "unchecked") || !strings.Contains(description, "withheld") || !strings.Contains(description, "never shown as 0") {
		problems = append(problems, "the alert description does not say a count not read whole is withheld, never shown as 0")
	}
	if strings.Contains(note, "may not have been checked") || !strings.Contains(note, `"`+tileText+`"`) {
		problems = append(problems, "the note does not name the tile text "+tileText)
	}
	return problems
}

func TestMonitoringDoc_degraded_alert_says_a_withheld_count_is_never_0(t *testing.T) {
	page := readMonitoringDoc(t)
	start := strings.Index(page, "- alert: ForgeScoutScanDegraded")
	end := strings.Index(page, "- alert: ForgeScoutScanStalled")
	if start < 0 || end < start {
		t.Fatalf("Setup: %s has no ForgeScoutScanDegraded rule before ForgeScoutScanStalled", monitoringDoc)
	}
	description := strings.Join(strings.Fields(page[start:end]), " ")
	note := docSection(t, page, "- `ForgeScoutScanDegraded` means")[0]
	tile := nanText(t)
	for _, problem := range checkDegradedAlertText(description, note, tile) {
		t.Errorf("%s: %s", monitoringDoc, problem)
	}
	old := checkDegradedAlertText("so its counts may read 0 unchecked.", "means a zero on the dashboard for that connection may not have been checked.", tile)
	if len(old) != 2 {
		t.Errorf("checkDegradedAlertText(an unchecked zero) = %v, want two problems", old)
	}
}

func TestMonitoringDoc_day_counts_name_the_day_the_runs_were_created(t *testing.T) {
	page := readMonitoringDoc(t)
	for _, lead := range []string{"A `ci day` line counts", "The run charts read the `ci day` lines"} {
		text := docSection(t, page, lead)[0]
		if problems := checkDayText("", text); len(problems) != 0 {
			t.Errorf("%s paragraph %q: %v", monitoringDoc, lead, problems)
		}
	}
}

// labelMatch is one predicate of a label filter on an extracted label.
type labelMatch struct{ label, op, value string }

// logqlStage is one stage of a LogQL log pipeline: a line filter (|=, !=,
// |~, !~), a json stage, or a label filter, whose predicates anyOf are
// joined by or.
type logqlStage struct {
	params    map[string]string
	op, value string
	anyOf     []labelMatch
}

const labelPredicate = "([A-Za-z_][A-Za-z0-9_]*)\\s*(=~|!~|!=|=)\\s*(`[^`]*`|\"(?:[^\"\\\\]|\\\\.)*\")"

var (
	lineStageRe  = regexp.MustCompile("^\\s*(\\|=|!=|\\|~|!~)\\s*(`[^`]*`|\"(?:[^\"\\\\]|\\\\.)*\")")
	jsonDocRe    = regexp.MustCompile(`^\s*\|\s*json\b((?:\s*,?\s*[A-Za-z_][A-Za-z0-9_]*(?:\s*=\s*"[^"]*")?)*)`)
	jsonDocParam = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)(?:\s*=\s*"([^"]*)")?`)
	labelStageRe = regexp.MustCompile(`^\s*\|\s*` + labelPredicate)
	labelOrRe    = regexp.MustCompile(`^\s+or\s+` + labelPredicate)
)

func unquoteLogQL(s string) string {
	if strings.HasPrefix(s, "`") {
		return strings.Trim(s, "`")
	}
	v, err := strconv.Unquote(s)
	if err != nil {
		return s
	}
	return v
}

// parseLogQLPipeline reads the stages after a stream selector, or fails on
// any stage outside the four kinds above.
func parseLogQLPipeline(stages string) ([]logqlStage, error) {
	var out []logqlStage
	for rest := stages; strings.TrimSpace(rest) != ""; {
		if m := lineStageRe.FindStringSubmatch(rest); m != nil {
			out = append(out, logqlStage{op: m[1], value: unquoteLogQL(m[2])})
			rest = rest[len(m[0]):]
			continue
		}
		if m := labelStageRe.FindStringSubmatch(rest); m != nil && m[1] != "json" {
			stage := logqlStage{op: "label"}
			for ; m != nil; m = labelOrRe.FindStringSubmatch(rest) {
				stage.anyOf = append(stage.anyOf, labelMatch{label: m[1], op: m[2], value: unquoteLogQL(m[3])})
				rest = rest[len(m[0]):]
			}
			out = append(out, stage)
			continue
		}
		if m := jsonDocRe.FindStringSubmatch(rest); m != nil {
			params := map[string]string{}
			for _, p := range jsonDocParam.FindAllStringSubmatch(m[1], -1) {
				params[p[1]] = cmp.Or(p[2], p[1])
			}
			out = append(out, logqlStage{op: "json", params: params})
			rest = rest[len(m[0]):]
			continue
		}
		return nil, fmt.Errorf("unknown stage at %q", rest)
	}
	return out, nil
}

// selectsLine reports whether a JSON log line passes every stage, as Loki
// evaluates them: line filters on the raw line, a json stage extracting the
// named top-level keys (all of them when it names none), and label filters,
// each predicate matched as labelMatch.holds states.
func selectsLine(stages []logqlStage, line string) bool {
	labels := map[string]string{}
	for _, s := range stages {
		switch s.op {
		case "|=", "!=":
			if strings.Contains(line, s.value) != (s.op == "|=") {
				return false
			}
		case "|~", "!~":
			if regexp.MustCompile(s.value).MatchString(line) != (s.op == "|~") {
				return false
			}
		case "json":
			var fields map[string]any
			if json.Unmarshal([]byte(line), &fields) != nil {
				return false
			}
			for k, v := range fields {
				if len(s.params) == 0 {
					labels[k] = fmt.Sprint(v)
				}
			}
			for name, key := range s.params {
				if v, ok := fields[key]; ok {
					labels[name] = fmt.Sprint(v)
				}
			}
		case "label":
			if !slices.ContainsFunc(s.anyOf, func(m labelMatch) bool { return m.holds(labels[m.label]) }) {
				return false
			}
		}
	}
	return true
}

// holds reports whether the predicate holds for the label value v, as Loki
// 3.7.8 matches a label (lokiLabelRegex).
func (m labelMatch) holds(v string) bool {
	switch m.op {
	case "=", "!=":
		return (v == m.value) == (m.op == "=")
	}
	match, ok := lokiLabelRegex(m.value)
	return ok && match(v) == (m.op == "=~")
}

// lokiLabelRegex is Loki 3.7.8's label regex match, ported from
// pkg/logql/log/filter.go (parseRegexpFilter and RegexSimplifier) and pinned
// by TestLabelMatch_holds_as_Loki_matches_a_label: a regex the simplifier
// rewrites becomes equality and substring tests, and any other is matched
// against the whole value.
func lokiLabelRegex(expr string) (func(string) bool, bool) {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return nil, false
	}
	re = re.Simplify()
	if f, ok := lokiSimplify(re, true); ok {
		return f, true
	}
	return regexp.MustCompile(`^(?:` + re.String() + `)$`).MatchString, true
}

func lokiSimplify(re *syntax.Regexp, label bool) (func(string) bool, bool) {
	switch re.Op {
	case syntax.OpAlternate:
		var legs []func(string) bool
		for _, sub := range re.Sub {
			f, ok := lokiSimplify(uncaptured(sub), label)
			if !ok {
				return nil, false
			}
			legs = append(legs, f)
		}
		return anyOf(legs), true
	case syntax.OpConcat:
		return lokiConcat(re, "", false)
	case syntax.OpCapture:
		return lokiSimplify(uncaptured(re), label)
	case syntax.OpLiteral:
		if label {
			return equalTo(string(re.Rune), folds(re)), true
		}
		return containing(string(re.Rune), folds(re)), true
	case syntax.OpStar:
		if re.Sub[0].Op == syntax.OpAnyCharNotNL {
			return func(string) bool { return true }, true
		}
	case syntax.OpPlus:
		if re.Sub[0].Op == syntax.OpAnyCharNotNL {
			return func(v string) bool { return v != "" }, true
		}
	case syntax.OpEmptyMatch:
		return func(string) bool { return true }, true
	}
	return nil, false
}

// lokiConcat is simplifyConcat: at most three parts, one literal, .* parts
// skipped, and an alternation after the literal joined onto it; base is the
// literal an enclosing alternation carries in.
func lokiConcat(re *syntax.Regexp, base string, hasBase bool) (func(string) bool, bool) {
	var subs []*syntax.Regexp
	for _, sub := range re.Sub {
		if sub = uncaptured(sub); sub.Op != syntax.OpEmptyMatch {
			subs = append(subs, sub)
		}
	}
	if len(subs) > 3 {
		return nil, false
	}
	var legs []func(string) bool
	literals, fold := 0, false
	for _, sub := range subs {
		switch {
		case sub.Op == syntax.OpLiteral:
			if literals++; literals > 1 {
				return nil, false
			}
			base, hasBase, fold = base+string(sub.Rune), true, folds(sub)
		case sub.Op == syntax.OpAlternate && hasBase:
			alts, ok := lokiConcatAlternate(sub, base, fold)
			if !ok {
				return nil, false
			}
			legs = append(legs, alts...)
		case sub.Op == syntax.OpStar && sub.Sub[0].Op == syntax.OpAnyCharNotNL:
		default:
			return nil, false
		}
	}
	switch {
	case legs != nil:
		return anyOf(legs), true
	case hasBase:
		return containing(base, fold), true
	}
	return nil, false
}

// lokiConcatAlternate is simplifyConcatAlternate: each branch of alt joined
// onto the literal before it, as a substring test.
func lokiConcatAlternate(alt *syntax.Regexp, base string, fold bool) ([]func(string) bool, bool) {
	var legs []func(string) bool
	for _, sub := range alt.Sub {
		if !fold && folds(sub) {
			return nil, false
		}
		switch {
		case sub.Op == syntax.OpEmptyMatch, sub.Op == syntax.OpStar && sub.Sub[0].Op == syntax.OpAnyCharNotNL:
			legs = append(legs, containing(base, fold))
		case sub.Op == syntax.OpLiteral:
			legs = append(legs, containing(base+string(sub.Rune), fold))
		case sub.Op == syntax.OpConcat:
			f, ok := lokiConcat(sub, base, true)
			if !ok {
				return nil, false
			}
			legs = append(legs, f)
		default:
			return nil, false
		}
	}
	return legs, legs != nil
}

// uncaptured is util.ClearCapture: one capture group removed.
func uncaptured(re *syntax.Regexp) *syntax.Regexp {
	if re.Op == syntax.OpCapture {
		return re.Sub[0]
	}
	return re
}

func folds(re *syntax.Regexp) bool { return re.Flags&syntax.FoldCase != 0 }

func equalTo(lit string, fold bool) func(string) bool {
	if fold {
		return func(v string) bool { return strings.EqualFold(v, lit) }
	}
	return func(v string) bool { return v == lit }
}

func containing(lit string, fold bool) func(string) bool {
	if fold {
		return func(v string) bool { return strings.Contains(strings.ToLower(v), strings.ToLower(lit)) }
	}
	return func(v string) bool { return strings.Contains(v, lit) }
}

func anyOf(fs []func(string) bool) func(string) bool {
	return func(v string) bool {
		return slices.ContainsFunc(fs, func(f func(string) bool) bool { return f(v) })
	}
}

var stalledExprRe = regexp.MustCompile(`absent_over_time\(\{container="forge-scout"\}(.*) \[55m\]\)`)

// checkStalledSelector reports each line the stall rule's selector gets
// wrong: only a scan complete or scan stopped line proves a scan finished.
func checkStalledSelector(expr string) []string {
	m := stalledExprRe.FindStringSubmatch(expr)
	if m == nil {
		return []string{"the rule is not absent_over_time({container=\"forge-scout\"} ... [55m])"}
	}
	stages, err := parseLogQLPipeline(m[1])
	if err != nil {
		return []string{err.Error()}
	}
	lines := map[string]bool{
		`{"level":"INFO","msg":"scan complete","forge":"github","connection":"gh"}`:            true,
		`{"level":"WARN","msg":"scan stopped","forge":"github","reason":"read_reserve"}`:       true,
		`{"level":"INFO","msg":"ci run","workflow":"scan complete","branch":"scan stopped"}`:   false,
		`{"level":"ERROR","msg":"scan degraded","reason":"scan stopped before scan complete"}`: false,
		`{"level":"INFO","msg":"scan complete soon","forge":"github"}`:                         false,
		`{"level":"INFO","msg":"trigger scan complete","outcome":"complete"}`:                  false,
	}
	var problems []string
	for line, want := range lines {
		if got := selectsLine(stages, line); got != want {
			problems = append(problems, fmt.Sprintf("the selector selects %s = %v, want %v", line, got, want))
		}
	}
	slices.Sort(problems)
	return problems
}

// degradedRuleRe captures the degraded rule's window in minutes and the
// number of scan degraded lines it needs inside it.
var degradedRuleRe = regexp.MustCompile(`(?s)count_over_time\(.*\[(\d+)m\]\s*\)\)\s*>=\s*(\d+)`)

// degradedRuleResolves reports whether a rule needing need scan degraded
// lines in the last window resolves while every scan keeps ending degraded,
// gap apart: between the need-th ending and the next one.
func degradedRuleResolves(window, gap time.Duration, need int) bool {
	ends := make([]time.Duration, need+1)
	for i := range ends {
		ends[i] = time.Duration(i) * gap
	}
	for at := ends[need-1]; at < ends[need]; at += time.Second {
		n := 0
		for _, e := range ends {
			if e <= at && e > at-window {
				n++
			}
		}
		if n < need {
			return true
		}
	}
	return false
}

func TestMonitoringDoc_degraded_alert_stays_firing_across_the_widest_scan_gap(t *testing.T) {
	page := readMonitoringDoc(t)
	start := strings.Index(page, "- alert: ForgeScoutScanDegraded")
	end := strings.Index(page, "- alert: ForgeScoutScanStalled")
	if start < 0 || end < start {
		t.Fatalf("Setup: %s has no ForgeScoutScanDegraded rule before ForgeScoutScanStalled", monitoringDoc)
	}
	rule := page[start:end]
	m := degradedRuleRe.FindStringSubmatch(rule)
	if m == nil {
		t.Fatalf("Setup: %s ForgeScoutScanDegraded has no count_over_time(...[<n>m])) >= <n>", monitoringDoc)
	}
	minutes, _ := strconv.Atoi(m[1])
	need, _ := strconv.Atoi(m[2])
	window := time.Duration(minutes) * time.Minute
	// Two scan endings are at most the longest wait, the interval and its
	// jitter, plus a scan at its limit apart.
	interval := config.DefaultScanInterval
	gap := interval + interval*config.ScanJitterPercent/100 + config.ScanLimit(interval)
	if need < 1 || degradedRuleResolves(window, gap, need) {
		t.Errorf("%s ForgeScoutScanDegraded needs %d degraded scans in %v, so it resolves while every scan ends degraded %v apart", monitoringDoc, need, window, gap)
	}
	if text := strings.Join(strings.Fields(rule), " "); !strings.Contains(text, "in the last "+m[1]+"m") {
		t.Errorf("%s ForgeScoutScanDegraded description does not name its %sm window", monitoringDoc, m[1])
	}
	if !degradedRuleResolves(55*time.Minute, gap, 2) || degradedRuleResolves(100*time.Minute, gap, 2) {
		t.Error("degradedRuleResolves(55m, 100m) disagree with two endings 46.5m apart, want 55m to resolve and 100m to hold")
	}
}

func TestMonitoringDoc_stall_alert_counts_only_scan_complete_and_scan_stopped_lines(t *testing.T) {
	page := readMonitoringDoc(t)
	start := strings.Index(page, "- alert: ForgeScoutScanStalled")
	if start < 0 {
		t.Fatalf("Setup: %s has no ForgeScoutScanStalled rule", monitoringDoc)
	}
	block, _, _ := strings.Cut(page[start:], "for:")
	expr := strings.Join(strings.Fields(block), " ")
	for _, problem := range checkStalledSelector(expr) {
		t.Errorf("%s ForgeScoutScanStalled: %s", monitoringDoc, problem)
	}
	old := checkStalledSelector("absent_over_time({container=\"forge-scout\"} |~ `scan complete|scan stopped` [55m])")
	if len(old) != 4 {
		t.Errorf("checkStalledSelector(a raw-line regex) = %v, want the four lines that only quote the phrases", old)
	}
	regex := checkStalledSelector("absent_over_time({container=\"forge-scout\"} |= \"scan \" | json msg=\"msg\" | msg=~\"scan complete|scan stopped\" [55m])")
	if len(regex) != 2 {
		t.Errorf("checkStalledSelector(a msg regex) = %v, want the two messages that contain a phrase", regex)
	}
}

func TestLabelMatch_holds_as_Loki_matches_a_label(t *testing.T) {
	// Measured on Loki 3.7.8: each case's msg filter over one line per value,
	// and the values it selected. A line without msg is selected as "" is.
	values := []string{"scan complete", "scan completeness", "xscan complete", "trigger scan complete", "Scan Complete", "SCAN COMPLETE SOON", "scan stopped", "scan stopped short", "scanXcomplete", "unsupported", "unsupported_signals", "excluded", "partially excluded", "other", "another", "scan", ""}
	cases := []struct {
		op, re  string
		selects []string
	}{
		{"=~", `scan complete`, []string{"scan complete"}},
		{"=~", `(?i)scan complete`, []string{"Scan Complete", "scan complete"}},
		{"=~", `(scan complete)`, []string{"scan complete"}},
		{"=~", `scan complete|scan stopped`, []string{"scan complete", "scan completeness", "scan stopped", "scan stopped short", "trigger scan complete", "xscan complete"}},
		{"=~", `scan (complete|stopped)`, []string{"scan complete", "scan completeness", "scan stopped", "scan stopped short", "trigger scan complete", "xscan complete"}},
		{"=~", `unsupported|excluded`, []string{"excluded", "unsupported"}},
		{"=~", `scan complete|other`, []string{"other", "scan complete"}},
		{"=~", `scan complete|.*stopped`, []string{"scan complete", "scan stopped", "scan stopped short"}},
		{"=~", `scan|scan complete`, []string{"scan", "scan complete", "scan completeness", "scan stopped", "scan stopped short", "scanXcomplete", "trigger scan complete", "xscan complete"}},
		{"=~", `scan.*`, []string{"scan", "scan complete", "scan completeness", "scan stopped", "scan stopped short", "scanXcomplete", "trigger scan complete", "xscan complete"}},
		{"=~", `.*scan complete`, []string{"scan complete", "scan completeness", "trigger scan complete", "xscan complete"}},
		{"=~", `scan complete.*`, []string{"scan complete", "scan completeness", "trigger scan complete", "xscan complete"}},
		{"=~", `.*scan complete.*`, []string{"scan complete", "scan completeness", "trigger scan complete", "xscan complete"}},
		{"=~", `scan (complete|stopped).*`, []string{"scan complete", "scan completeness", "scan stopped", "scan stopped short", "trigger scan complete", "xscan complete"}},
		{"=~", `(?i)scan (complete|stopped)`, []string{"SCAN COMPLETE SOON", "Scan Complete", "scan complete", "scan completeness", "scan stopped", "scan stopped short", "trigger scan complete", "xscan complete"}},
		{"=~", `^(scan complete|scan stopped)$`, []string{"scan complete", "scan stopped"}},
		{"=~", `scan (complete|stopped)$`, []string{"scan complete", "scan stopped"}},
		{"=~", `^scan (complete|stopped)`, []string{"scan complete", "scan stopped"}},
		{"=~", `scan.complete`, []string{"scan complete", "scanXcomplete"}},
		{"=~", `.*scan.*complete`, []string{"scan complete", "scanXcomplete", "trigger scan complete", "xscan complete"}},
		{"=~", `.*`, []string{"", "SCAN COMPLETE SOON", "Scan Complete", "another", "excluded", "other", "partially excluded", "scan", "scan complete", "scan completeness", "scan stopped", "scan stopped short", "scanXcomplete", "trigger scan complete", "unsupported", "unsupported_signals", "xscan complete"}},
		{"=~", `.+`, []string{"SCAN COMPLETE SOON", "Scan Complete", "another", "excluded", "other", "partially excluded", "scan", "scan complete", "scan completeness", "scan stopped", "scan stopped short", "scanXcomplete", "trigger scan complete", "unsupported", "unsupported_signals", "xscan complete"}},
		{"!~", `unsupported|excluded`, []string{"", "SCAN COMPLETE SOON", "Scan Complete", "another", "other", "partially excluded", "scan", "scan complete", "scan completeness", "scan stopped", "scan stopped short", "scanXcomplete", "trigger scan complete", "unsupported_signals", "xscan complete"}},
		{"!~", `scan complete|scan stopped`, []string{"", "SCAN COMPLETE SOON", "Scan Complete", "another", "excluded", "other", "partially excluded", "scan", "scanXcomplete", "unsupported", "unsupported_signals"}},
		{"!~", `scan complete`, []string{"", "SCAN COMPLETE SOON", "Scan Complete", "another", "excluded", "other", "partially excluded", "scan", "scan completeness", "scan stopped", "scan stopped short", "scanXcomplete", "trigger scan complete", "unsupported", "unsupported_signals", "xscan complete"}},
		{"!~", `.+`, []string{""}},
	}
	for _, tc := range cases {
		m := labelMatch{label: "msg", op: tc.op, value: tc.re}
		for _, v := range values {
			if got, want := m.holds(v), slices.Contains(tc.selects, v); got != want {
				t.Errorf("msg%s%q holds for %q = %v, want %v", tc.op, tc.re, v, got, want)
			}
		}
	}
}
