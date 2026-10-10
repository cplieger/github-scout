//nolint:goconst // wire bodies spell each product's JSON keys inline, as its API docs do
package forgeconntest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitHub Enterprise Server's layout, which forgeapi takes for a loopback
// instance: REST under /api/v3 and GraphQL at /api/graphql.
const (
	ghREST    = "/api/v3"
	ghGraphQL = "/api/graphql"
)

// Alert is one open code-scanning alert (GitHub only).
type Alert struct {
	Created  time.Time
	Rule     string
	Severity string
	Tool     string
	Number   int64
}

// Workflow is one workflow definition (GitHub only). State is active,
// disabled_inactivity, disabled_manually, disabled_fork or deleted.
type Workflow struct {
	Name  string
	Path  string
	State string
	ID    int64
}

func (s *Server) serveGitHub(w http.ResponseWriter, r *http.Request, line string) (string, bool) {
	h := w.Header()
	h.Set("X-GitHub-Request-Id", "FAKE:REQUEST")
	h.Set("X-RateLimit-Limit", "5000")
	remaining, reset := s.budget()
	h.Set("X-RateLimit-Remaining", itoa(remaining))
	h.Set("X-RateLimit-Reset", itoa(int(reset.Unix())))
	if r.Method == http.MethodPost && r.URL.Path == ghGraphQL {
		return s.serveGitHubGraphQL(w, r)
	}
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, ghREST+"/") {
		return line, false
	}
	path := strings.TrimPrefix(r.URL.Path, ghREST)
	switch path {
	case "/meta":
		writeJSON(w, http.StatusOK, map[string]any{"installed_version": "3.20.0", "verifiable_password_authentication": false})
		return line, true
	case "/versions":
		writeJSON(w, http.StatusOK, []string{"2026-03-10", "2022-11-28"})
		return line, true
	case "/user":
		writeJSON(w, http.StatusOK, map[string]any{"login": s.world.Login, "name": s.world.Login, "html_url": webURL(s.URL, s.world.Login)})
		return line, true
	}
	parts := strings.Split(strings.TrimPrefix(path, "/repos/"), "/")
	if !strings.HasPrefix(path, "/repos/") || len(parts) < 3 || !strings.EqualFold(parts[0], s.world.Owner) {
		return line, false
	}
	repo, ok := s.repo(parts[1])
	if !ok {
		return line, false
	}
	return line, s.githubRepoRoute(w, r, repo, strings.Join(parts[2:], "/"))
}

// githubRepoRoute answers one repository route, reporting whether it serves
// it.
func (s *Server) githubRepoRoute(w http.ResponseWriter, r *http.Request, repo *Repo, route string) bool {
	switch route {
	case "actions/runs":
		if repo.RunsForbidden {
			writeJSON(w, http.StatusForbidden, map[string]any{"message": "Resource not accessible by personal access token"})
			return true
		}
		s.githubRuns(w, r, repo)
	case "actions/workflows":
		s.githubWorkflows(w, r, repo)
	case "code-scanning/alerts":
		s.githubAlerts(w, r, repo)
	default:
		return false
	}
	return true
}

func githubConclusion(outcome string) (status string, conclusion any) {
	switch outcome {
	case "pending":
		return "in_progress", nil
	case "approval":
		return "completed", "action_required"
	default:
		return "completed", outcome
	}
}

func (s *Server) githubRuns(w http.ResponseWriter, r *http.Request, repo *Repo) {
	runs := runsHighestIDFirst(repo, since(r, "created"))
	rows := make([]map[string]any, 0, len(runs))
	if pageOf(r) == 1 {
		for i := range runs {
			run := &runs[i]
			status, conclusion := githubConclusion(run.Outcome)
			rows = append(rows, map[string]any{
				"id": run.ID, "run_number": run.Number, "event": run.Event, "name": run.Workflow,
				"head_branch": run.Branch, "head_sha": "0000000000000000000000000000000000000001",
				"status": status, "conclusion": conclusion,
				"html_url":   webURL(s.URL, s.fullName(repo)+"/actions/runs/"+itoa(int(run.ID))),
				"created_at": stamp(run.Created), "run_started_at": stamp(run.Started), "updated_at": stamp(run.Updated),
				"repository": map[string]any{"full_name": s.fullName(repo)},
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "workflow_runs": rows})
}

func (s *Server) githubWorkflows(w http.ResponseWriter, r *http.Request, repo *Repo) {
	rows := []map[string]any{}
	if pageOf(r) == 1 {
		for _, wf := range repo.Workflows {
			rows = append(rows, map[string]any{
				"id": wf.ID, "name": wf.Name, "path": wf.Path, "state": wf.State,
				"html_url": webURL(s.URL, s.fullName(repo)+"/blob/main/"+wf.Path),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(repo.Workflows), "workflows": rows})
}

func (s *Server) githubAlerts(w http.ResponseWriter, r *http.Request, repo *Repo) {
	if repo.AlertsRedirect != "" {
		http.Redirect(w, r, repo.AlertsRedirect, http.StatusFound)
		return
	}
	if repo.Alerts == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "no analysis found"})
		return
	}
	rows := []map[string]any{}
	if pageOf(r) == 1 {
		for _, a := range repo.Alerts {
			rows = append(rows, map[string]any{
				"number": a.Number, "created_at": stamp(a.Created),
				"html_url": webURL(s.URL, s.fullName(repo)+"/security/code-scanning/"+itoa(int(a.Number))),
				"rule":     map[string]any{"id": a.Rule, "description": a.Rule, "security_severity_level": a.Severity},
				"tool":     map[string]any{"name": a.Tool},
			})
		}
	}
	writeJSON(w, http.StatusOK, rows)
}

type graphQLRequest struct {
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
	Query         string         `json:"query"`
}

func (s *Server) serveGitHubGraphQL(w http.ResponseWriter, r *http.Request) (string, bool) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req graphQLRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "POST graphql <undecodable>", false
	}
	line := "POST graphql " + req.OperationName
	remaining, reset := s.budget()
	data := map[string]any{"rateLimit": map[string]any{
		"cost": 1, "limit": 5000, "remaining": remaining, "resetAt": stamp(reset),
	}}
	var errs []map[string]any
	switch req.OperationName {
	case "RepoOwned":
		data["repositoryOwner"] = s.githubOwner(req.Variables)
	case "PRMine":
		var refused []map[string]any
		data["search"], refused = s.githubSearch(req.Variables, true)
		errs = append(s.searchErrors(), refused...)
	case "IssueMine":
		data["search"], _ = s.githubSearch(req.Variables, false)
		errs = s.searchErrors()
	case "CommitRollup":
		data["repository"], errs = s.githubRollup(req.Variables)
	default:
		return line, false
	}
	out := map[string]any{"data": data}
	if errs != nil {
		out["errors"] = errs
	}
	writeJSON(w, http.StatusOK, out)
	return line, true
}

func (s *Server) searchErrors() []map[string]any {
	if !s.world.GraphQLErrors {
		return nil
	}
	return []map[string]any{{"message": "Something went wrong while executing your query.", "path": []string{"search"}}}
}

func (s *Server) githubOwner(vars map[string]any) any {
	owner, _ := vars["owner"].(string)
	if !strings.EqualFold(owner, s.world.Owner) {
		return nil
	}
	nodes := make([]map[string]any, 0, len(s.world.Repos))
	for i := range s.world.Repos {
		repo := &s.world.Repos[i]
		nodes = append(nodes, map[string]any{
			"nameWithOwner": s.fullName(repo), "description": "", "url": webURL(s.URL, s.fullName(repo)),
			"isPrivate": repo.Private, "isArchived": repo.Archived, "isFork": repo.Fork, "updatedAt": stamp(time.Now()),
			"defaultBranchRef": map[string]any{"name": repo.DefaultBranch}, "hasIssuesEnabled": true,
			"viewerPermission": "ADMIN", "mergeCommitAllowed": true, "squashMergeAllowed": true, "rebaseMergeAllowed": true,
		})
	}
	return map[string]any{"repositories": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}, "nodes": nodes,
	}}
}

// githubSearch answers one search page, with the FORBIDDEN error of each
// pull request whose check rollup it refuses.
func (s *Server) githubSearch(vars map[string]any, prs bool) (page map[string]any, refused []map[string]any) {
	q, _ := vars["q"].(string)
	var nodes []map[string]any
	if strings.Contains(strings.ToLower(q), "user:"+strings.ToLower(s.world.Owner)+" ") {
		for i := range s.world.Repos {
			repo := &s.world.Repos[i]
			items := repo.Issues
			if prs {
				items = repo.PRs
			}
			for j := range items {
				if prs && items[j].ChecksForbidden {
					refused = append(refused, rollupRefusal("search", "nodes", len(nodes), "commits", "nodes", 0, "commit"))
				}
				nodes = append(nodes, s.githubSearchNode(repo, &items[j], prs))
			}
		}
	}
	return map[string]any{
		"issueCount": len(nodes), "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}, "nodes": nodes,
	}, refused
}

// refusedRollup is a check rollup whose contexts GitHub refused: the counts
// answered, the context rows null.
func refusedRollup() map[string]any {
	return map[string]any{"state": "SUCCESS", "contexts": map[string]any{
		"totalCount": 1, "checkRunCount": 1, "statusContextCount": 0,
		"checkRunCountsByState": []any{map[string]any{"state": "SUCCESS", "count": 1}}, "statusContextCountsByState": []any{},
		"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}, "nodes": nil,
	}}
}

// rollupRefusal is the error GitHub answers beside refusedRollup, at the
// rollup under path.
func rollupRefusal(path ...any) map[string]any {
	return map[string]any{
		"type": "FORBIDDEN", "message": "Resource not accessible by personal access token",
		"path": append(path, "statusCheckRollup", "contexts", "nodes"),
	}
}

func (s *Server) githubSearchNode(repo *Repo, it *Item, pr bool) map[string]any {
	kind, path := "Issue", "/issues/"
	if pr {
		kind, path = "PullRequest", "/pull/"
	}
	node := map[string]any{
		"__typename": kind, "number": it.Number, "title": it.Title, "body": "", "state": "OPEN",
		"url":       webURL(s.URL, s.fullName(repo)+path+itoa(it.Number)),
		"createdAt": stamp(it.Created), "updatedAt": stamp(it.Updated),
		"author":     map[string]any{"login": it.Author},
		"labels":     map[string]any{"totalCount": len(it.Labels), "nodes": labelObjects(it.Labels, "ededed")},
		"repository": map[string]any{"nameWithOwner": s.fullName(repo)},
	}
	if pr {
		node["isDraft"] = it.Draft
		node["headRefName"] = "feature"
		node["headRepository"] = map[string]any{"nameWithOwner": s.fullName(repo)}
		node["baseRefName"] = repo.DefaultBranch
		node["headRefOid"] = it.HeadSHA
		node["mergeable"] = "MERGEABLE"
		node["mergeStateStatus"] = "CLEAN"
		node["merged"] = false
		node["commits"] = map[string]any{"nodes": []any{}}
		if it.ChecksForbidden {
			node["commits"] = map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"oid": it.HeadSHA, "statusCheckRollup": refusedRollup()}}}}
		}
	}
	return node
}

func (s *Server) githubRollup(vars map[string]any) (repository any, refused []map[string]any) {
	name, _ := vars["name"].(string)
	ref, _ := vars["ref"].(string)
	repo, ok := s.repo(name)
	if !ok {
		return nil, nil
	}
	commit := map[string]any{"__typename": "Commit", "oid": ref, "statusCheckRollup": nil}
	for i := range repo.PRs {
		if repo.PRs[i].ChecksForbidden && repo.PRs[i].HeadSHA == ref {
			commit["statusCheckRollup"] = refusedRollup()
			return map[string]any{"nameWithOwner": s.fullName(repo), "object": commit}, []map[string]any{rollupRefusal("repository", "object")}
		}
	}
	if state, ok := repo.Checks[ref]; ok {
		upper := map[string]string{"success": "SUCCESS", "failure": "FAILURE", "pending": "PENDING"}[state]
		commit["statusCheckRollup"] = map[string]any{"state": upper, "contexts": map[string]any{
			"totalCount": 1, "checkRunCount": 0, "statusContextCount": 1,
			"checkRunCountsByState": []any{}, "statusContextCountsByState": []any{map[string]any{"state": upper, "count": 1}},
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
			"nodes":    []any{map[string]any{"__typename": "StatusContext", "context": "ci", "description": "", "targetUrl": "", "state": upper}},
		}}
	}
	return map[string]any{"nameWithOwner": s.fullName(repo), "object": commit}, nil
}
