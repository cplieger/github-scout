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
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
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
