package collect

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/forge"
)

// readFields are the families every scan complete and scan stopped line
// names a read state for, with the counts each publishes.
var readFields = map[string][]string{
	"repos":              {"repos_discovered"},
	"open_prs":           {"open_prs"},
	"open_issues":        {"open_issues"},
	"runs":               {"slow_workflows_listed", "run_durations_known"},
	"failing_workflows":  {"failing_workflows"},
	"pr_checks":          {"failing_checks_prs"},
	"security_alerts":    {"security_alerts"},
	"disabled_workflows": {"disabled_workflows"},
}

// readStateEndpoint is a GitHub connection with one row of every family.
func readStateEndpoint() *endpoint {
	ep := githubEndpoint("gh")
	ep.conn.prs = map[string][]forge.PullRequest{"o": {{Repo: "o/r", Number: 1, Author: "x", HeadSHA: "h1"}}}
	ep.conn.issues = map[string][]forge.Issue{"o": {{Repo: "o/r", Number: 2, Author: "x"}}}
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}}}
	ep.conn.checks = map[string]forge.CheckResult{"h1": {State: "passing"}}
	ep.gh.alerts = map[string][]forge.Alert{"o/r": {{Repo: "o/r", Number: 3, Severity: "high", Tool: "CodeQL"}}}
	ep.gh.workflows = map[string][]forge.Workflow{"o/r": {
		{Repo: "o/r", Name: "CI", State: "active"}, {Repo: "o/r", Name: "nightly", State: "disabled_manually"},
	}}
	return ep
}

// fail makes the read named call answer err.
func fail(ep *endpoint, call string, err error) {
	if strings.HasPrefix(call, "alerts ") || strings.HasPrefix(call, "workflows ") {
		ep.gh.errs = map[string]error{call: err}
		return
	}
	ep.conn.errs = map[string]error{call: err}
}

// hold runs block before the read named call.
func hold(ep *endpoint, call string, block func(context.Context, string) error) {
	only := func(ctx context.Context, c string) error {
		if c != call {
			return nil
		}
		return block(ctx, c)
	}
	ep.conn.block, ep.gh.block = only, only
}

// verdictLine is the connection's scan complete line, else its scan stopped.
func verdictLine(h *harness) map[string]string {
	for _, msg := range []string{"scan complete", "scan stopped"} {
		for _, l := range lines(h.rec, msg) {
			if l["connection"] == "gh" {
				l["msg"] = msg
				return l
			}
		}
	}
	return nil
}

func TestScan_a_clean_read_of_every_family_is_complete(t *testing.T) {
	h := newHarness(t, readStateEndpoint())
	if got := h.scan(t); got != Complete {
		t.Fatalf("Scan = %s, want complete; messages %v", got, h.rec.Messages())
	}
	l := verdictLine(h)
	for fam := range readFields {
		if l[fam+"_read"] != "complete" {
			t.Errorf("clean scan %s_read = %q, want complete", fam, l[fam+"_read"])
		}
	}
}

// failurePoint is one failure injected into one family's read; limit, when
// set, bounds the connection's scan, unwritable fails the state save, and
// notDurable leaves it on disk but not durable. readFailed marks the
// family's only read failing, so the family has nothing to count from.
type failurePoint struct {
	setup      func(ep *endpoint, cancel context.CancelFunc)
	name       string
	family     string
	limit      time.Duration
	unwritable bool
	notDurable bool
	readFailed bool
}

// readFailure is one way a read can fail, injected into every read of the
// failure table; sentinel names the forge error it answers, if any.
type readFailure struct {
	inject   func(ep *endpoint, call string, cancel context.CancelFunc)
	name     string
	sentinel string
	limit    time.Duration
}

func failWith(err error) func(*endpoint, string, context.CancelFunc) {
	return func(ep *endpoint, call string, _ context.CancelFunc) { fail(ep, call, err) }
}

// readFailures is every way a read can fail. TestReadFailures_name_every_forge_sentinel
// holds it to every sentinel internal/forge declares.
var readFailures = []readFailure{
	{name: "error", inject: failWith(errors.New("upstream 502"))},
	{name: "token_rejected", sentinel: "ErrTokenInvalid", inject: failWith(forge.ErrTokenInvalid)},
	{name: "rate_limited", sentinel: "ErrRateLimited", inject: failWith(forge.ErrRateLimited)},
	{name: "reserve_stop", sentinel: "ErrReadDeferred", inject: failWith(forge.ErrReadDeferred)},
	{name: "partial_page", sentinel: "ErrSnapshotPartial", inject: failWith(forge.ErrSnapshotPartial)},
	{name: "forbidden", sentinel: "ErrForbidden", inject: failWith(forge.ErrForbidden)},
	{name: "refused", sentinel: "ErrRefused", inject: failWith(forge.ErrRefused)},
	{name: "connection_failed", sentinel: "ErrConnection", inject: failWith(forge.ErrConnection)},
	{name: "timeout", limit: 100 * time.Millisecond, inject: func(ep *endpoint, call string, _ context.CancelFunc) {
		hold(ep, call, blockUntilDone(call, 3*time.Second))
	}},
	{name: "cancel", inject: func(ep *endpoint, call string, cancel context.CancelFunc) {
		hold(ep, call, func(context.Context, string) error { cancel(); return context.Canceled })
	}},
}

// notReadFailures are the forge sentinels a source answers that fail no
// read, each with the answer it gets and the test that pins it.
var notReadFailures = map[string]string{
	"ErrOwnerUnresolved": "an owner listing the instance does not resolve is read per repository: TestScan_owner_unresolved_listing_falls_back_per_repository",
	"ErrRunsUndated":     "a product whose runs carry no creation time has no run signals: TestScan_undated_runs_on_every_repository_are_unsupported",
}

// forgeSentinels is every exported Err variable internal/forge declares.
func forgeSentinels(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "../forge/forge.go", nil, 0)
	if err != nil {
		t.Fatalf("Setup: parse internal/forge: %v", err)
	}
	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			for _, n := range spec.(*ast.ValueSpec).Names {
				if strings.HasPrefix(n.Name, "Err") {
					names = append(names, n.Name)
				}
			}
		}
	}
	return names
}

func TestReadFailures_name_every_forge_sentinel(t *testing.T) {
	sentinels := forgeSentinels(t)
	if len(sentinels) == 0 {
		t.Fatal("forgeSentinels found no Err variable in internal/forge, want its sentinels")
	}
	injected := map[string]bool{}
	for _, f := range readFailures {
		if f.sentinel != "" {
			injected[f.sentinel] = true
		}
	}
	for _, s := range sentinels {
		_, exempt := notReadFailures[s]
		if injected[s] == exempt {
			t.Errorf("forge.%s injected %t, exempt %t: every sentinel a source can answer is exactly one of a readFailures entry or a notReadFailures entry",
				s, injected[s], exempt)
		}
	}
	for s := range injected {
		if !slices.Contains(sentinels, s) {
			t.Errorf("readFailures injects forge.%s, which internal/forge does not declare", s)
		}
	}
}

// TestScan_no_failure_point_reads_as_complete drives every family through
// each failure the fakes can inject and checks the summary never calls the
// family complete, never publishes a count for a family it could not read,
// and never reports the scan complete.
func TestScan_no_failure_point_reads_as_complete(t *testing.T) {
	cases := []failurePoint{
		{name: "repos_cut", family: "repos", setup: func(ep *endpoint, _ context.CancelFunc) {
			ep.conn.truncated = true
		}},
		{name: "repos_no_owner_resolved", family: "repos", setup: func(ep *endpoint, _ context.CancelFunc) {
			ep.conn.unresolved = []string{"o"}
		}},
		{name: "repos_unresolved", family: "repos", setup: func(ep *endpoint, _ context.CancelFunc) {
			ep.cfg.Owners = []string{"o", "p"}
			ep.conn.unresolved = []string{"p"}
		}},
		{name: "prs_unlisted_row", family: "open_prs", setup: func(ep *endpoint, _ context.CancelFunc) {
			ep.conn.prs["o"] = append(ep.conn.prs["o"], forge.PullRequest{Repo: "o/unlisted", Number: 9, Author: "x", HeadSHA: "h9"})
		}},
		{name: "issues_unlisted_row", family: "open_issues", setup: func(ep *endpoint, _ context.CancelFunc) {
			ep.conn.issues["o"] = append(ep.conn.issues["o"], forge.Issue{Repo: "o/unlisted", Number: 9, Author: "x"})
		}},
		{name: "runs_cut", family: "runs", setup: func(ep *endpoint, _ context.CancelFunc) {
			list := ep.conn.runs["o/r"]
			list.Truncated = true
			ep.conn.runs["o/r"] = list
		}},
		{name: "runs_unstorable", family: "runs", setup: func(ep *endpoint, _ context.CancelFunc) {
			bad := run(2, forge.RunFailing, now.Add(-time.Hour))
			bad.CreatedAt = time.Time{}
			ep.conn.runs["o/r"] = listing{Runs: []forge.Run{bad}}
		}},
		{name: "checks_partial", family: "pr_checks", setup: func(ep *endpoint, _ context.CancelFunc) {
			ep.conn.checks["h1"] = forge.CheckResult{State: "passing", End: forge.EndCut}
		}},
		{name: "checks_no_head", family: "pr_checks", setup: func(ep *endpoint, _ context.CancelFunc) {
			ep.conn.prs["o"][0].HeadSHA = ""
		}},
		{name: "alerts_cut", family: "security_alerts", setup: func(ep *endpoint, _ context.CancelFunc) {
			ep.gh.truncated = map[string]bool{"alerts o/r": true}
		}},
		{name: "workflows_cut", family: "disabled_workflows", setup: func(ep *endpoint, _ context.CancelFunc) {
			ep.gh.truncated = map[string]bool{"workflows o/r": true}
		}},
		{name: "state_unwritable", family: "run_state", unwritable: true, setup: func(*endpoint, context.CancelFunc) {}},
		{name: "state_not_durable", family: "run_state", notDurable: true, setup: func(*endpoint, context.CancelFunc) {}},
	}
	for _, call := range []struct{ name, family, call string }{
		{"discover", "repos", "discover o"},
		{"prs", "open_prs", "prs o"},
		{"issues", "open_issues", "issues o"},
		{"runs", "runs", "runs o/r"},
		{"checks", "pr_checks", "checks h1"},
		{"alerts", "security_alerts", "alerts o/r"},
		{"workflows", "disabled_workflows", "workflows o/r"},
	} {
		for _, f := range readFailures {
			cases = append(cases, failurePoint{
				name: call.name + "_" + f.name, family: call.family, limit: f.limit, readFailed: true,
				setup: func(ep *endpoint, cancel context.CancelFunc) { f.inject(ep, call.call, cancel) },
			})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep := readStateEndpoint()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tc.setup(ep, cancel)
			h := newLimitedHarness(t, tc.limit, ep)
			if tc.unwritable {
				blockStateWrites(t, h.dir)
			}
			if tc.notDurable {
				commitNotDurable(t)
			}
			h.rec = h.resetRecorder(t)
			got := h.c.Scan(ctx)
			if got == Complete {
				t.Errorf("Scan with %s = complete, want a trigger to exit 1; messages %v", tc.name, h.rec.Messages())
			}
			l := verdictLine(h)
			if strings.HasSuffix(tc.name, "_cancel") {
				if got != Interrupted || l != nil {
					t.Errorf("Scan cancelled at %s = %s with verdict %v, want interrupted and no verdict line", tc.name, got, l)
				}
				return
			}
			if l == nil {
				t.Fatalf("Scan with %s logged no scan complete or scan stopped; messages %v", tc.name, h.rec.Messages())
			}
			for fam := range readFields {
				if _, ok := l[fam+"_read"]; !ok {
					t.Errorf("%s with %s carries no %s_read", l["msg"], tc.name, fam)
				}
			}
			if tc.family == "run_state" {
				if !strings.Contains(l["failed_signals"], "run_state") || l["degraded"] != "true" {
					t.Errorf("%s with %s = failed_signals %q degraded %q, want run_state named and degraded",
						l["msg"], tc.name, l["failed_signals"], l["degraded"])
				}
				return
			}
			state := l[tc.family+"_read"]
			if state == "complete" {
				t.Errorf("%s with %s says %s_read complete; line %v", l["msg"], tc.name, tc.family, l)
			}
			if repos := l["repos_read"]; repos != "complete" {
				for fam := range readFields {
					if s := l[fam+"_read"]; fam != "repos" && (s == "complete" || (repos == "blind" && s == "partial")) {
						t.Errorf("%s with %s: %s_read %s through a %s repository list, want it no more complete", l["msg"], tc.name, fam, s, repos)
					}
				}
			}
			for fam, counts := range readFields {
				if s := l[fam+"_read"]; s != "unread" && s != "blind" {
					continue
				}
				for _, k := range counts {
					if v, ok := l[k]; ok {
						t.Errorf("%s with %s carries %s = %s while %s_read is %s, want no count", l["msg"], tc.name, k, v, fam, l[fam+"_read"])
					}
				}
			}
			if !tc.readFailed {
				return
			}
			if l["msg"] == "scan complete" && state != "blind" {
				t.Errorf("scan complete with %s says %s_read %s, want blind: its only read failed", tc.name, tc.family, state)
			}
			for _, k := range readFields[tc.family] {
				if v, ok := l[k]; ok {
					t.Errorf("%s with %s carries %s = %s, want no count: the family's only read failed", l["msg"], tc.name, k, v)
				}
			}
		})
	}
}

// partialUnit is one family fed a partial or cut input: rows names the lines
// none of which may be logged, counts the scan complete fields that must be
// absent, checksUnknown that every pull request row reads checks unknown,
// and vouched that the failing row is the newest run the cut listing read.
type partialUnit struct {
	setup         func(ep *endpoint)
	name          string
	rows          []string
	counts        []string
	checksUnknown bool
	vouched       bool
}

// TestScan_no_partial_unit_derives_a_row_field_count_or_verdict feeds each
// family a partial or cut input and checks nothing published or stored is
// derived from it.
func TestScan_no_partial_unit_derives_a_row_field_count_or_verdict(t *testing.T) {
	snapshots := []string{"open pull request", "open issue", "security alert", "top security alert", "security tool", "disabled workflow"}
	derived := []string{"failing workflow", "slow workflow"}
	cases := []partialUnit{
		{
			name: "repos_cut", rows: append(slices.Clone(snapshots), derived...),
			counts: []string{"open_prs", "open_issues", "failing_checks_prs", "security_alerts", "disabled_workflows", "failing_workflows"},
			setup:  func(ep *endpoint) { ep.conn.truncated = true },
		},
		{
			name: "prs_partial", rows: []string{"open pull request"}, counts: []string{"open_prs", "failing_checks_prs"},
			setup: func(ep *endpoint) { fail(ep, "prs o", forge.ErrSnapshotPartial) },
		},
		{
			name: "issues_partial", rows: []string{"open issue"}, counts: []string{"open_issues"},
			setup: func(ep *endpoint) { fail(ep, "issues o", forge.ErrSnapshotPartial) },
		},
		{
			name: "runs_cut", rows: []string{"slow workflow"}, vouched: true,
			setup: func(ep *endpoint) { ep.conn.runs["o/r"] = listing{Runs: timedFailures(), Truncated: true} },
		},
		{
			name: "checks_partial", checksUnknown: true,
			setup: func(ep *endpoint) { ep.conn.checks["h1"] = forge.CheckResult{State: "failing", End: forge.EndCut} },
		},
		{
			name: "alerts_cut", rows: []string{"security alert", "top security alert", "security tool"}, counts: []string{"security_alerts"},
			setup: func(ep *endpoint) { ep.gh.truncated = map[string]bool{"alerts o/r": true} },
		},
		{
			name: "workflows_cut", rows: []string{"disabled workflow"}, counts: []string{"disabled_workflows"},
			setup: func(ep *endpoint) { ep.gh.truncated = map[string]bool{"workflows o/r": true} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep := readStateEndpoint()
			tc.setup(ep)
			h := newHarness(t, ep)
			h.scan(t)
			for _, msg := range tc.rows {
				if got := lines(h.rec, msg); len(got) != 0 {
					t.Errorf("%s: %q lines = %v, want none derived from the partial read", tc.name, msg, got)
				}
			}
			c := lineFor(t, h.rec, "scan complete", "gh")
			for _, k := range tc.counts {
				if v, ok := c[k]; ok {
					t.Errorf("%s: scan complete carries %s = %s, want no count of a partial read", tc.name, k, v)
				}
			}
			if tc.checksUnknown {
				for _, l := range lines(h.rec, "open pull request") {
					if l["checks"] != "unknown" {
						t.Errorf("%s: pull request %s checks = %q, want unknown: an omitted check may change the fold", tc.name, l["number"], l["checks"])
					}
				}
				if c["failing_checks_prs"] != "0" {
					t.Errorf("%s: failing_checks_prs = %q, want 0: a partial fold is unknown, not failing", tc.name, c["failing_checks_prs"])
				}
			}
			if tc.vouched {
				if l := lineFor(t, h.rec, "failing workflow", "gh"); l["state"] != "failing" || l["run_id"] != "1" || l["failing_since_clipped"] != "true" {
					t.Errorf("%s: failing workflow = %v, want run 1 failing, the newest the cut listing read, its streak clipped at the cut", tc.name, l)
				}
				if n := h.rec.CountExact("ci run"); n != minDurationRuns {
					t.Errorf("%s: ci run lines = %d, want each of the %d runs read: a run event is not a derived row", tc.name, n, minDurationRuns)
				}
			}
		})
	}
}

// timedFailures is minDurationRuns failing runs of o/r's CI, enough for a
// slow workflow row.
func timedFailures() []forge.Run {
	var runs []forge.Run
	for i := range minDurationRuns {
		runs = append(runs, run(int64(i+1), forge.RunFailing, now.Add(-time.Duration(i+3)*time.Hour)))
	}
	return runs
}

func TestScan_an_unmapped_run_leaves_runs_read_complete_and_its_workflow_at_its_last_verdict(t *testing.T) {
	ep := githubEndpoint("gh")
	odd := run(2, "", now.Add(-30*time.Minute))
	ep.conn.runs = map[string]listing{"o/r": {Runs: []forge.Run{run(1, forge.RunFailing, now.Add(-time.Hour))}, Unmapped: []forge.Run{odd}}}
	h := newHarness(t, ep)
	if got := h.scan(t); got != Complete {
		t.Errorf("Scan with an unmapped run = %s, want complete: the listing was read to its end", got)
	}
	c := lineFor(t, h.rec, "scan complete", "gh")
	if c["runs_unmapped"] != "1" || c["runs_read"] != "complete" || c["failing_workflows_read"] != "complete" || c["degraded"] != "false" {
		t.Errorf("scan with an unmapped run = runs_unmapped %q runs_read %q failing_workflows_read %q degraded %q, want 1, complete, complete, false",
			c["runs_unmapped"], c["runs_read"], c["failing_workflows_read"], c["degraded"])
	}
	if l := lineFor(t, h.rec, "failing workflow", "gh"); l["run_id"] != "1" || l["state"] != "failing" {
		t.Errorf("failing workflow = %v, want run 1 failing, its last judged run: a run in an unmapped state is not judged", l)
	}
	if l := lineFor(t, h.rec, "run state unknown", "gh"); l["level"] != "WARN" || l["repo"] != "o/r" || l["runs"] != "1" {
		t.Errorf("run state unknown = %v, want WARN naming o/r and 1 run", l)
	}
	if l := lineFor(t, h.rec, "ci run", "gh"); l["run_id"] != "1" {
		t.Errorf("ci run = %v, want the known run 1 still logged", l)
	}
}

func TestScan_operator_exclusions_and_archived_repositories_leave_every_read_complete(t *testing.T) {
	ep := readStateEndpoint()
	ep.conn.repos["o"] = append(ep.conn.repos["o"],
		forge.Repo{Path: "o/old", DefaultBranch: "main", Archived: true}, forge.Repo{Path: "o/x", DefaultBranch: "main"},
		forge.Repo{Path: "o/fork", DefaultBranch: "main", Fork: true})
	ep.cfg.ExcludeRepos["o/x"] = true
	ep.cfg.ExcludeAuthors["renovate[bot]"] = true
	ep.conn.prs["o"] = append(ep.conn.prs["o"],
		forge.PullRequest{Repo: "o/old", Number: 5, Author: "x"}, forge.PullRequest{Repo: "o/r", Number: 6, Author: "renovate[bot]", HeadSHA: "h6"})
	ep.conn.issues["o"] = append(ep.conn.issues["o"], forge.Issue{Repo: "o/x", Number: 7, Author: "x"})
	h := newHarness(t, ep)
	if got := h.scan(t); got != Complete {
		t.Fatalf("Scan with only configured exclusions = %s, want complete; messages %v", got, h.rec.Messages())
	}
	l := verdictLine(h)
	for _, fam := range []string{"repos", "open_prs", "open_issues", "runs", "pr_checks", "security_alerts", "disabled_workflows"} {
		if l[fam+"_read"] != "complete" {
			t.Errorf("%s_read = %q, want complete: an excluded item is not a missing one", fam, l[fam+"_read"])
		}
	}
	if l["open_prs"] != "1" || l["excluded_prs"] != "1" || l["skipped"] != "1" || l["security_skipped"] != "1" {
		t.Errorf("open_prs %q excluded_prs %q skipped %q security_skipped %q, want 1, 1, 1 and the fork's 1",
			l["open_prs"], l["excluded_prs"], l["skipped"], l["security_skipped"])
	}
}

func TestScan_security_switched_off_reads_excluded_and_complete(t *testing.T) {
	ep := readStateEndpoint()
	ep.cfg.Security.Enabled = false
	h := newHarness(t, ep)
	if got := h.scan(t); got != Complete {
		t.Errorf("Scan with security switched off = %s, want complete", got)
	}
	if l := verdictLine(h); l["security_alerts_read"] != "excluded" || l["unsupported_signals"] != "" {
		t.Errorf("security_alerts_read %q unsupported_signals %q, want excluded and none", l["security_alerts_read"], l["unsupported_signals"])
	}
}

func TestScan_a_limit_reached_after_the_last_read_is_still_a_stop(t *testing.T) {
	ep := giteaEndpoint("gt")
	ep.conn.block = func(ctx context.Context, call string) error {
		if call != "runs o/r" {
			return nil
		}
		<-ctx.Done()
		return forge.ErrRunsUndated
	}
	h := newLimitedHarness(t, 100*time.Millisecond, ep)
	if got := h.scan(t); got != Incomplete {
		t.Errorf("Scan past its limit after its last read = %s, want incomplete; messages %v", got, h.rec.Messages())
	}
	if s := lineFor(t, h.rec, "scan stopped", "gt"); s["reason"] != "scan_timeout" {
		t.Errorf("scan stopped = %v, want scan_timeout", s)
	}
}

func TestScan_a_connection_that_ends_early_still_names_its_products_unsupported_signals(t *testing.T) {
	tests := []struct {
		setup   func(ep *endpoint)
		name    string
		msg     string
		product forge.Product
		want    string
	}{
		{
			name: "gitlab_discovery_rejected", msg: "scan complete", product: forge.ProductGitLab, want: "security_alerts,disabled_workflows",
			setup: func(ep *endpoint) { ep.conn.errs = map[string]error{"discover o": forge.ErrTokenInvalid} },
		},
		{
			name: "forgejo_rejected_after_detection", msg: "scan complete", product: forge.ProductForgejo,
			want: "pr_checks,security_alerts,disabled_workflows",
			setup: func(ep *endpoint) {
				ep.openErr = &forge.OpenError{Product: forge.ProductForgejo, Err: fmt.Errorf("identify account: %w", forge.ErrTokenInvalid)}
			},
		},
		{
			name: "gitea_stopped_at_discovery", msg: "scan stopped", product: forge.ProductGitea,
			want:  "pr_checks,security_alerts,disabled_workflows",
			setup: func(ep *endpoint) { ep.conn.errs = map[string]error{"discover o": forge.ErrReadDeferred} },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := giteaEndpoint("c")
			ep.conn.product = tc.product
			tc.setup(ep)
			h := newHarness(t, ep)
			h.scan(t)
			l := lineFor(t, h.rec, tc.msg, "c")
			if got := l["unsupported_signals"]; got != tc.want {
				t.Errorf("%s %s unsupported_signals = %q, want %q: what the product lacks does not depend on how far the scan got",
					tc.name, tc.msg, got, tc.want)
			}
			for _, fam := range []string{"open_prs", "open_issues", "runs"} {
				if l[fam+"_read"] != "unread" {
					t.Errorf("%s %s %s_read = %q, want unread", tc.name, tc.msg, fam, l[fam+"_read"])
				}
			}
			for _, k := range []string{"open_prs", "open_issues", "security_alerts", "failing_checks_prs"} {
				if v, ok := l[k]; ok {
					t.Errorf("%s %s carries %s = %s, want no count for a family never read", tc.name, tc.msg, k, v)
				}
			}
		})
	}
}
