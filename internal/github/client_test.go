package github

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func TestListFailedRunsQueriesEachConclusion(t *testing.T) {
	seenStatuses := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		seenStatuses[status] = true
		// The created filter must carry the >= operator (URL-decoded here).
		if c := r.URL.Query().Get("created"); !strings.HasPrefix(c, ">=") {
			t.Errorf("created filter = %q, want >= prefix", c)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workflow_runs":[
			{"id":1,"name":"CI","head_branch":"main","run_number":42,"event":"push","conclusion":"` + status + `","html_url":"https://github.com/cplieger/x/actions/runs/1","created_at":"2026-06-20T10:00:00Z"}
		]}`))
	}))
	defer srv.Close()

	repo := model.Repo{Owner: "cplieger", Name: "x"}
	runs, err := newTestClient(t, srv).ListFailedRuns(context.Background(), repo, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ListFailedRuns: %v", err)
	}
	for _, c := range model.FailureConclusions {
		if !seenStatuses[c] {
			t.Errorf("conclusion %q was never queried", c)
		}
	}
	if len(runs) != len(model.FailureConclusions) {
		t.Errorf("got %d runs, want %d (one per conclusion)", len(runs), len(model.FailureConclusions))
	}
	if runs[0].Repo != "cplieger/x" || runs[0].RunNumber != 42 {
		t.Errorf("run not parsed correctly: %+v", runs[0])
	}
}

func TestListFailedRunsPartialFailure(t *testing.T) {
	// "failure" status returns valid data; the others return a 500. The
	// client must surface an error AND keep the runs it did collect.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") == "failure" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"workflow_runs":[{"id":7,"name":"CI","conclusion":"failure","created_at":"2026-06-20T10:00:00Z"}]}`))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	repo := model.Repo{Owner: "cplieger", Name: "x"}
	runs, err := newTestClient(t, srv).ListFailedRuns(context.Background(), repo, time.Now().Add(-1*time.Hour))
	if err == nil {
		t.Errorf("expected partial-failure error, got nil")
	}
	if len(runs) != 1 || runs[0].RunID != 7 {
		t.Errorf("expected to keep the 1 successful run, got %+v", runs)
	}
}

func TestUnsafeSegmentsRejected(t *testing.T) {
	c := NewClient(httpx.NewClient(time.Second), "tok", nil, slog.Default())
	if _, err := c.ListRepos(context.Background(), "../evil"); err == nil {
		t.Errorf("ListRepos accepted unsafe owner")
	}
	bad := model.Repo{Owner: "ok", Name: "../evil"}
	if _, err := c.ListFailedRuns(context.Background(), bad, time.Now()); err == nil {
		t.Errorf("ListFailedRuns accepted unsafe repo name")
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

func TestCodeScanningNotEnabledIsNotError(t *testing.T) {
	// Repos without code scanning return 404/403 — the client maps these to
	// an empty result, not an error, so a repo lacking CodeQL is silent.
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no code scanning", status)
		}))
		alerts, err := newTestClient(t, srv).ListCodeScanningAlerts(context.Background(), model.Repo{Owner: "cplieger", Name: "a"})
		srv.Close()
		if err != nil {
			t.Errorf("status %d should be tolerated, got error: %v", status, err)
		}
		if len(alerts) != 0 {
			t.Errorf("status %d should yield no alerts, got %d", status, len(alerts))
		}
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
