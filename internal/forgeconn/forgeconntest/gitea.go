//nolint:goconst // wire bodies spell each product's JSON keys inline, as its API docs do
package forgeconntest

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const gtREST = "/api/v1"

func (s *Server) serveGitea(w http.ResponseWriter, r *http.Request) bool {
	if s.product == Forgejo {
		remaining, reset := s.budget()
		w.Header().Set("RateLimit", `"default";r=`+itoa(remaining)+`;t=`+itoa(max(int(time.Until(reset).Seconds()), 0)))
	}
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case "/swagger.v1.json":
		s.giteaSwagger(w)
		return true
	case gtREST + "/version":
		writeJSON(w, http.StatusOK, map[string]any{"version": s.giteaVersion()})
		return true
	case gtREST + "/settings/api":
		writeJSON(w, http.StatusOK, map[string]any{"max_response_items": 50})
		return true
	case gtREST + "/user":
		writeJSON(w, http.StatusOK, map[string]any{"login": s.world.Login, "full_name": s.world.Login, "html_url": webURL(s.URL, s.world.Login)})
		return true
	case gtREST + "/users/" + s.world.Owner + "/repos":
		s.giteaRepos(w, r)
		return true
	case gtREST + "/repos/issues/search":
		return s.giteaSearch(w, r)
	}
	return s.giteaRepoRoute(w, r)
}

func (s *Server) giteaRepoRoute(w http.ResponseWriter, r *http.Request) bool {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, gtREST+"/repos/"), "/")
	if !strings.HasPrefix(r.URL.Path, gtREST+"/repos/") || len(parts) < 3 || parts[0] != s.world.Owner {
		return false
	}
	repo, ok := s.repo(parts[1])
	if !ok {
		return false
	}
	switch tail := strings.Join(parts[2:], "/"); {
	case tail == "actions/runs":
		s.giteaRuns(w, r, repo)
	case strings.HasPrefix(tail, "commits/") && strings.HasSuffix(tail, "/status"):
		sha := strings.TrimSuffix(strings.TrimPrefix(tail, "commits/"), "/status")
		state, ok := repo.Checks[sha]
		statuses := []map[string]any{}
		if ok {
			statuses = append(statuses, map[string]any{"context": "ci", "description": "", "target_url": "", "status": state})
		}
		writeJSON(w, http.StatusOK, map[string]any{"sha": sha, "state": state, "total_count": len(statuses), "statuses": statuses})
	default:
		return false
	}
	return true
}

func (s *Server) giteaVersion() string {
	if s.product == Forgejo {
		return "16.0.0+gitea-1.22.0"
	}
	return "1.27.0"
}

func (s *Server) giteaSwagger(w http.ResponseWriter) {
	title, paths := "Gitea API", map[string]any{
		"/repos/{owner}/{repo}/actions/runs": map[string]any{"get": map[string]any{"operationId": "getWorkflowRuns", "parameters": []any{map[string]any{"name": "head_sha", "in": "query"}}}},
	}
	if s.product == Forgejo {
		title = "Forgejo API"
		paths = map[string]any{"/repos/{owner}/{repo}/actions/runs/{run_id}/logs": map[string]any{"get": map[string]any{"operationId": "repoGetActionRunLogs"}}}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"swagger": "2.0", "basePath": gtREST, "info": map[string]any{"title": title, "version": s.giteaVersion()}, "paths": paths,
	})
}

func (s *Server) giteaRepos(w http.ResponseWriter, r *http.Request) {
	rows := []map[string]any{}
	if pageOf(r) == 1 {
		for i := range s.world.Repos {
			repo := &s.world.Repos[i]
			rows = append(rows, map[string]any{
				"full_name": s.fullName(repo), "name": repo.Name, "owner": map[string]any{"login": s.world.Owner},
				"description": "", "html_url": webURL(s.URL, s.fullName(repo)), "clone_url": webURL(s.URL, s.fullName(repo)+".git"),
				"updated_at": stamp(time.Now()), "private": repo.Private, "archived": repo.Archived, "fork": repo.Fork,
				"default_branch": repo.DefaultBranch, "has_issues": true, "has_pull_requests": true,
				"permissions":         map[string]any{"admin": true, "push": true, "pull": true},
				"allow_merge_commits": true, "default_merge_style": "merge",
			})
		}
	}
	w.Header().Set("X-Total-Count", itoa(len(s.world.Repos)))
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) giteaSearch(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	if q.Get("owner") != s.world.Owner {
		writeJSON(w, http.StatusOK, []any{})
		return true
	}
	prs := q.Get("type") == "pulls"
	rows := []map[string]any{}
	for i := range s.world.Repos {
		repo := &s.world.Repos[i]
		items := repo.Issues
		if prs {
			items = repo.PRs
		}
		for j := range items {
			it := &items[j]
			if pageOf(r) != 1 {
				continue
			}
			path := "/issues/"
			row := map[string]any{
				"number": it.Number, "state": "open", "title": it.Title, "body": "",
				"user": map[string]any{"login": it.Author}, "created_at": stamp(it.Created), "updated_at": stamp(it.Updated),
				"labels":     labelObjects(it.Labels, "ededed"),
				"repository": map[string]any{"full_name": s.fullName(repo), "name": repo.Name, "owner": s.world.Owner},
			}
			if prs {
				path = "/pulls/"
				row["pull_request"] = map[string]any{"draft": it.Draft, "merged": false}
			}
			row["html_url"] = webURL(s.URL, s.fullName(repo)+path+itoa(it.Number))
			rows = append(rows, row)
		}
	}
	writeJSON(w, http.StatusOK, rows)
	return true
}

// giteaRunStatus spells an outcome: Gitea's completed run carries its
// conclusion beside status completed, Forgejo's status names the outcome.
func (s *Server) giteaRunStatus(outcome string) (status, conclusion string) {
	if outcome == "pending" {
		return "running", ""
	}
	if s.product == Forgejo {
		return outcome, ""
	}
	return "completed", outcome
}

// giteaRuns answers one page of the repository's runs, highest id first, at
// the requested limit up to the stated maximum of 50, as Gitea pages its lists.
func (s *Server) giteaRuns(w http.ResponseWriter, r *http.Request, repo *Repo) {
	runs := runsHighestIDFirst(repo, time.Time{})
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit < 1 || limit > 50 {
		limit = 50
	}
	lo, hi := min((pageOf(r)-1)*limit, len(runs)), min(pageOf(r)*limit, len(runs))
	rows := make([]map[string]any, 0, hi-lo)
	for i := range runs[lo:hi] {
		run := &runs[lo+i]
		status, conclusion := s.giteaRunStatus(run.Outcome)
		row := map[string]any{
			"id": run.ID, "status": status, "conclusion": conclusion,
			"html_url": webURL(s.URL, s.fullName(repo)+"/actions/runs/"+itoa(int(run.ID))),
		}
		if s.product == Forgejo {
			row["index_in_repo"], row["workflow_id"], row["prettyref"] = run.Number, run.Workflow+".yml", run.Branch
			row["trigger_event"], row["commit_sha"] = run.Event, "0000000000000000000000000000000000000001"
			row["created"], row["started"], row["updated"] = stamp(run.Created), stamp(run.Started), stamp(run.Updated)
		} else {
			row["run_number"], row["path"], row["head_branch"] = run.Number, ".gitea/workflows/"+run.Workflow+".yml@refs/heads/"+run.Branch, run.Branch
			row["event"], row["head_sha"] = run.Event, "0000000000000000000000000000000000000001"
			row["created_at"], row["started_at"], row["updated_at"] = stamp(run.Created), stamp(run.Started), stamp(run.Updated)
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "workflow_runs": rows})
}
