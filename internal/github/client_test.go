package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/model"
	"github.com/cplieger/httpx"
)

// newTestClient wires a Client at the test server's URL with a short-timeout
// http.Client so tests never hang.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := NewClient(httpx.NewClient(5*time.Second), "test-token", nil, slog.Default())
	c.baseURL = srv.URL
	return c
}

func TestListReposFiltersOwnerAndArchived(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Auth + version headers must be present on every call.
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != apiVersion {
			t.Errorf("api version header = %q, want %q", got, apiVersion)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"keep","owner":{"login":"cplieger"},"private":false,"archived":false},
			{"name":"private-keep","owner":{"login":"cplieger"},"private":true,"archived":false},
			{"name":"archived-skip","owner":{"login":"cplieger"},"private":false,"archived":true},
			{"name":"other-owner","owner":{"login":"someoneelse"},"private":false,"archived":false}
		]`))
	}))
	defer srv.Close()

	repos, err := newTestClient(t, srv).ListRepos(context.Background(), "cplieger")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2 (keep + private-keep)", len(repos))
	}
	names := map[string]bool{}
	for _, r := range repos {
		names[r.Name] = true
	}
	if !names["keep"] || !names["private-keep"] {
		t.Errorf("expected keep + private-keep, got %v", names)
	}
	if names["archived-skip"] || names["other-owner"] {
		t.Errorf("archived/other-owner repos should be filtered, got %v", names)
	}
}

func TestListReposPaginates(t *testing.T) {
	// Page 1 returns a full page (100) → client must fetch page 2.
	var full strings.Builder
	full.WriteByte('[')
	for i := range perPage {
		if i > 0 {
			full.WriteByte(',')
		}
		full.WriteString(`{"name":"r`)
		full.WriteString(itoa(i))
		full.WriteString(`","owner":{"login":"cplieger"}}`)
	}
	full.WriteByte(']')

	var pagesSeen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pagesSeen = append(pagesSeen, page)
		w.Header().Set("Content-Type", "application/json")
		if page == "1" {
			_, _ = w.Write([]byte(full.String()))
			return
		}
		_, _ = w.Write([]byte(`[{"name":"last","owner":{"login":"cplieger"}}]`))
	}))
	defer srv.Close()

	repos, err := newTestClient(t, srv).ListRepos(context.Background(), "cplieger")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != perPage+1 {
		t.Errorf("got %d repos, want %d", len(repos), perPage+1)
	}
	if len(pagesSeen) < 2 || pagesSeen[0] != "1" || pagesSeen[1] != "2" {
		t.Errorf("expected to fetch page 1 then 2, saw %v", pagesSeen)
	}
}

func TestListRunsAllConclusions(t *testing.T) {
	// One status=completed query returns runs of every conclusion; the
	// client maps them all (no per-conclusion fan-out) and preserves each
	// conclusion for the dashboard to filter and aggregate.
	var queries int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries++
		if got := r.URL.Query().Get("status"); got != "completed" {
			t.Errorf("status = %q, want completed", got)
		}
		// The created filter must carry the >= operator (URL-decoded here).
		if c := r.URL.Query().Get("created"); !strings.HasPrefix(c, ">=") {
			t.Errorf("created filter = %q, want >= prefix", c)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workflow_runs":[
			{"id":1,"name":"CI","head_branch":"main","run_number":42,"event":"push","conclusion":"success","html_url":"https://github.com/cplieger/x/actions/runs/1","created_at":"2026-06-20T10:00:00Z"},
			{"id":2,"name":"CI","head_branch":"main","run_number":43,"event":"push","conclusion":"failure","html_url":"https://github.com/cplieger/x/actions/runs/2","created_at":"2026-06-20T11:00:00Z"},
			{"id":3,"name":"Release","head_branch":"main","run_number":44,"event":"schedule","conclusion":"cancelled","html_url":"https://github.com/cplieger/x/actions/runs/3","created_at":"2026-06-20T12:00:00Z"}
		]}`))
	}))
	defer srv.Close()

	repo := model.Repo{Owner: "cplieger", Name: "x"}
	runs, err := newTestClient(t, srv).ListRuns(context.Background(), repo, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if queries != 1 {
		t.Errorf("made %d queries, want 1 (single status=completed call)", queries)
	}
	if len(runs) != 3 {
		t.Fatalf("got %d runs, want 3 (all conclusions, not just failures)", len(runs))
	}
	byConclusion := map[string]model.WorkflowRun{}
	for _, r := range runs {
		byConclusion[r.Conclusion] = r
	}
	for _, c := range []string{"success", "failure", "cancelled"} {
		if byConclusion[c].Repo != "cplieger/x" {
			t.Errorf("conclusion %q missing or wrong repo: %+v", c, byConclusion[c])
		}
	}
	if byConclusion["failure"].RunNumber != 43 {
		t.Errorf("failure run not parsed correctly: %+v", byConclusion["failure"])
	}
}

func TestListRunsPaginates(t *testing.T) {
	// A full first page (perPage runs) forces a second page fetch.
	var full strings.Builder
	full.WriteString(`{"workflow_runs":[`)
	for i := range perPage {
		if i > 0 {
			full.WriteByte(',')
		}
		full.WriteString(`{"id":`)
		full.WriteString(itoa(i + 1))
		full.WriteString(`,"conclusion":"success","created_at":"2026-06-20T10:00:00Z"}`)
	}
	full.WriteString(`]}`)

	var pagesSeen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pagesSeen = append(pagesSeen, page)
		w.Header().Set("Content-Type", "application/json")
		if page == "1" {
			_, _ = w.Write([]byte(full.String()))
			return
		}
		_, _ = w.Write([]byte(`{"workflow_runs":[{"id":99999,"conclusion":"failure","created_at":"2026-06-20T10:00:00Z"}]}`))
	}))
	defer srv.Close()

	repo := model.Repo{Owner: "cplieger", Name: "x"}
	runs, err := newTestClient(t, srv).ListRuns(context.Background(), repo, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != perPage+1 {
		t.Errorf("got %d runs, want %d", len(runs), perPage+1)
	}
	if len(pagesSeen) < 2 || pagesSeen[0] != "1" || pagesSeen[1] != "2" {
		t.Errorf("expected to fetch page 1 then 2, saw %v", pagesSeen)
	}
}

func TestUnsafeSegmentsRejected(t *testing.T) {
	c := NewClient(httpx.NewClient(time.Second), "tok", nil, slog.Default())
	if _, err := c.ListRepos(context.Background(), "../evil"); err == nil {
		t.Errorf("ListRepos accepted unsafe owner")
	}
	bad := model.Repo{Owner: "ok", Name: "../evil"}
	if _, err := c.ListRuns(context.Background(), bad, time.Now()); err == nil {
		t.Errorf("ListRuns accepted unsafe repo name")
	}
}

// itoa is a tiny stdlib-free int formatter for building the pagination
// fixture (avoids pulling strconv into the builder loop).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestSearchOpenPRsCrossRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, "is:open is:pr") || !strings.Contains(q, "user:cplieger") {
			t.Errorf("PR query missing qualifiers: %q", q)
		}
		if !strings.Contains(q, "-author:app/renovate") {
			t.Errorf("PR query missing exclude: %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[
			{"number":7,"title":"feat: x","html_url":"https://github.com/cplieger/a/pull/7","draft":false,"created_at":"2026-06-20T10:00:00Z","user":{"login":"cplieger"},"repository_url":"https://api.github.com/repos/cplieger/a"},
			{"number":8,"title":"wip","html_url":"https://github.com/cplieger/b/pull/8","draft":true,"created_at":"2026-06-20T11:00:00Z","user":{"login":"cplieger"},"repository_url":"https://api.github.com/repos/cplieger/b"}
		]}`))
	}))
	defer srv.Close()

	prs, err := newTestClient(t, srv).SearchOpenPRs(context.Background(), "cplieger", "-author:app/renovate")
	if err != nil {
		t.Fatalf("SearchOpenPRs: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("got %d PRs, want 2", len(prs))
	}
	if prs[0].Repo != "cplieger/a" || prs[0].Number != 7 || prs[0].Draft {
		t.Errorf("PR0 parsed wrong: %+v", prs[0])
	}
	if prs[1].Repo != "cplieger/b" || !prs[1].Draft {
		t.Errorf("PR1 parsed wrong: %+v", prs[1])
	}
}

func TestSearchOpenIssuesJoinsLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, "is:open is:issue") {
			t.Errorf("issue query missing qualifier: %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[
			{"number":12,"title":"bug","html_url":"https://github.com/cplieger/a/issues/12","created_at":"2026-06-20T10:00:00Z","user":{"login":"someone"},"labels":[{"name":"bug"},{"name":"p1"}],"repository_url":"https://api.github.com/repos/cplieger/a"}
		]}`))
	}))
	defer srv.Close()

	issues, err := newTestClient(t, srv).SearchOpenIssues(context.Background(), "cplieger", "-label:renovate")
	if err != nil {
		t.Fatalf("SearchOpenIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Labels != "bug,p1" {
		t.Errorf("issue labels not joined: %+v", issues)
	}
}

func TestListCodeScanningAlerts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "open" {
			t.Errorf("expected state=open, got %q", r.URL.Query().Get("state"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"number":3,"created_at":"2026-06-20T10:00:00Z","html_url":"https://github.com/cplieger/a/security/code-scanning/3","rule":{"id":"go/sql-injection","security_severity_level":"high"},"tool":{"name":"CodeQL"}}
		]`))
	}))
	defer srv.Close()

	alerts, err := newTestClient(t, srv).ListCodeScanningAlerts(context.Background(), model.Repo{Owner: "cplieger", Name: "a"})
	if err != nil {
		t.Fatalf("ListCodeScanningAlerts: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Rule != "go/sql-injection" || alerts[0].Severity != "high" || alerts[0].Tool != "CodeQL" {
		t.Errorf("alert parsed wrong: %+v", alerts)
	}
}

func TestCodeScanning404IsNotError(t *testing.T) {
	// A repo that never ran code scanning returns 404 — the client maps it
	// to an empty result, not an error, so a repo lacking CodeQL is silent.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no analysis found", http.StatusNotFound)
	}))
	defer srv.Close()

	alerts, err := newTestClient(t, srv).ListCodeScanningAlerts(context.Background(), model.Repo{Owner: "cplieger", Name: "a"})
	if err != nil {
		t.Errorf("404 should be tolerated, got error: %v", err)
	}
	if len(alerts) != 0 {
		t.Errorf("404 should yield no alerts, got %d", len(alerts))
	}
}

func TestCodeScanning403IsError(t *testing.T) {
	// A 403 is ambiguous — Advanced Security disabled, a missing token
	// scope, or a rate limit. It MUST surface as an error rather than be
	// silently mapped to "zero alerts", which would hide a security signal.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Resource not accessible by personal access token", http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).ListCodeScanningAlerts(context.Background(), model.Repo{Owner: "cplieger", Name: "a"}); err == nil {
		t.Errorf("403 must surface as an error (silent zero-alerts is a security false-negative)")
	}
}

func TestRepoFromAPIURL(t *testing.T) {
	tests := map[string]string{
		"https://api.github.com/repos/cplieger/github-scout": "cplieger/github-scout",
		"https://api.github.com/repos/owner/name":            "owner/name",
		"garbage": "garbage",
	}
	for in, want := range tests {
		if got := repoFromAPIURL(in); got != want {
			t.Errorf("repoFromAPIURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewClientNilLoggerDefaults(t *testing.T) {
	// A nil logger must fall back to slog.Default, never be left nil.
	c := NewClient(httpx.NewClient(time.Second), "tok", nil, nil)
	if c.logger == nil {
		t.Errorf("NewClient with nil logger left c.logger nil; want slog.Default fallback")
	}
}

// jsonList renders n comma-joined JSON objects from itemFmt (a single %d
// placeholder gets a per-item id), for building full-page pagination fixtures.
func jsonList(itemFmt string, n, base int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, itemFmt, base+i)
	}
	return b.String()
}

// capPageHandler serves a full page (perPage items) for page numbers 1..maxPages
// and a short page otherwise. A correct client fetches exactly maxPages pages;
// a loop-bound mutation (page--, page<maxPages, or len<=perPage break) diverges
// in the request count, and the out-of-range short page keeps a page-- mutant
// from looping forever. wrap formats the envelope around the item list.
func capPageHandler(requests *int, wrap func(items string) string, itemFmt string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*requests++
		w.Header().Set("Content-Type", "application/json")
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page >= 1 && page <= maxPages {
			_, _ = w.Write([]byte(wrap(jsonList(itemFmt, perPage, page*1000))))
			return
		}
		_, _ = w.Write([]byte(wrap("")))
	}
}

func TestListReposStopsAtMaxPages(t *testing.T) {
	var requests int
	srv := httptest.NewServer(capPageHandler(&requests,
		func(items string) string { return "[" + items + "]" },
		`{"name":"r%d","owner":{"login":"cplieger"}}`))
	defer srv.Close()

	repos, err := newTestClient(t, srv).ListRepos(context.Background(), "cplieger")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if requests != maxPages {
		t.Errorf("made %d page requests, want %d (must fetch up to maxPages)", requests, maxPages)
	}
	if len(repos) != maxPages*perPage {
		t.Errorf("got %d repos, want %d", len(repos), maxPages*perPage)
	}
}

func TestListRunsStopsAtMaxPages(t *testing.T) {
	var requests int
	srv := httptest.NewServer(capPageHandler(&requests,
		func(items string) string { return `{"workflow_runs":[` + items + `]}` },
		`{"id":%d,"conclusion":"success","created_at":"2026-06-20T10:00:00Z"}`))
	defer srv.Close()

	runs, err := newTestClient(t, srv).ListRuns(context.Background(),
		model.Repo{Owner: "cplieger", Name: "x"}, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if requests != maxPages {
		t.Errorf("made %d page requests, want %d", requests, maxPages)
	}
	if len(runs) != maxPages*perPage {
		t.Errorf("got %d runs, want %d", len(runs), maxPages*perPage)
	}
}

func TestSearchStopsAtMaxPages(t *testing.T) {
	var requests int
	srv := httptest.NewServer(capPageHandler(&requests,
		func(items string) string { return `{"items":[` + items + `]}` },
		`{"number":%d,"repository_url":"https://api.github.com/repos/cplieger/a","user":{"login":"cplieger"}}`))
	defer srv.Close()

	prs, err := newTestClient(t, srv).SearchOpenPRs(context.Background(), "cplieger", "")
	if err != nil {
		t.Fatalf("SearchOpenPRs: %v", err)
	}
	if requests != maxPages {
		t.Errorf("made %d page requests, want %d", requests, maxPages)
	}
	if len(prs) != maxPages*perPage {
		t.Errorf("got %d PRs, want %d", len(prs), maxPages*perPage)
	}
}

func TestListCodeScanningAlertsStopsAtMaxPages(t *testing.T) {
	var requests int
	srv := httptest.NewServer(capPageHandler(&requests,
		func(items string) string { return "[" + items + "]" },
		`{"number":%d,"rule":{"id":"go/x"},"tool":{"name":"CodeQL"}}`))
	defer srv.Close()

	alerts, err := newTestClient(t, srv).ListCodeScanningAlerts(context.Background(),
		model.Repo{Owner: "cplieger", Name: "a"})
	if err != nil {
		t.Fatalf("ListCodeScanningAlerts: %v", err)
	}
	if requests != maxPages {
		t.Errorf("made %d page requests, want %d", requests, maxPages)
	}
	if len(alerts) != maxPages*perPage {
		t.Errorf("got %d alerts, want %d", len(alerts), maxPages*perPage)
	}
}
