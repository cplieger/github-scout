//nolint:goconst // wire bodies spell each product's JSON keys inline, as its API docs do
package forgeconntest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const glREST = "/api/v4"

// gitlabStatus spells a run outcome as a pipeline status.
func gitlabStatus(outcome string) string {
	switch outcome {
	case "failure":
		return "failed"
	case "cancelled":
		return "canceled"
	case "pending":
		return "running"
	default:
		return outcome
	}
}

func (s *Server) serveGitLab(w http.ResponseWriter, r *http.Request, line string) (string, bool) {
	h := w.Header()
	h.Set("X-Gitlab-Meta", `{"correlation_id":"fake-correlation","version":"1"}`)
	remaining, reset := s.budget()
	h.Set("RateLimit-Remaining", itoa(remaining))
	h.Set("RateLimit-Reset", itoa(int(reset.Unix())))
	if r.Method == http.MethodPost && r.URL.Path == "/api/graphql" {
		return s.serveGitLabGraphQL(w, r)
	}
	escaped := r.URL.EscapedPath()
	if r.Method != http.MethodGet || !strings.HasPrefix(escaped, glREST+"/") {
		return line, false
	}
	path := strings.TrimPrefix(escaped, glREST)
	switch path {
	case "/metadata":
		writeJSON(w, http.StatusOK, map[string]any{"version": "18.4.0", "enterprise": false, "revision": "fake"})
		return line, true
	case "/user":
		writeJSON(w, http.StatusOK, map[string]any{"username": s.world.Login, "name": s.world.Login, "web_url": webURL(s.URL, s.world.Login)})
		return line, true
	}
	if handled, ok := s.gitlabNamespace(w, r, path); ok {
		return line, handled
	}
	if rest, ok := strings.CutPrefix(path, "/projects/"); ok {
		return line, s.gitlabProject(w, r, rest)
	}
	return line, false
}

// gitlabNamespace serves the owner's user and group routes; ok reports a
// route that names the owner.
func (s *Server) gitlabNamespace(w http.ResponseWriter, r *http.Request, path string) (handled, ok bool) {
	if rest, found := strings.CutPrefix(path, "/users/"); found {
		id, tail, _ := strings.Cut(rest, "/")
		if tail != "projects" || !s.world.UserNamespace || !s.addresses(id, glUserID) {
			return false, false
		}
		return s.gitlabGroup(w, r, "projects"), true
	}
	rest, found := strings.CutPrefix(path, "/groups/")
	if !found {
		return false, false
	}
	id, tail, _ := strings.Cut(rest, "/")
	if !s.addresses(id, glGroupID) {
		return false, false
	}
	if s.world.UserNamespace {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Group Not Found"})
		return true, true
	}
	return s.gitlabGroup(w, r, tail), true
}

// The ids the owner's group and user answer to. A route reads an owner named
// by digits alone as an id, so forgeapi resolves such a name to one first.
const (
	glGroupID = "4242"
	glUserID  = "77"
)

// addresses reports a route segment naming the owner: its id when the owner
// is digits alone, else its escaped name.
func (s *Server) addresses(segment, id string) bool {
	if strings.Trim(s.world.Owner, "0123456789") == "" {
		return segment == id
	}
	return segment == url.PathEscape(s.world.Owner)
}

// serveGitLabGraphQL answers the owner lookup and refuses every other
// document, as an instance whose GraphQL schema lacks a field they select:
// forgeapi then reads REST for the connection's life, the arm this fake serves.
func (s *Server) serveGitLabGraphQL(w http.ResponseWriter, r *http.Request) (string, bool) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req graphQLRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "POST graphql <undecodable>", false
	}
	if req.OperationName != "OwnerLookup" {
		writeJSON(w, http.StatusOK, map[string]any{"errors": []any{map[string]any{
			"message": "Field 'queryComplexity' doesn't exist on type 'Query'", "extensions": map[string]any{"code": "undefinedField"},
		}}})
		return "POST graphql <refused>", true
	}
	var group, user any
	if req.Variables["fullPath"] == s.world.Owner && !s.world.UserNamespace {
		group = map[string]any{"id": "gid://gitlab/Group/" + glGroupID}
	}
	if req.Variables["username"] == s.world.Owner && s.world.UserNamespace {
		user = map[string]any{"id": "gid://gitlab/User/" + glUserID}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"group": group, "user": user, "queryComplexity": map[string]any{"score": 3, "limit": 200},
	}})
	return "POST graphql OwnerLookup", true
}

func (s *Server) gitlabGroup(w http.ResponseWriter, r *http.Request, collection string) bool {
	rows := []map[string]any{}
	switch collection {
	case "projects":
		for i := range s.world.Repos {
			rows = append(rows, s.gitlabProjectRow(&s.world.Repos[i]))
		}
	case "merge_requests", "issues":
		for i := range s.world.Repos {
			rows = append(rows, s.gitlabItemRows(&s.world.Repos[i], collection == "merge_requests")...)
		}
	default:
		return false
	}
	if pageOf(r) != 1 {
		rows = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, rows)
	return true
}

func (s *Server) gitlabItemRows(repo *Repo, mr bool) []map[string]any {
	items := repo.Issues
	if mr {
		items = repo.PRs
	}
	rows := make([]map[string]any, 0, len(items))
	for i := range items {
		rows = append(rows, s.gitlabItemRow(repo, &items[i], mr))
	}
	return rows
}

func (s *Server) gitlabProject(w http.ResponseWriter, r *http.Request, rest string) bool {
	encoded, tail, _ := strings.Cut(rest, "/")
	full, err := url.PathUnescape(encoded)
	if err != nil {
		return false
	}
	name, ok := strings.CutPrefix(full, s.world.Owner+"/")
	if !ok {
		return false
	}
	repo, ok := s.repo(name)
	if !ok {
		return false
	}
	rows := []map[string]any{}
	switch {
	case tail == "pipelines":
		rows = s.gitlabPipelines(r, repo)
	case strings.HasPrefix(tail, "repository/commits/") && strings.HasSuffix(tail, "/statuses"):
		sha := strings.TrimSuffix(strings.TrimPrefix(tail, "repository/commits/"), "/statuses")
		if state, ok := repo.Checks[sha]; ok {
			rows = append(rows, map[string]any{"name": "ci", "description": "", "target_url": "", "status": gitlabStatus(state), "sha": sha})
		}
	case tail == "merge_requests" || tail == "issues":
		rows = s.gitlabItemRows(repo, tail == "merge_requests")
	default:
		return false
	}
	if pageOf(r) != 1 {
		rows = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, rows)
	return true
}

func (s *Server) gitlabProjectRow(repo *Repo) map[string]any {
	row := map[string]any{
		"id": 1000 + len(repo.Name), "path_with_namespace": s.fullName(repo), "description": "",
		"web_url": webURL(s.URL, s.fullName(repo)), "http_url_to_repo": webURL(s.URL, s.fullName(repo)+".git"),
		"last_activity_at": stamp(time.Now()), "visibility": map[bool]string{true: "private", false: "public"}[repo.Private],
		"default_branch": repo.DefaultBranch, "issues_enabled": true, "issues_access_level": "enabled",
		"merge_method": "merge", "squash_option": "default_off", "archived": repo.Archived,
	}
	if repo.Fork {
		row["forked_from_project"] = map[string]any{"path_with_namespace": "upstream/" + repo.Name}
	}
	return row
}

func (s *Server) gitlabItemRow(repo *Repo, it *Item, mr bool) map[string]any {
	sigil, path := "#", "/-/issues/"
	if mr {
		sigil, path = "!", "/-/merge_requests/"
	}
	row := map[string]any{
		"iid": it.Number, "project_id": 1, "state": "opened", "title": it.Title, "description": "",
		"author": map[string]any{"username": it.Author}, "web_url": webURL(s.URL, s.fullName(repo)+path+itoa(it.Number)),
		"created_at": stamp(it.Created), "updated_at": stamp(it.Updated), "labels": it.Labels,
		"references": map[string]any{"full": s.fullName(repo) + sigil + itoa(it.Number)},
	}
	if mr {
		row["draft"] = it.Draft
		row["sha"] = it.HeadSHA
		row["source_branch"] = "feature"
		row["target_branch"] = repo.DefaultBranch
		row["source_project_id"] = 1
		row["target_project_id"] = 1
		row["merge_status"] = "can_be_merged"
		row["detailed_merge_status"] = "mergeable"
	}
	return row
}

func (s *Server) gitlabPipelines(r *http.Request, repo *Repo) []map[string]any {
	runs := runsHighestIDFirst(repo, since(r, "created_after"))
	rows := make([]map[string]any, 0, len(runs))
	for i := range runs {
		run := &runs[i]
		rows = append(rows, map[string]any{
			"id": run.ID, "iid": run.Number, "project_id": 1, "name": run.Workflow, "ref": run.Branch,
			"sha": "0000000000000000000000000000000000000001", "status": gitlabStatus(run.Outcome), "source": run.Event,
			"created_at": stamp(run.Created), "updated_at": stamp(run.Updated),
			"web_url": webURL(s.URL, s.fullName(repo)+"/-/pipelines/"+itoa(int(run.ID))),
		})
	}
	return rows
}
