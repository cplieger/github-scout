package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/model"
	"github.com/cplieger/httpx/v3"
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
		if !strings.Contains(q, "archived:false") {
			t.Errorf("PR query missing archived:false qualifier (archived repos must be excluded): %q", q)
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
		if !strings.Contains(q, "archived:false") {
			t.Errorf("issue query missing archived:false qualifier (archived repos must be excluded): %q", q)
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

func TestCodeScanning404IsNoCodeScanning(t *testing.T) {
	// A repo that never ran code scanning returns 404 — the client maps it to
	// the benign model.ErrNoCodeScanning sentinel (NOT a read failure, and NOT
	// a silent clean read), so the collector can exclude it from the "blind"
	// calculation. It still yields no alerts.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no analysis found", http.StatusNotFound)
	}))
	defer srv.Close()

	alerts, err := newTestClient(t, srv).ListCodeScanningAlerts(context.Background(), model.Repo{Owner: "cplieger", Name: "a"})
	if !errors.Is(err, model.ErrNoCodeScanning) {
		t.Errorf("404 should map to model.ErrNoCodeScanning, got: %v", err)
	}
	if len(alerts) != 0 {
		t.Errorf("404 should yield no alerts, got %d", len(alerts))
	}
}

func TestCodeScanning403IsError(t *testing.T) {
	// A 403 is ambiguous — Advanced Security disabled, a missing token
	// scope, or a rate limit. It MUST surface as an error rather than be
	// silently mapped to "zero alerts", which would hide a security signal,
	// and it must NOT be the benign no-code-scanning sentinel.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Resource not accessible by personal access token", http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).ListCodeScanningAlerts(context.Background(), model.Repo{Owner: "cplieger", Name: "a"})
	if err == nil {
		t.Errorf("403 must surface as an error (silent zero-alerts is a security false-negative)")
	}
	if errors.Is(err, model.ErrNoCodeScanning) {
		t.Errorf("403 must NOT be mapped to the benign no-code-scanning sentinel")
	}
	// A 403 is per-repo (one private repo without GHAS, a missing scope), not
	// an org-wide class, so it must map to NEITHER systemic sentinel; the
	// collector then treats it as a plain per-repo failure that escalates only
	// when code scanning is blind for every repo that has it.
	if errors.Is(err, model.ErrTokenInvalid) || errors.Is(err, model.ErrRateLimited) {
		t.Errorf("403 must not map to a systemic sentinel, got: %v", err)
	}
}

// TestStatus401MapsTokenInvalid and TestStatus429MapsRateLimited pin the
// systemic status→sentinel mapping the collector's escalation depends on. They
// are the boundary half of the contract: the github client is the single place
// that turns an HTTP status into meaning, so internal/collect can classify on
// model sentinels without importing the HTTP transport. A regression that
// stopped mapping 401/429 would silently downgrade an org-wide credential or
// rate-limit failure to a per-repo blip and fail to page.
func TestStatus401MapsTokenInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Bad credentials", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).ListRepos(context.Background(), "cplieger")
	if !errors.Is(err, model.ErrTokenInvalid) {
		t.Errorf("401 should map to model.ErrTokenInvalid, got: %v", err)
	}
	if errors.Is(err, model.ErrRateLimited) {
		t.Errorf("401 must not also be ErrRateLimited")
	}
}

func TestStatus429MapsRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "API rate limit exceeded", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	// One attempt only: a 429 is retryable, and the test only needs to observe
	// the post-exhaustion mapping, not sit through backoff.
	c := NewClient(httpx.NewClient(5*time.Second), "test-token", []httpx.GetOption{httpx.WithMaxAttempts(1)}, slog.Default())
	c.baseURL = srv.URL

	_, err := c.ListRepos(context.Background(), "cplieger")
	if !errors.Is(err, model.ErrRateLimited) {
		t.Errorf("429 should map to model.ErrRateLimited, got: %v", err)
	}
	if errors.Is(err, model.ErrTokenInvalid) {
		t.Errorf("429 must not also be ErrTokenInvalid")
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

// TestCodeScanningNotFound pins the 404-vs-everything-else classifier that
// decides whether a code-scanning read error is the benign no-analyses 404 or
// a real failure. The security contract hinges on the non-StatusError branch:
// a couldn't-check (a transient or decode error) must never be read as a
// confirmed-clean, so only a 404 StatusError maps to true.
func TestCodeScanningNotFound(t *testing.T) {
	tests := []struct {
		err  error
		name string
		want bool
	}{
		{&httpx.StatusError{Code: http.StatusNotFound}, "404 status is no-code-scanning", true},
		{&httpx.StatusError{Code: http.StatusForbidden}, "403 status is not no-code-scanning", false},
		{&httpx.StatusError{Code: http.StatusInternalServerError}, "500 status is not no-code-scanning", false},
		{errors.New("decode: unexpected EOF"), "non-status error is not no-code-scanning", false},
		{nil, "nil error is not no-code-scanning", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codeScanningNotFound(tt.err); got != tt.want {
				t.Errorf("codeScanningNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestUnsafeSegmentsRejectedSearchAndCodeScanning extends the urlsafe guard
// coverage to the three methods TestUnsafeSegmentsRejected does not exercise:
// SearchOpenPRs / SearchOpenIssues (via search) and ListCodeScanningAlerts. A
// traversal/injection segment (../evil) must be rejected before URL
// construction in each.
func TestUnsafeSegmentsRejectedSearchAndCodeScanning(t *testing.T) {
	c := NewClient(httpx.NewClient(time.Second), "tok", nil, slog.Default())

	if _, err := c.SearchOpenPRs(context.Background(), "../evil", ""); err == nil {
		t.Errorf("SearchOpenPRs accepted unsafe owner")
	}
	if _, err := c.SearchOpenIssues(context.Background(), "../evil", ""); err == nil {
		t.Errorf("SearchOpenIssues accepted unsafe owner")
	}
	bad := model.Repo{Owner: "ok", Name: "../evil"}
	if _, err := c.ListCodeScanningAlerts(context.Background(), bad); err == nil {
		t.Errorf("ListCodeScanningAlerts accepted unsafe repo name")
	}
}

// TestSearchIncompleteResultsErrors pins the data-integrity contract on the
// Search API's incomplete_results flag: GitHub sets it when a search times out
// server-side, so the returned set is partial and must NOT be read as a
// confirmed-empty/complete result. search() returns an error in that case and
// SearchOpenPRs propagates it (nil, err), so the collector folds the failed
// search into its degraded/signal_blind verdict rather than reporting a false 0.
func TestSearchIncompleteResultsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"incomplete_results":true,"items":[
			{"number":1,"repository_url":"https://api.github.com/repos/cplieger/a","user":{"login":"cplieger"}}
		]}`))
	}))
	defer srv.Close()

	prs, err := newTestClient(t, srv).SearchOpenPRs(context.Background(), "cplieger", "")
	if err == nil {
		t.Fatalf("SearchOpenPRs must error when GitHub returns incomplete_results (a timed-out search is not a confirmed-empty result)")
	}
	if !strings.Contains(err.Error(), "incomplete results") {
		t.Errorf("error = %v, want it to mention incomplete results", err)
	}
	if prs != nil {
		t.Errorf("prs = %v, want nil PRs on an incomplete-results error", prs)
	}
}

// TestCodeScanning404MidPaginationIsRealError pins the len(alerts)==0 guard in
// ListCodeScanningAlerts: a 404 is mapped to the benign model.ErrNoCodeScanning
// ONLY before any alert is collected. A 404 on page 2+ (after page 1 returned a
// full page) is a real read failure and must surface as a wrapped error, never
// be swallowed as "no code scanning" (which would drop the alerts already read
// and report a false clean). Statement coverage is green on the line via the
// first-page-404 test, but the len(alerts)!=0 path is otherwise unexercised.
func TestCodeScanning404MidPaginationIsRealError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte("[" + jsonList(`{"number":%d,"rule":{"id":"go/x"},"tool":{"name":"CodeQL"}}`, perPage, 1) + "]"))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).ListCodeScanningAlerts(context.Background(), model.Repo{Owner: "cplieger", Name: "a"})
	if err == nil {
		t.Fatalf("a 404 mid-pagination (after alerts were collected) must be a real error, not silently swallowed as no-code-scanning")
	}
	if errors.Is(err, model.ErrNoCodeScanning) {
		t.Errorf("a mid-pagination 404 must NOT map to model.ErrNoCodeScanning: err = %v", err)
	}
}

// TestGetJSONSurfacesDecodeError pins the untrusted-input decode chokepoint:
// getJSON is the single point through which all five API reads decode bytes
// from GitHub, so a malformed body must surface as a "decode response" error
// rather than be swallowed into a zero-value struct and read as a
// confirmed-empty result.
func TestGetJSONSurfacesDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items": [ this is not valid json `))
	}))
	defer srv.Close()

	prs, err := newTestClient(t, srv).SearchOpenPRs(context.Background(), "cplieger", "")
	if err == nil {
		t.Fatalf("SearchOpenPRs must error on a malformed JSON body, not read it as zero results")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("error = %v, want it to mention \"decode response\"", err)
	}
	if prs != nil {
		t.Errorf("prs = %v, want nil on a decode error", prs)
	}
}

// TestListRunsReturnsPartialOnMidPaginationError pins ListRuns's partial-return
// contract: on a getJSON failure mid-pagination it returns the runs collected
// from earlier pages ALONGSIDE the error (return runs, err -- not nil, err),
// unlike the other readers. The collector relies on this: collectRuns emits the
// partial set while still folding the error into its degraded verdict.
func TestListRunsReturnsPartialOnMidPaginationError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(`{"workflow_runs":[` +
				jsonList(`{"id":%d,"conclusion":"success","created_at":"2026-06-20T10:00:00Z"}`, perPage, 1) + `]}`))
			return
		}
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(httpx.NewClient(5*time.Second), "test-token", []httpx.GetOption{httpx.WithMaxAttempts(1)}, slog.Default())
	c.baseURL = srv.URL

	runs, err := c.ListRuns(context.Background(), model.Repo{Owner: "cplieger", Name: "x"}, time.Now().Add(-24*time.Hour))
	if err == nil {
		t.Fatalf("ListRuns must error when a page fetch fails mid-pagination")
	}
	if len(runs) != perPage {
		t.Errorf("got %d runs, want %d (the page-1 set must be returned alongside the error, not dropped)", len(runs), perPage)
	}
}

// TestGetJSON_routes_retry_logs_to_client_logger verifies that getJSON wires
// the client's logger into httpx via WithLogger: httpx's per-attempt retry
// diagnostics must land on the injected logger, not the global slog.Default().
// The server returns one 503 (retried) then 200, so httpx logs one Debug
// "will retry" line, which must appear in the client logger's buffer.
func TestGetJSON_routes_retry_logs_to_client_logger(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // retried by httpx
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewClient(httpx.NewClient(5*time.Second), "tok",
		[]httpx.GetOption{httpx.WithBaseDelay(time.Millisecond)}, logger)
	c.baseURL = srv.URL

	if _, err := c.ListRepos(context.Background(), "cplieger"); err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if !strings.Contains(buf.String(), "will retry") {
		t.Errorf("client logger did not capture httpx retry log (WithLogger not wired?); log=%q", buf.String())
	}
}

// TestListCodeScanningAlertsRuleDescriptionFallback pins the cmp.Or fallback in
// ListCodeScanningAlerts: when an alert's rule.id is empty (some tools populate
// only rule.description), Rule falls back to the description rather than
// emitting an empty rule name. Every other code-scanning test sets rule.id, so
// a mutant dropping the cmp.Or fallback to a bare a.Rule.ID survives; this
// exercises the empty-id branch.
func TestListCodeScanningAlertsRuleDescriptionFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"number":5,"created_at":"2026-06-20T10:00:00Z","html_url":"https://github.com/cplieger/a/security/code-scanning/5","rule":{"id":"","description":"Hard-coded credentials","security_severity_level":"high"},"tool":{"name":"CodeQL"}}
		]`))
	}))
	defer srv.Close()

	alerts, err := newTestClient(t, srv).ListCodeScanningAlerts(context.Background(), model.Repo{Owner: "cplieger", Name: "a"})
	if err != nil {
		t.Fatalf("ListCodeScanningAlerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	if alerts[0].Rule != "Hard-coded credentials" {
		t.Errorf("Rule = %q, want the rule.description fallback (rule.id was empty)", alerts[0].Rule)
	}
}

// TestListReposOwnerMatchIsCaseInsensitive pins the strings.EqualFold owner
// match in ListRepos: GitHub's API can return the owner login in a different
// case than the configured GITHUB_OWNER, so a mixed-case configured owner must
// still keep a lowercased API login. Every other ListRepos test uses matching
// lowercase, so a mutant swapping EqualFold for == survives; this exercises the
// case-mismatch path (mirroring the collector's TestKeepIsCaseInsensitiveOnOwner
// contract at the client layer).
func TestListReposOwnerMatchIsCaseInsensitive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"keep","owner":{"login":"cplieger"},"private":false,"archived":false}]`))
	}))
	defer srv.Close()

	repos, err := newTestClient(t, srv).ListRepos(context.Background(), "Cplieger")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("got %d repos, want 1 (a mixed-case configured owner must match a lowercased API login)", len(repos))
	}
}
