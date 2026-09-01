package github

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/github-scout/internal/ghsignal"
	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/slogx/capture"
)

// newTestClient wires a Client at the test server's URL with a short-timeout
// http.Client so tests never hang.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return newTestClientWithLogger(t, srv, slog.Default())
}

// newTestClientWithLogger is newTestClient with a caller-supplied logger, for
// the paths whose only observable output is the log line they emit.
func newTestClientWithLogger(t *testing.T, srv *httptest.Server, logger *slog.Logger) *Client {
	t.Helper()
	c := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "test-token", Logger: logger})
	c.baseURL = srv.URL
	return c
}

func TestListReposFiltersOwnerAndArchived(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("api version header = %q, want %q", got, "2022-11-28")
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

	repos, err := newTestClient(t, srv).ListRepos(t.Context(), "cplieger")
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

// TestListReposCarriesForkFlag pins that the fork bit reaches the domain type
// (the code-scanning skip depends on it) and that a fork is discovered, not
// filtered out — it still keeps its runs, PR and issue signals.
func TestListReposCarriesForkFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"own","owner":{"login":"cplieger"},"fork":false},
			{"name":"forked","owner":{"login":"cplieger"},"fork":true}
		]`))
	}))
	defer srv.Close()

	repos, err := newTestClient(t, srv).ListRepos(t.Context(), "cplieger")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2 (a fork is discovered, not filtered)", len(repos))
	}
	forks := map[string]bool{}
	for _, r := range repos {
		forks[r.Name] = r.Fork
	}
	if forks["own"] {
		t.Errorf("ListRepos(own).Fork = true, want false")
	}
	if !forks["forked"] {
		t.Errorf("ListRepos(forked).Fork = false, want true")
	}
}

func TestListReposPaginates(t *testing.T) {
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

	repos, err := newTestClient(t, srv).ListRepos(t.Context(), "cplieger")
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
	var queries int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries++
		if got := r.URL.Query().Get("status"); got != "completed" {
			t.Errorf("status = %q, want completed", got)
		}
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

	repo := ghsignal.Repo{Owner: "cplieger", Name: "x"}
	runs, err := newTestClient(t, srv).ListRuns(t.Context(), repo, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if queries != 1 {
		t.Errorf("made %d queries, want 1 (single status=completed call)", queries)
	}
	if len(runs) != 3 {
		t.Fatalf("got %d runs, want 3 (all conclusions, not just failures)", len(runs))
	}
	byConclusion := map[string]ghsignal.WorkflowRun{}
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

	repo := ghsignal.Repo{Owner: "cplieger", Name: "x"}
	runs, err := newTestClient(t, srv).ListRuns(t.Context(), repo, time.Now().Add(-24*time.Hour))
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
	c := NewClient(Options{HTTP: httpx.NewClient(time.Second), Token: "tok", Logger: slog.Default()})
	if _, err := c.ListRepos(t.Context(), "../evil"); err == nil {
		t.Errorf("ListRepos accepted unsafe owner")
	}
	bad := ghsignal.Repo{Owner: "ok", Name: "../evil"}
	if _, err := c.ListRuns(t.Context(), bad, time.Now()); err == nil {
		t.Errorf("ListRuns accepted unsafe repo name")
	}
}

// itoa is a tiny stdlib-free int formatter for the pagination fixture.
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

	prs, err := newTestClient(t, srv).SearchOpenPRs(t.Context(), "cplieger", "-author:app/renovate")
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

	issues, err := newTestClient(t, srv).SearchOpenIssues(t.Context(), "cplieger", "-label:renovate")
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

	alerts, err := newTestClient(t, srv).ListCodeScanningAlerts(t.Context(), ghsignal.Repo{Owner: "cplieger", Name: "a"})
	if err != nil {
		t.Fatalf("ListCodeScanningAlerts: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Rule != "go/sql-injection" || alerts[0].Severity != "high" || alerts[0].Tool != "CodeQL" {
		t.Errorf("alert parsed wrong: %+v", alerts)
	}
}

// TestCodeScanning404IsNoCodeScanning: a 404 (no code scanning ever run) must
// map to the benign ghsignal.ErrNoCodeScanning sentinel, not a read failure,
// so the collector excludes it from the "blind" calculation.
func TestCodeScanning404IsNoCodeScanning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no analysis found", http.StatusNotFound)
	}))
	defer srv.Close()

	alerts, err := newTestClient(t, srv).ListCodeScanningAlerts(t.Context(), ghsignal.Repo{Owner: "cplieger", Name: "a"})
	if !errors.Is(err, ghsignal.ErrNoCodeScanning) {
		t.Errorf("404 should map to ghsignal.ErrNoCodeScanning, got: %v", err)
	}
	if len(alerts) != 0 {
		t.Errorf("404 should yield no alerts, got %d", len(alerts))
	}
}

// TestCodeScanning403IsError: a 403 (Advanced Security off, missing scope, or
// rate limit) must surface as an error, never the benign no-code-scanning
// sentinel or a silent "zero alerts" (a security false-negative).
func TestCodeScanning403IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Resource not accessible by personal access token", http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).ListCodeScanningAlerts(t.Context(), ghsignal.Repo{Owner: "cplieger", Name: "a"})
	if err == nil {
		t.Errorf("403 must surface as an error (silent zero-alerts is a security false-negative)")
	}
	if errors.Is(err, ghsignal.ErrNoCodeScanning) {
		t.Errorf("403 must NOT be mapped to the benign no-code-scanning sentinel")
	}
	// Per-repo, not org-wide: must map to neither systemic sentinel.
	if errors.Is(err, ghsignal.ErrTokenInvalid) || errors.Is(err, ghsignal.ErrRateLimited) {
		t.Errorf("403 must not map to a systemic sentinel, got: %v", err)
	}
}

// TestStatus401MapsTokenInvalid and TestStatus429MapsRateLimited pin the
// status→sentinel mapping the collector's escalation depends on: the client
// is the one place that turns an HTTP status into a ghsignal sentinel, so
// internal/collect never imports the HTTP transport to classify a failure.
func TestStatus401MapsTokenInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Bad credentials", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).ListRepos(t.Context(), "cplieger")
	if !errors.Is(err, ghsignal.ErrTokenInvalid) {
		t.Errorf("401 should map to ghsignal.ErrTokenInvalid, got: %v", err)
	}
	if errors.Is(err, ghsignal.ErrRateLimited) {
		t.Errorf("401 must not also be ErrRateLimited")
	}
}

func TestStatus429MapsRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "API rate limit exceeded", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	// One attempt: observe the post-exhaustion mapping without sitting through backoff.
	c := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "test-token", RetryOpts: []httpx.Option{httpx.WithMaxAttempts(1)}, Logger: slog.Default()})
	c.baseURL = srv.URL

	_, err := c.ListRepos(t.Context(), "cplieger")
	if !errors.Is(err, ghsignal.ErrRateLimited) {
		t.Errorf("429 should map to ghsignal.ErrRateLimited, got: %v", err)
	}
	if errors.Is(err, ghsignal.ErrTokenInvalid) {
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

// TestNewClientNilLoggerDefaults: a nil logger must fall back to slog.Default.
func TestNewClientNilLoggerDefaults(t *testing.T) {
	c := NewClient(Options{HTTP: httpx.NewClient(time.Second), Token: "tok"})
	if c.logger == nil {
		t.Errorf("NewClient with nil logger left c.logger nil; want slog.Default fallback")
	}
}

// jsonList renders n comma-joined JSON objects from itemFmt (a single %d
// placeholder gets a per-item id).
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
// and a short page otherwise, so a client must fetch exactly maxPages pages and
// cannot loop forever past the cap. wrap formats the envelope around the item list.
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

	logger, rec := capture.New()
	repos, err := newTestClientWithLogger(t, srv, logger).ListRepos(t.Context(), "cplieger")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if requests != maxPages {
		t.Errorf("made %d page requests, want %d (must fetch up to maxPages)", requests, maxPages)
	}
	if len(repos) != maxPages*perPage {
		t.Errorf("got %d repos, want %d", len(repos), maxPages*perPage)
	}
	// The only operator signal that the scan universe was capped; must name the ceiling.
	const warning = "repo listing hit pagination bound; scan universe may be truncated"
	if got := rec.CountExact(warning); got != 1 {
		t.Errorf("ListRepos over the page cap emitted %d truncation warnings, want 1", got)
	}
	if got, ok := rec.AttrValueExact(warning, "repo_cap"); !ok || got != "500" {
		t.Errorf("truncation warning repo_cap = %q (present=%v), want %q", got, ok, "500")
	}
}

func TestListRunsStopsAtMaxPages(t *testing.T) {
	var requests int
	srv := httptest.NewServer(capPageHandler(&requests,
		func(items string) string { return `{"workflow_runs":[` + items + `]}` },
		`{"id":%d,"conclusion":"success","created_at":"2026-06-20T10:00:00Z"}`))
	defer srv.Close()

	runs, err := newTestClient(t, srv).ListRuns(t.Context(),
		ghsignal.Repo{Owner: "cplieger", Name: "x"}, time.Now().Add(-24*time.Hour))
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

	logger, rec := capture.New()
	prs, err := newTestClientWithLogger(t, srv, logger).SearchOpenPRs(t.Context(), "cplieger", "")
	if err != nil {
		t.Fatalf("SearchOpenPRs: %v", err)
	}
	if requests != maxPages {
		t.Errorf("made %d page requests, want %d", requests, maxPages)
	}
	if len(prs) != maxPages*perPage {
		t.Errorf("got %d PRs, want %d", len(prs), maxPages*perPage)
	}
	// Only signal distinguishing a partial snapshot from a complete one; must name the ceiling.
	const warning = "search hit pagination bound; snapshot may be truncated"
	if got := rec.CountExact(warning); got != 1 {
		t.Errorf("search over the page cap emitted %d truncation warnings, want 1", got)
	}
	if got, ok := rec.AttrValueExact(warning, "item_cap"); !ok || got != "500" {
		t.Errorf("truncation warning item_cap = %q (present=%v), want %q", got, ok, "500")
	}
}

func TestListCodeScanningAlertsStopsAtMaxPages(t *testing.T) {
	var requests int
	srv := httptest.NewServer(capPageHandler(&requests,
		func(items string) string { return "[" + items + "]" },
		`{"number":%d,"rule":{"id":"go/x"},"tool":{"name":"CodeQL"}}`))
	defer srv.Close()

	alerts, err := newTestClient(t, srv).ListCodeScanningAlerts(t.Context(),
		ghsignal.Repo{Owner: "cplieger", Name: "a"})
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

// TestCodeScanningNotFound pins the 404-vs-everything-else classifier: a
// couldn't-check (transient or decode error) must never read as confirmed-clean,
// so only a 404 StatusError maps to true.
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

// TestUnsafeSegmentsRejectedSearchAndCodeScanning: a traversal/injection
// segment (../evil) must be rejected before URL construction in
// SearchOpenPRs, SearchOpenIssues, and ListCodeScanningAlerts.
func TestUnsafeSegmentsRejectedSearchAndCodeScanning(t *testing.T) {
	c := NewClient(Options{HTTP: httpx.NewClient(time.Second), Token: "tok", Logger: slog.Default()})

	if _, err := c.SearchOpenPRs(t.Context(), "../evil", ""); err == nil {
		t.Errorf("SearchOpenPRs accepted unsafe owner")
	}
	if _, err := c.SearchOpenIssues(t.Context(), "../evil", ""); err == nil {
		t.Errorf("SearchOpenIssues accepted unsafe owner")
	}
	bad := ghsignal.Repo{Owner: "ok", Name: "../evil"}
	if _, err := c.ListCodeScanningAlerts(t.Context(), bad); err == nil {
		t.Errorf("ListCodeScanningAlerts accepted unsafe repo name")
	}
}

// TestSearchIncompleteResultsErrors: GitHub's incomplete_results flag means
// the search timed out server-side, so the set is partial and must error
// rather than read as a confirmed-empty/complete result.
func TestSearchIncompleteResultsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"incomplete_results":true,"items":[
			{"number":1,"repository_url":"https://api.github.com/repos/cplieger/a","user":{"login":"cplieger"}}
		]}`))
	}))
	defer srv.Close()

	prs, err := newTestClient(t, srv).SearchOpenPRs(t.Context(), "cplieger", "")
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

// TestCodeScanning404MidPaginationIsRealError: a 404 is the benign
// ghsignal.ErrNoCodeScanning ONLY before any alert is collected. A 404 on
// page 2+ is a real read failure and must surface as an error, never be
// swallowed (which would drop the alerts already read and report a false clean).
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

	_, err := newTestClient(t, srv).ListCodeScanningAlerts(t.Context(), ghsignal.Repo{Owner: "cplieger", Name: "a"})
	if err == nil {
		t.Fatalf("a 404 mid-pagination (after alerts were collected) must be a real error, not silently swallowed as no-code-scanning")
	}
	if errors.Is(err, ghsignal.ErrNoCodeScanning) {
		t.Errorf("a mid-pagination 404 must NOT map to ghsignal.ErrNoCodeScanning: err = %v", err)
	}
}

// TestGetJSONSurfacesDecodeError: getJSON is the single point where all five
// API reads decode bytes from GitHub, so a malformed body must surface as a
// "decode response" error, never a zero-value confirmed-empty result.
func TestGetJSONSurfacesDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items": [ this is not valid json `))
	}))
	defer srv.Close()

	prs, err := newTestClient(t, srv).SearchOpenPRs(t.Context(), "cplieger", "")
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

// TestListRunsReturnsPartialOnMidPaginationError: on a getJSON failure
// mid-pagination, ListRuns returns the runs collected so far ALONGSIDE the
// error (runs, err — not nil, err), unlike the other readers.
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

	c := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "test-token", RetryOpts: []httpx.Option{httpx.WithMaxAttempts(1)}, Logger: slog.Default()})
	c.baseURL = srv.URL

	runs, err := c.ListRuns(t.Context(), ghsignal.Repo{Owner: "cplieger", Name: "x"}, time.Now().Add(-24*time.Hour))
	if err == nil {
		t.Fatalf("ListRuns must error when a page fetch fails mid-pagination")
	}
	if len(runs) != perPage {
		t.Errorf("got %d runs, want %d (the page-1 set must be returned alongside the error, not dropped)", len(runs), perPage)
	}
}

// TestGetJSON_routes_retry_logs_to_client_logger: getJSON must wire the
// client's logger into httpx via WithLogger, not the global slog.Default().
func TestGetJSON_routes_retry_logs_to_client_logger(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewClient(Options{HTTP: httpx.NewClient(5 * time.Second), Token: "tok", RetryOpts: []httpx.Option{httpx.WithBaseDelay(time.Millisecond)}, Logger: logger})
	c.baseURL = srv.URL

	if _, err := c.ListRepos(t.Context(), "cplieger"); err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if !strings.Contains(buf.String(), "failed, retrying") {
		t.Errorf("client logger did not capture httpx retry log (WithLogger not wired?); log=%q", buf.String())
	}
}

// TestListCodeScanningAlertsRuleDescriptionFallback: when rule.id is empty
// (some tools populate only rule.description), Rule falls back to the
// description rather than an empty name.
func TestListCodeScanningAlertsRuleDescriptionFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"number":5,"created_at":"2026-06-20T10:00:00Z","html_url":"https://github.com/cplieger/a/security/code-scanning/5","rule":{"id":"","description":"Hard-coded credentials","security_severity_level":"high"},"tool":{"name":"CodeQL"}}
		]`))
	}))
	defer srv.Close()

	alerts, err := newTestClient(t, srv).ListCodeScanningAlerts(t.Context(), ghsignal.Repo{Owner: "cplieger", Name: "a"})
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

// TestListReposOwnerMatchIsCaseInsensitive: GitHub's API can return the owner
// login in a different case than the configured GITHUB_OWNER, so the match
// must be case-insensitive (mirrors the collector's TestKeepIsCaseInsensitiveOnOwner).
func TestListReposOwnerMatchIsCaseInsensitive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"keep","owner":{"login":"cplieger"},"private":false,"archived":false}]`))
	}))
	defer srv.Close()

	repos, err := newTestClient(t, srv).ListRepos(t.Context(), "Cplieger")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("got %d repos, want 1 (a mixed-case configured owner must match a lowercased API login)", len(repos))
	}
}
