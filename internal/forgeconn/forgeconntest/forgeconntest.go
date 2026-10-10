// Package forgeconntest serves fake forge instances over HTTP for tests and
// fixture generation: one World of repositories, runs, pull requests and
// issues, answered in the wire shape of GitHub, GitLab, Gitea or Forgejo,
// each written from that product's published API shape. Every request is
// recorded; one the fake does not serve answers 404 and is listed by
// Unhandled.
//
//nolint:goconst // wire bodies spell each product's JSON keys inline, as its API docs do
package forgeconntest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/github-scout/internal/config"
)

// Product is the product a fake answers as.
type Product string

// The products.
const (
	GitHub  Product = "github"
	GitLab  Product = "gitlab"
	Gitea   Product = "gitea"
	Forgejo Product = "forgejo"
)

// World is what a fake instance holds. The zero Remaining reports 4900.
type World struct {
	Login string
	Owner string
	Repos []Repo
	// Remaining is the request budget each response reports (none on Gitea).
	Remaining int
	// UserNamespace makes the owner a GitLab user rather than a group: the
	// group routes answer 404 and the user's project route answers.
	UserNamespace bool
	// GraphQLErrors answers every GraphQL search document with data beside an
	// error, which forgeapi marks as a partial answer.
	GraphQLErrors bool
	// RejectToken answers the account route 401, as for a revoked token,
	// while the detection routes still answer.
	RejectToken bool
}

// Repo is one repository under the World's owner.
type Repo struct {
	// Checks maps a head commit to its combined state: success, failure or pending.
	Checks        map[string]string
	Name          string
	DefaultBranch string
	Runs          []Run
	PRs           []Item
	Issues        []Item
	// Alerts nil answers the code-scanning route 404 (no analyses).
	Alerts []Alert
	// AlertsRedirect, when set, answers the code-scanning route 302 to it.
	AlertsRedirect string
	// RunsForbidden answers the GitHub run listing 403 with budget left and
	// no Retry-After, as a token without Actions read gets.
	Workflows     []Workflow
	RunsForbidden bool
	Private       bool
	Archived      bool
	Fork          bool
}

// Run is one CI run. Outcome is success, failure, cancelled, pending or
// approval (a fork run waiting for a maintainer, GitHub only).
type Run struct {
	Created  time.Time
	Started  time.Time
	Updated  time.Time
	Workflow string
	Branch   string
	Event    string
	Outcome  string
	ID       int64
	Number   int64
}

// Item is one open pull request or issue.
type Item struct {
	Created time.Time
	Updated time.Time
	Title   string
	Author  string
	HeadSHA string
	Labels  []string
	Number  int
	Draft   bool
}

// Server is one fake instance.
type Server struct {
	*httptest.Server
	// reset is when the reported budget renews; zero is an hour after each
	// response.
	reset     time.Time
	product   Product
	requests  []string
	unhandled []string
	world     World
	mu        sync.Mutex
}

// New starts a fake instance of product serving w. The caller closes it.
func New(product Product, w World) *Server {
	if w.Remaining == 0 {
		w.Remaining = 4900
	}
	s := &Server{product: product, world: w}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Connection is a config.Connection pointing at the fake, owner-scoped to the
// World's owner, with private addresses and plaintext allowed.
func (s *Server) Connection(name string) config.Connection {
	return config.Connection{
		Name: name, URL: s.URL, Token: "test-token", Owners: []string{s.world.Owner},
		PrivateAddresses: true, AllowPlaintext: true,
		Security: config.Security{Enabled: true, SkipForks: true, SkipRepos: map[string]bool{}},
	}
}

// SetBudget changes the budget later responses report.
func (s *Server) SetBudget(remaining int, reset time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.world.Remaining, s.reset = remaining, reset
}

// budget is the budget a response reports now.
func (s *Server) budget() (remaining int, reset time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reset.IsZero() {
		return s.world.Remaining, time.Now().Add(time.Hour)
	}
	return s.world.Remaining, s.reset
}

// Requests lists every request served, as "METHOD path?query" (a GraphQL
// request as "POST graphql <operation>").
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// Unhandled lists the requests the fake did not serve.
func (s *Server) Unhandled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.unhandled...)
}

func (s *Server) record(line string, handled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, line)
	if !handled {
		s.unhandled = append(s.unhandled, line)
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	line := r.Method + " " + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}
	var handled bool
	if s.foreignProbe(r) {
		s.record(line, true)
		http.Error(w, "Not found.", http.StatusNotFound)
		return
	}
	if s.world.RejectToken && r.URL.Path == s.accountRoute() {
		s.record(line, true)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
		return
	}
	switch s.product {
	case GitHub:
		line, handled = s.serveGitHub(w, r, line)
	case GitLab:
		line, handled = s.serveGitLab(w, r, line)
	default:
		handled = s.serveGitea(w, r)
	}
	s.record(line, handled)
	if !handled {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
}

// foreignProbe reports another family's detection read, which a real
// instance of this product answers 404: forgeapi asks GitLab's metadata route,
// then GitHub's, before the Gitea family's version route.
func (s *Server) foreignProbe(r *http.Request) bool {
	switch r.URL.Path {
	case "/api/v4/metadata":
		return s.product != GitLab
	case "/api/v3/meta":
		return s.product != GitHub
	}
	return false
}

func (s *Server) accountRoute() string {
	switch s.product {
	case GitHub:
		return ghREST + "/user"
	case GitLab:
		return glREST + "/user"
	default:
		return gtREST + "/user"
	}
}

func (s *Server) repo(name string) (*Repo, bool) {
	for i := range s.world.Repos {
		if strings.EqualFold(s.world.Repos[i].Name, name) {
			return &s.world.Repos[i], true
		}
	}
	return nil, false
}

func (s *Server) fullName(r *Repo) string { return s.world.Owner + "/" + r.Name }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func stamp(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// since parses a time query parameter, the zero time when absent.
func since(r *http.Request, key string) time.Time {
	raw := strings.TrimPrefix(r.URL.Query().Get(key), ">=")
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// runsHighestIDFirst returns the repository's runs created at or after bound,
// newest id first.
func runsHighestIDFirst(repo *Repo, bound time.Time) []Run {
	out := make([]Run, 0, len(repo.Runs))
	for i := range repo.Runs {
		if bound.IsZero() || !repo.Runs[i].Created.Before(bound) {
			out = append(out, repo.Runs[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

func labelObjects(names []string, color string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n, "color": color, "description": ""})
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

func pageOf(r *http.Request) int {
	p, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || p < 1 {
		return 1
	}
	return p
}

func webURL(base, path string) string { return fmt.Sprintf("%s/%s", base, path) }
