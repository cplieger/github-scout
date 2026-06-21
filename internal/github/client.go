// Package github is github-scout's GitHub REST API client. It exposes
// exactly the two reads the scout needs — discover an owner's repos, and
// list a repo's failed workflow runs — over the cplieger/httpx retry
// transport. Public repos and (with a token that can see them) private
// repos are both covered, which is the whole reason this exists: the
// Grafana GitHub-datasource plugin cannot enumerate "all workflows across
// all repos", and private repos have no org-level alert endpoint.
//
// Attribution: the auth-header set and the page-count pagination approach
// follow patterns common to the MIT-licensed community GitHub exporters
// (githubexporter/github-exporter, xrstf/github_exporter). See NOTICE.
package github

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/github-scout/internal/model"
	"github.com/cplieger/github-scout/internal/urlsafe"
	"github.com/cplieger/httpx"
)

const (
	// apiBase is the GitHub REST API root. github-scout targets
	// github.com only; GitHub Enterprise would need this configurable.
	apiBase = "https://api.github.com"
	// apiVersion pins the REST API version header GitHub recommends
	// sending on every request so a future default bump can't change
	// response shapes under us.
	apiVersion = "2022-11-28"
	// perPage is the max page size the API allows; using it minimises
	// round-trips.
	perPage = 100
	// maxPages bounds pagination so a pathological repo (hundreds of
	// failures in the lookback window — itself a signal) can't make a
	// single scan unbounded. 5 pages = 500 items per query.
	maxPages = 5
	// bodyCap limits each JSON response. GitHub pages cap at 100 items;
	// 8 MB is comfortably above the largest realistic runs page while
	// rejecting a runaway/compromised response.
	bodyCap = 8 << 20
)

// Client reads the GitHub REST API. Construct via NewClient; the zero
// value is not usable.
type Client struct {
	http      *http.Client
	logger    *slog.Logger
	token     string
	baseURL   string
	retryOpts []httpx.Option
}

// NewClient returns a Client using the provided *http.Client for all
// requests, authenticating with token, and applying retryOpts to each
// call. A nil logger falls back to slog.Default.
func NewClient(client *http.Client, token string, retryOpts []httpx.Option, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{http: client, token: token, baseURL: apiBase, retryOpts: retryOpts, logger: logger}
}

// apiRepo is the subset of the /user/repos response github-scout reads.
type apiRepo struct {
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
	Private  bool `json:"private"`
	Archived bool `json:"archived"`
}

// ListRepos returns every non-archived repo owned by the authenticated
// token whose owner login equals owner. It uses /user/repos (not
// /users/{owner}/repos) so private repos are included — failed Actions
// runs in a private repo are just as actionable as public ones, and the
// public endpoint omits them. Results are filtered to owner so a token
// with org memberships doesn't pull in repos the operator didn't ask for.
func (c *Client) ListRepos(ctx context.Context, owner string) ([]model.Repo, error) {
	if !urlsafe.IsSafeURLSegment(owner) {
		return nil, fmt.Errorf("unsafe owner segment: %q", owner)
	}
	var repos []model.Repo
	for page := 1; page <= maxPages; page++ {
		q := url.Values{}
		q.Set("affiliation", "owner")
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page))
		q.Set("sort", "full_name")
		reqURL := c.baseURL + "/user/repos?" + q.Encode()

		var pageRepos []apiRepo
		if err := c.getJSON(ctx, reqURL, &pageRepos); err != nil {
			return nil, fmt.Errorf("list repos page %d: %w", page, err)
		}
		for _, r := range pageRepos {
			if r.Owner.Login != owner || r.Archived {
				continue
			}
			repos = append(repos, model.Repo{
				Owner:    r.Owner.Login,
				Name:     r.Name,
				Private:  r.Private,
				Archived: r.Archived,
			})
		}
		if len(pageRepos) < perPage {
			break // last page
		}
	}
	return repos, nil
}

// apiRunsPage is the /actions/runs response envelope.
type apiRunsPage struct {
	WorkflowRuns []apiRun `json:"workflow_runs"`
}

// apiRun is the subset of a workflow run github-scout surfaces.
type apiRun struct {
	CreatedAt  time.Time `json:"created_at"`
	Name       string    `json:"name"`
	HeadBranch string    `json:"head_branch"`
	Event      string    `json:"event"`
	Conclusion string    `json:"conclusion"`
	HTMLURL    string    `json:"html_url"`
	ID         int64     `json:"id"`
	RunNumber  int64     `json:"run_number"`
}

// ListFailedRuns returns failed workflow runs for repo created at or after
// since. It issues one query per conclusion in model.FailureConclusions
// (the Actions API filters on a single status value per request), so a
// timed-out scheduled job is caught alongside an outright build failure.
// Partial failure is surfaced to the caller: if one conclusion query
// fails the others still return, and the error is non-nil so the caller
// can log degradation without losing the runs it did collect.
func (c *Client) ListFailedRuns(ctx context.Context, repo model.Repo, since time.Time) ([]model.FailedRun, error) {
	if !urlsafe.IsSafeURLSegment(repo.Owner) || !urlsafe.IsSafeURLSegment(repo.Name) {
		return nil, fmt.Errorf("unsafe repo segment: %q", repo.FullName())
	}
	var (
		runs     []model.FailedRun
		firstErr error
	)
	for _, conclusion := range model.FailureConclusions {
		got, err := c.listRunsByStatus(ctx, repo, conclusion, since)
		if err != nil {
			c.logger.Warn("failed to list runs for conclusion",
				"repo", repo.FullName(), "conclusion", conclusion, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		runs = append(runs, got...)
	}
	return runs, firstErr
}

// listRunsByStatus pages through runs for a single status/conclusion.
func (c *Client) listRunsByStatus(ctx context.Context, repo model.Repo, status string, since time.Time) ([]model.FailedRun, error) {
	var runs []model.FailedRun
	for page := 1; page <= maxPages; page++ {
		q := url.Values{}
		q.Set("status", status)
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page))
		// GitHub accepts a date-range operator in `created`; url.Values
		// percent-encodes ">=" which the API decodes server-side.
		q.Set("created", ">="+since.UTC().Format(time.RFC3339))
		reqURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs?%s", c.baseURL, repo.Owner, repo.Name, q.Encode())

		var pageData apiRunsPage
		if err := c.getJSON(ctx, reqURL, &pageData); err != nil {
			return runs, err
		}
		for _, r := range pageData.WorkflowRuns {
			runs = append(runs, model.FailedRun{
				Repo:       repo.FullName(),
				Workflow:   r.Name,
				RunID:      r.ID,
				RunNumber:  r.RunNumber,
				Branch:     r.HeadBranch,
				Event:      r.Event,
				Conclusion: r.Conclusion,
				URL:        r.HTMLURL,
				CreatedAt:  r.CreatedAt,
			})
		}
		if len(pageData.WorkflowRuns) < perPage {
			break
		}
	}
	return runs, nil
}

// getJSON fetches reqURL with auth + version headers via the httpx retry
// transport and decodes the body into out. The body is capped so a
// runaway response can't exhaust memory.
func (c *Client) getJSON(ctx context.Context, reqURL string, out any) error {
	opts := make([]httpx.Option, 0, len(c.retryOpts)+2)
	opts = append(opts, c.retryOpts...)
	opts = append(opts,
		httpx.WithHeaders(func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+c.token)
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("X-GitHub-Api-Version", apiVersion)
		}),
		httpx.WithMaxBodyBytes(bodyCap),
	)
	body, err := httpx.Retry(ctx, c.http, reqURL, opts...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// --- Pull requests & issues (cross-repo via the Search API) ---

// apiSearchResp is the /search/issues envelope.
type apiSearchResp struct {
	Items []apiSearchItem `json:"items"`
}

// apiSearchItem is one search result. The endpoint returns both issues and
// pull requests; the `is:pr` / `is:issue` query qualifier selects which.
type apiSearchItem struct {
	CreatedAt     time.Time `json:"created_at"`
	Title         string    `json:"title"`
	HTMLURL       string    `json:"html_url"`
	RepositoryURL string    `json:"repository_url"`
	User          struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Number int64 `json:"number"`
	Draft  bool  `json:"draft"`
}

// SearchOpenPRs returns every open pull request across the owner's repos in
// a single cross-repo query. exclude is appended raw to the search query
// (e.g. "-author:app/renovate") so the caller controls noise filtering.
func (c *Client) SearchOpenPRs(ctx context.Context, owner, exclude string) ([]model.PullRequest, error) {
	items, err := c.search(ctx, "is:open is:pr", owner, exclude)
	if err != nil {
		return nil, err
	}
	prs := make([]model.PullRequest, 0, len(items))
	for i := range items {
		it := &items[i]
		prs = append(prs, model.PullRequest{
			CreatedAt: it.CreatedAt,
			Repo:      repoFromAPIURL(it.RepositoryURL),
			Title:     it.Title,
			Author:    it.User.Login,
			URL:       it.HTMLURL,
			Number:    it.Number,
			Draft:     it.Draft,
		})
	}
	return prs, nil
}

// SearchOpenIssues returns every open issue across the owner's repos in a
// single cross-repo query. exclude filters bot/auto-generated noise (e.g.
// "-author:app/renovate -label:renovate -label:auto-generated").
func (c *Client) SearchOpenIssues(ctx context.Context, owner, exclude string) ([]model.Issue, error) {
	items, err := c.search(ctx, "is:open is:issue", owner, exclude)
	if err != nil {
		return nil, err
	}
	issues := make([]model.Issue, 0, len(items))
	for i := range items {
		it := &items[i]
		labels := make([]string, 0, len(it.Labels))
		for _, l := range it.Labels {
			labels = append(labels, l.Name)
		}
		issues = append(issues, model.Issue{
			CreatedAt: it.CreatedAt,
			Repo:      repoFromAPIURL(it.RepositoryURL),
			Title:     it.Title,
			Author:    it.User.Login,
			Labels:    strings.Join(labels, ","),
			URL:       it.HTMLURL,
			Number:    it.Number,
		})
	}
	return issues, nil
}

// search runs a paginated /search/issues query. base is the qualifier
// prefix ("is:open is:pr"); owner scopes to user:<owner>; exclude is
// appended verbatim. The Search API caps at 1000 results; maxPages bounds
// our cost well below that.
func (c *Client) search(ctx context.Context, base, owner, exclude string) ([]apiSearchItem, error) {
	if !urlsafe.IsSafeURLSegment(owner) {
		return nil, fmt.Errorf("unsafe owner segment: %q", owner)
	}
	q := base + " user:" + owner
	if exclude = strings.TrimSpace(exclude); exclude != "" {
		q += " " + exclude
	}
	var items []apiSearchItem
	for page := 1; page <= maxPages; page++ {
		v := url.Values{}
		v.Set("q", q)
		v.Set("per_page", strconv.Itoa(perPage))
		v.Set("page", strconv.Itoa(page))
		reqURL := c.baseURL + "/search/issues?" + v.Encode()

		var resp apiSearchResp
		if err := c.getJSON(ctx, reqURL, &resp); err != nil {
			return items, fmt.Errorf("search %q page %d: %w", base, page, err)
		}
		items = append(items, resp.Items...)
		if len(resp.Items) < perPage {
			break
		}
	}
	return items, nil
}

// --- Code scanning alerts (per-repo) ---

// apiCodeAlert is one code-scanning alert.
type apiCodeAlert struct {
	CreatedAt time.Time `json:"created_at"`
	HTMLURL   string    `json:"html_url"`
	Rule      struct {
		ID                    string `json:"id"`
		Description           string `json:"description"`
		SecuritySeverityLevel string `json:"security_severity_level"`
	} `json:"rule"`
	Tool struct {
		Name string `json:"name"`
	} `json:"tool"`
	Number int64 `json:"number"`
}

// ListCodeScanningAlerts returns open code-scanning alerts for repo. Repos
// without code scanning enabled (private repos lacking Advanced Security,
// or repos that never configured CodeQL) return 403/404; those are NOT
// errors — the repo simply has no alerts to surface, so we return empty.
func (c *Client) ListCodeScanningAlerts(ctx context.Context, repo model.Repo) ([]model.CodeScanningAlert, error) {
	if !urlsafe.IsSafeURLSegment(repo.Owner) || !urlsafe.IsSafeURLSegment(repo.Name) {
		return nil, fmt.Errorf("unsafe repo segment: %q", repo.FullName())
	}
	var alerts []model.CodeScanningAlert
	for page := 1; page <= maxPages; page++ {
		v := url.Values{}
		v.Set("state", "open")
		v.Set("per_page", strconv.Itoa(perPage))
		v.Set("page", strconv.Itoa(page))
		reqURL := fmt.Sprintf("%s/repos/%s/%s/code-scanning/alerts?%s", c.baseURL, repo.Owner, repo.Name, v.Encode())

		var page0 []apiCodeAlert
		if err := c.getJSON(ctx, reqURL, &page0); err != nil {
			if codeScanningUnavailable(err) {
				return nil, nil // feature not enabled for this repo — not an error
			}
			return alerts, err
		}
		for i := range page0 {
			a := &page0[i]
			alerts = append(alerts, model.CodeScanningAlert{
				CreatedAt: a.CreatedAt,
				Repo:      repo.FullName(),
				Rule:      cmp.Or(a.Rule.ID, a.Rule.Description),
				Severity:  a.Rule.SecuritySeverityLevel,
				Tool:      a.Tool.Name,
				URL:       a.HTMLURL,
				Number:    a.Number,
			})
		}
		if len(page0) < perPage {
			break
		}
	}
	return alerts, nil
}

// codeScanningUnavailable reports whether err is a 403/404 — the statuses
// GitHub returns when code scanning is not enabled / not available for a
// repository, which the collector treats as "no alerts" rather than a
// failure.
func codeScanningUnavailable(err error) bool {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return se.Code == 403 || se.Code == 404
	}
	return false
}

// repoFromAPIURL extracts "owner/name" from a repository API URL of the
// form https://api.github.com/repos/<owner>/<name>. Returns the raw input
// if it doesn't match (defensive; the Search API always populates it).
func repoFromAPIURL(repoURL string) string {
	const marker = "/repos/"
	if i := strings.LastIndex(repoURL, marker); i != -1 {
		return repoURL[i+len(marker):]
	}
	return repoURL
}
