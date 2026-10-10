package collect

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/forge"
)

// lokiLineLimit is Loki's default max_line_size, the entry a push refuses
// above.
const lokiLineLimit = 256 << 10

// adapterOwned are the row fields an adapter fills from a closed set, so a
// forge cannot make them long. Every other string a row carries is the
// forge's, and oversize feeds it.
var adapterOwned = map[string]bool{"Run.Trigger": true, "Alert.Source": true, "Workflow.State": true}

// sharedValue groups the fields that name one repository or branch, which
// the collector matches across rows.
var sharedValue = map[string]string{
	"Repo.Path": "repo", "Run.Repo": "repo", "PullRequest.Repo": "repo", "Issue.Repo": "repo",
	"Alert.Repo": "repo", "Workflow.Repo": "repo", "Repo.DefaultBranch": "branch", "Run.Branch": "branch",
}

// huge is 300 KiB of name followed by quotes and backslashes, which JSON
// escaping doubles: a value below the 8 MiB response cap that would make a
// line Loki refuses.
func huge(name string) string { return name + strings.Repeat(`"\`, 150<<10) }

// oversize sets every forge-filled string field of the row ptr points at to
// a huge value, and returns the qualified names it set.
func oversize(ptr any) []string {
	v := reflect.ValueOf(ptr).Elem()
	var set []string
	for i := range v.NumField() {
		f, field := v.Type().Field(i), v.Field(i)
		name := v.Type().Name() + "." + f.Name
		if !field.CanSet() || adapterOwned[name] {
			continue
		}
		value := huge(cmp.Or(sharedValue[name], name))
		switch field.Type() {
		case reflect.TypeFor[string]():
			field.SetString(value)
		case reflect.TypeFor[[]string]():
			field.Set(reflect.ValueOf([]string{value, value}))
		default:
			continue
		}
		set = append(set, name)
	}
	return set
}

func TestOversize_exemptions_name_real_fields(t *testing.T) {
	rows := []any{forge.Repo{}, forge.Run{}, forge.PullRequest{}, forge.Issue{}, forge.Alert{}, forge.Workflow{}}
	known := map[string]bool{}
	for _, row := range rows {
		ty := reflect.TypeOf(row)
		for f := range ty.Fields() {
			known[ty.Name()+"."+f.Name] = true
		}
	}
	for name := range adapterOwned {
		if !known[name] {
			t.Errorf("adapterOwned names %s, which no forge row has", name)
		}
	}
	for name := range sharedValue {
		if !known[name] {
			t.Errorf("sharedValue names %s, which no forge row has", name)
		}
	}
}

// oversizedEndpoint is a GitHub connection whose every row carries huge
// forge-filled values, and that logs every item message.
func oversizedEndpoint(t *testing.T) *endpoint {
	t.Helper()
	repoPath := huge("repo")
	ep := githubEndpoint("gh")
	repo := forge.Repo{Path: "o/r", DefaultBranch: "main"}
	oversize(&repo)
	ep.conn.repos = map[string][]forge.Repo{"o": {repo}}
	var runs []forge.Run
	for i := range 4 {
		r := run(int64(i+1), forge.RunFailing, now.Add(-time.Duration(4-i)*time.Hour))
		oversize(&r)
		runs = append(runs, r)
	}
	ep.conn.runs = map[string]listing{repoPath: {Runs: runs}}
	pr := forge.PullRequest{Number: 1, CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
	oversize(&pr)
	ep.conn.prs = map[string][]forge.PullRequest{"o": {pr}}
	ep.conn.checks = map[string]forge.CheckResult{pr.HeadSHA: {State: "failing"}}
	is := forge.Issue{Number: 2, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	oversize(&is)
	ep.conn.issues = map[string][]forge.Issue{"o": {is}}
	alert := forge.Alert{Number: 1, Source: "code_scanning", CreatedAt: now}
	oversize(&alert)
	ep.gh.alerts = map[string][]forge.Alert{repoPath: {alert}}
	wf := forge.Workflow{State: "disabled_inactivity"}
	oversize(&wf)
	ep.gh.workflows = map[string][]forge.Workflow{repoPath: {wf}}
	return ep
}

// jsonScan runs one scan of h through a real JSON handler at debug level and
// returns each line it wrote.
func jsonScan(t *testing.T, h *harness) [][]byte {
	t.Helper()
	var buf bytes.Buffer
	h.c.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.c.Scan(t.Context())
	var out [][]byte
	sc := bufio.NewScanner(&buf)
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		out = append(out, bytes.Clone(sc.Bytes()))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("Setup: read the JSON lines: %v", err)
	}
	return out
}

func TestScan_oversized_forge_values_keep_every_line_under_the_loki_limit(t *testing.T) {
	lines := jsonScan(t, newHarness(t, oversizedEndpoint(t)))
	itemMessages := slices.Concat(snapshotMessages, []string{dayMessage, "ci run"})
	seen := map[string]int{}
	for _, raw := range lines {
		var line map[string]any
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("Setup: decode line %.200s: %v", raw, err)
		}
		msg, _ := line["msg"].(string)
		if len(raw) > lokiLineLimit/4 {
			t.Errorf("%q line is %d bytes, want at most a quarter of Loki's %d-byte limit", msg, len(raw), lokiLineLimit)
		}
		if !slices.Contains(itemMessages, msg) {
			continue
		}
		seen[msg]++
		cut, _ := line["truncated"].(string)
		named := strings.Split(cut, ",")
		if cut == "" {
			t.Errorf("%q line carries forge values over the bound and no truncated field: %.300s", msg, raw)
		}
		for key, v := range line {
			if s, ok := v.(string); ok && strings.HasSuffix(s, cutMark) && !slices.Contains(named, key) {
				t.Errorf("%q line cut %s and truncated = %q does not name it", msg, key, cut)
			}
			if s, ok := v.(string); ok && len(s) > maxValueBytes {
				t.Errorf("%q line %s is %d bytes, want at most %d", msg, key, len(s), maxValueBytes)
			}
		}
		for _, key := range named {
			s, present := line[key].(string)
			if present && !strings.HasSuffix(s, cutMark) {
				t.Errorf("%q line truncated names %s, whose value %.40q was not cut", msg, key, s)
			}
		}
		if _, ok := line["url"]; ok && msg != "security tool" && msg != dayMessage {
			t.Errorf("%q line kept a link over the bound: %.80s", msg, line["url"])
		}
	}
	for _, msg := range itemMessages {
		if seen[msg] == 0 {
			t.Errorf("the oversized scan logged no %q line, so its bound is untested", msg)
		}
	}
}

func TestScan_an_oversized_read_error_keeps_its_warning_under_the_loki_limit(t *testing.T) {
	ep := oversizedEndpoint(t)
	ep.conn.errs = map[string]error{"issues o": errors.New(huge("issues"))}
	ep.gh.errs = map[string]error{"alerts " + huge("repo"): errors.New(huge("alerts"))}
	lines := jsonScan(t, newHarness(t, ep))
	warned := map[string]bool{}
	for _, raw := range lines {
		var line map[string]any
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("Setup: decode line %.200s: %v", raw, err)
		}
		msg, _ := line["msg"].(string)
		if _, ok := line["error"]; ok {
			warned[msg] = true
		}
		if len(raw) > lokiLineLimit/4 {
			t.Errorf("%q line is %d bytes, want at most a quarter of Loki's %d-byte limit", msg, len(raw), lokiLineLimit)
		}
	}
	for _, msg := range []string{"open issues listing failed", "code scanning unreadable"} {
		if !warned[msg] {
			t.Errorf("the scan logged no %q line with its error, so the error bound is untested; logged %v", msg, warned)
		}
	}
}

func TestScan_an_oversized_open_or_discovery_error_keeps_its_line_under_the_loki_limit(t *testing.T) {
	opening := githubEndpoint("opening")
	opening.openErr = errors.New(huge("open"))
	listing := githubEndpoint("listing")
	listing.conn.login = "other"
	listing.conn.errs = map[string]error{"discover o": errors.New(huge("discover"))}
	failed := map[string]bool{}
	for _, raw := range jsonScan(t, newHarness(t, opening, listing)) {
		var line map[string]any
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("Setup: decode line %.200s: %v", raw, err)
		}
		msg, _ := line["msg"].(string)
		if conn, _ := line["connection"].(string); msg == "repo discovery failed" {
			failed[conn] = true
		}
		if len(raw) > lokiLineLimit/4 {
			t.Errorf("%q line is %d bytes, want at most a quarter of Loki's %d-byte limit", msg, len(raw), lokiLineLimit)
		}
	}
	for _, conn := range []string{"opening", "listing"} {
		if !failed[conn] {
			t.Errorf("connection %s logged no repo discovery failed line, so its error bound is untested; logged %v", conn, failed)
		}
	}
}

func TestLineAttrs_link_keeps_a_valid_url_and_leaves_out_any_other(t *testing.T) {
	tests := []struct {
		url  string
		keep bool
	}{
		{url: "https://forge.test/o/r/pull/1", keep: true},
		{url: "http://forge.test/o/r/pull/1", keep: true},
		{url: "HTTPS://forge.test/o/r/pull/1", keep: true},
		{url: "Http://forge.test/o/r/pull/1", keep: true},
		{url: "", keep: true},
		{url: "https:///o/r/pull/1"},
		{url: "https:forge.test/o/r"},
		{url: "ftp://forge.test/o/r"},
		{url: "javascript:alert(1)"},
		{url: "https://forge.test/o/r\n/pull/1"},
		{url: "https://forge.test/\u202eo/r"},
		{url: "https://forge.test/" + strings.Repeat("a", maxValueBytes)},
	}
	for _, tc := range tests {
		var l lineAttrs
		l.link(tc.url)
		attrs := l.done()
		kept := slices.ContainsFunc(attrs, func(a slog.Attr) bool { return a.Key == "url" && a.Value.String() == tc.url })
		marked := slices.ContainsFunc(attrs, func(a slog.Attr) bool { return a.Key == "truncated" && a.Value.String() == "url" })
		if kept != tc.keep || marked == tc.keep {
			t.Errorf("link(%.60q) kept %v, marked truncated %v; want kept %v", tc.url, kept, marked, tc.keep)
		}
	}
}
