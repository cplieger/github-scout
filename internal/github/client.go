// Package github is github-scout's GitHub REST API client. It exposes
// exactly the reads the scout needs — discover an owner's repos, list a
// repo's completed workflow runs, search open PRs and issues across all of
// them, and list a repo's code-scanning alerts — over the cplieger/httpx
// retry transport. Public repos and (with a token that can see them)
// private repos are both covered, which is the whole reason this exists:
// the Grafana GitHub-datasource plugin cannot enumerate "all workflows
// across all repos", and private repos have no org-level alert endpoint.
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
	"github.com/cplieger/httpx/v2"
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
			if !strings.EqualFold(r.Owner.Login, owner) || r.Archived {
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

// ListRuns returns all completed workflow runs for repo created at or after
// since, in a single paginated query. status=completed catches every
// conclusion (success, failure, timed_out, cancelled, …) in one request,
// so the former per-conclusion fan-out is gone: the collector emits each
// run once and classifies failures via model.IsFailureConclusion, and the
// dashboard derives both the failures view and the failure rate from the
// one all-runs stream. The Actions API returns runs newest-first, so a repo
// that exceeds maxPages*perPage completed runs inside the lookback window
// has its oldest runs truncated — acceptable, since that volume is itself a
// signal worth investigating directly.
func (c *Client) ListRuns(ctx context.Context, repo model.Repo, since time.Time) ([]model.WorkflowRun, error) {
	if !urlsafe.IsSafeURLSegment(repo.Owner) || !urlsafe.IsSafeURLSegment(repo.Name) {
		return nil, fmt.Errorf("unsafe repo segment: %q", repo.FullName())
	}
	var runs []model.WorkflowRun
	for page := 1; page <= maxPages; page++ {
		q := url.Values{}
		q.Set("status", "completed")
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page))
		// GitHub accepts a date-range operator in `created`; url.Values
		// percent-encodes ">=" which the API decodes server-side.
		q.Set("created", ">="+since.UTC().Format(time.RFC3339))
		reqURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs?%s", c.baseURL, repo.Owner, repo.Name, q.Encode())

		var pageData apiRunsPage
		if err := c.getJSON(ctx, reqURL, &pageData); err != nil {
			return runs, fmt.Errorf("list runs page %d: %w", page, err)
		}
		for _, r := range pageData.WorkflowRuns {
			runs = append(runs, model.WorkflowRun{
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
	opts := make([]httpx.Option, 0, len(c.retryOpts)+3)
	opts = append(opts, c.retryOpts...)
	opts = append(opts,
		httpx.WithHeaders(func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+c.token)
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("X-GitHub-Api-Version", apiVersion)
		}),
		httpx.WithMaxBodyBytes(bodyCap),
		// Route httpx's retry diagnostics through the client's logger
		// instead of the global slog.Default(), so retry logs share the
		// app's configured (JSON) handler and are injectable in tests.
		httpx.WithLogger(c.logger),
	)
	body, err := httpx.Retry(ctx, c.http, reqURL, opts...)
	if err != nil {
		return mapStatusError(err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// mapStatusError translates the SYSTEMIC transport failures httpx.Retry can
// return into the domain sentinels the collector escalates on, so
// internal/collect classifies on meaning and never imports httpx. A 401
// (rejected token) and a 429 (rate limit) are org-wide — they poison every
// call this scan — so they become model.ErrTokenInvalid / model.ErrRateLimited
// (the original status error is preserved in the chain for logging). Every
// other failure passes through unchanged: a 403 (per-repo: Advanced Security
// off or a missing scope), a 5xx, or a decode error is a plain per-signal
// failure, and a 404 is left for ListCodeScanningAlerts to map to
// ErrNoCodeScanning. This is the same boundary mapping as codeScanningNotFound,
// kept in one place so the github client is the single authority on
// status→meaning.
func mapStatusError(err error) error {
	if se, ok := errors.AsType[*httpx.StatusError](err); ok {
		switch se.Code {
		case http.StatusUnauthorized:
			return fmt.Errorf("%w: %w", model.ErrTokenInvalid, err)
		case http.StatusTooManyRequests:
			return fmt.Errorf("%w: %w", model.ErrRateLimited, err)
		}
	}
	return err
}

// --- Pull requests & issues (cross-repo via the Search API) ---

// apiSearchResp is the /search/issues envelope.
type apiSearchResp struct {
	Items             []apiSearchItem `json:"items"`
	IncompleteResults bool            `json:"incomplete_results"`
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
	// archived:false excludes archived repos from the cross-repo Search API,
	// which (unlike ListRepos) includes them by default. This aligns the
	// snapshot path with the repo-loop path (ListRepos filters r.Archived) and
	// with model.Repo's contract that archived repos are skipped: an archived
	// repo's open PRs/issues are not actionable.
	q := base + " user:" + owner + " archived:false"
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
			return nil, fmt.Errorf("search %q page %d: %w", base, page, err)
		}
		items = append(items, resp.Items...)
		if resp.IncompleteResults {
			return nil, fmt.Errorf("search %q page %d: GitHub returned incomplete results"+
				" (search timed out)", base, page)
		}
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

// ListCodeScanningAlerts returns open code-scanning alerts for repo. A repo
// that never ran code scanning returns 404 (no analyses); that is a benign
// "no data" outcome rather than a read failure, so it is surfaced as
// model.ErrNoCodeScanning (the collector treats such a repo as neither
// readable nor blind). A 403 is surfaced as a generic error: it can mean
// GitHub Advanced Security is disabled (expected on a private repo without a
// GHAS license) OR a missing token scope OR a rate-limit, and silently
// reporting zero alerts would hide a security signal. The collector treats a
// per-repo 403 as a degraded read and escalates only when code scanning is
// blind for EVERY repo that has it; silence an expected, persistent 403 (a
// private repo without GHAS) via EXCLUDE_REPOS.
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

		var pageAlerts []apiCodeAlert
		if err := c.getJSON(ctx, reqURL, &pageAlerts); err != nil {
			if codeScanningNotFound(err) && len(alerts) == 0 {
				// No analyses for this repo (404): not a read failure but a
				// benign "no data" outcome. Surface model.ErrNoCodeScanning so
				// the collector excludes this repo from the code-scanning
				// "blind" calculation instead of counting it as a clean read.
				// A 404 after earlier pages already returned alerts is a real
				// read failure, not no-data — surface it via the wrapped error.
				return nil, model.ErrNoCodeScanning
			}
			return nil, fmt.Errorf("list code scanning alerts page %d: %w", page, err)
		}
		for i := range pageAlerts {
			a := &pageAlerts[i]
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
		if len(pageAlerts) < perPage {
			break
		}
	}
	return alerts, nil
}

// codeScanningNotFound reports whether err is a 404 — the status GitHub
// returns when a repository has no code-scanning analyses (the feature was
// never configured, or no CodeQL run has completed). That is genuinely "no
// alerts", so the collector skips it silently. A 403 is deliberately NOT
// treated here: it conflates GitHub Advanced Security being disabled
// (common on private repos) with a missing token scope or a rate-limit, and
// silently reporting zero alerts on the latter two would be a false-negative
// on a security signal — so a 403 is surfaced as an error instead.
func codeScanningNotFound(err error) bool {
	if se, ok := errors.AsType[*httpx.StatusError](err); ok {
		return se.Code == http.StatusNotFound
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
