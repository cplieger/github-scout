// Package github implements the GitHub REST client.
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

	"github.com/cplieger/github-scout/internal/ghsignal"
	"github.com/cplieger/github-scout/internal/urlsafe"
	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/runesafe/v2"
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
	cache     *condCache
	token     string
	baseURL   string
	retryOpts []httpx.Option
}

// Options configures NewClient. HTTP and Token are required; the other
// three fields have meaningful zero values, which is why this is a struct
// rather than a parameter list — three optional trailing parameters made
// every call site restate defaults positionally, and the two same-typed
// strings (the required token, the optional cache path) left a token/path
// transposition compiling.
// Field order is govet fieldalignment's, not editorial.
type Options struct {
	// HTTP performs every request. Required.
	HTTP *http.Client
	// Logger receives the client's warnings; nil falls back to slog.Default.
	Logger *slog.Logger
	// Token authenticates every call.
	Token string
	// CondCachePath, when non-empty, persists the conditional-request cache
	// (per-URL validators + the item subset they validate) across processes;
	// empty keeps it in-memory only (tests).
	CondCachePath string
	// RetryOpts apply to each request; nil means the httpx defaults.
	RetryOpts []httpx.Option
}

// NewClient returns a Client for the GitHub REST API, configured by opts.
func NewClient(opts Options) *Client {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		http: opts.HTTP, token: opts.Token, baseURL: apiBase,
		retryOpts: opts.RetryOpts, logger: logger,
		cache: newCondCache(opts.CondCachePath, logger),
	}
}

// apiRepo is the subset of the /user/repos response github-scout reads.
type apiRepo struct {
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
	Private  bool `json:"private"`
	Archived bool `json:"archived"`
	Fork     bool `json:"fork"`
}

// ListRepos returns every non-archived repo owned by the authenticated
// token whose owner login equals owner. It uses /user/repos (not
// /users/{owner}/repos) so private repos are included — failed Actions
// runs in a private repo are just as actionable as public ones, and the
// public endpoint omits them. Results are filtered to owner so a token
// with org memberships doesn't pull in repos the operator didn't ask for.
func (c *Client) ListRepos(ctx context.Context, owner string) ([]ghsignal.Repo, error) {
	if !urlsafe.IsSafeURLSegment(owner) {
		return nil, fmt.Errorf("unsafe owner segment: %q", owner)
	}
	var repos []ghsignal.Repo
	for page := 1; page <= maxPages; page++ {
		q := url.Values{}
		q.Set("affiliation", "owner")
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page))
		q.Set("sort", "full_name")
		reqURL := c.baseURL + "/user/repos?" + q.Encode()

		var pageRepos []apiRepo
		if err := c.getJSONConditional(ctx, reqURL, &pageRepos); err != nil {
			return nil, fmt.Errorf("list repos page %d: %w", page, err)
		}
		for _, r := range pageRepos {
			if !strings.EqualFold(r.Owner.Login, owner) || r.Archived {
				continue
			}
			repos = append(repos, ghsignal.Repo{
				Owner:    r.Owner.Login,
				Name:     r.Name,
				Private:  r.Private,
				Archived: r.Archived,
				Fork:     r.Fork,
			})
		}
		if len(pageRepos) < perPage {
			return repos, nil // last page
		}
	}
	// maxPages exhausted with a full final page: more repos may exist beyond
	// the pagination bound. The bound itself is deliberate (see maxPages),
	// but an owner with >500 visible repos must not be silently under-scanned,
	// so surface the possible truncation instead of returning quietly.
	c.logger.Warn("repo listing hit pagination bound; scan universe may be truncated",
		"owner", owner, "page_cap", maxPages, "repo_cap", maxPages*perPage)
	return repos, nil
}

// apiRunsPage is the /actions/runs response envelope.
type apiRunsPage struct {
	WorkflowRuns []apiRun `json:"workflow_runs"`
}

// apiRun is the subset of a workflow run github-scout surfaces.
type apiRun struct {
	CreatedAt time.Time `json:"created_at"`
	Name      string    `json:"name"`
	// HeadBranch is tagged at the decode boundary: fork PR branch names are
	// user-authored text, so the provenance travels with the value.
	HeadBranch runesafe.Untrusted `json:"head_branch"`
	Event      string             `json:"event"`
	Conclusion string             `json:"conclusion"`
	HTMLURL    string             `json:"html_url"`
	ID         int64              `json:"id"`
	RunNumber  int64              `json:"run_number"`
}

// ListRuns returns completed runs since the cutoff; pagination bounds a scan.
func (c *Client) ListRuns(ctx context.Context, repo ghsignal.Repo, since time.Time) ([]ghsignal.WorkflowRun, error) {
	if !urlsafe.IsSafeURLSegment(repo.Owner) || !urlsafe.IsSafeURLSegment(repo.Name) {
		return nil, fmt.Errorf("unsafe repo segment: %q", repo.FullName())
	}
	var runs []ghsignal.WorkflowRun
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
			runs = append(runs, ghsignal.WorkflowRun{
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

// setHeaders applies the auth + version headers every GitHub request
// carries, shared by the unconditional (getJSON) and conditional
// (getJSONConditional) paths.
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
}

// getJSON fetches reqURL with auth + version headers via the httpx retry
// transport and decodes the body into out. The body is capped so a
// runaway response can't exhaust memory.
func (c *Client) getJSON(ctx context.Context, reqURL string, out any) error {
	opts := make([]httpx.GetOption, 0, len(c.retryOpts)+3)
	for _, o := range c.retryOpts {
		opts = append(opts, o)
	}
	opts = append(opts,
		httpx.WithHeaders(c.setHeaders),
		httpx.WithMaxBodyBytes(bodyCap),
		// Route httpx's retry diagnostics through the client's logger
		// instead of the global slog.Default(), so retry logs share the
		// app's configured (JSON) handler and are injectable in tests.
		httpx.WithLogger(c.logger),
	)
	body, err := httpx.GetBytes(ctx, c.http, reqURL, opts...)
	if err != nil {
		return mapStatusError(err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// getJSONConditional serves a cached representation on 304 and retries one cacheless 304 unconditionally.
func (c *Client) getJSONConditional(ctx context.Context, reqURL string, out any) error {
	res, err := c.conditionalGet(ctx, reqURL, c.cache.validators(reqURL))
	if err != nil {
		return mapStatusError(err)
	}
	if res.NotModified {
		if c.cache.decodeInto(reqURL, out) {
			return nil
		}
		// A 304 without a usable cached representation: the persisted entry
		// was corrupt (decodeInto dropped it), or an out-of-contract upstream
		// answered an unconditional request with a 304. Refetch once without
		// validators; a second 304 is a hard upstream fault.
		res, err = c.conditionalGet(ctx, reqURL, httpx.Validators{})
		if err != nil {
			return mapStatusError(err)
		}
		if res.NotModified {
			return errors.New("upstream returned 304 to an unconditional request")
		}
	}
	if err := json.Unmarshal(res.Body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	c.cache.store(reqURL, res.Validators, out)
	return nil
}

// conditionalGet runs one conditional GET under the same retry loop as the
// unconditional path (httpx.Do; DoConditional is single-attempt by
// contract). A non-transient 5xx is wrapped transient so this door retries
// every 5xx exactly as GetBytes does — DoConditional's CheckHTTPStatus
// mapping classifies only 502/503/504 transient, and the repo listing is
// the scan's one health-flipping call, so it must not lose retries in the
// adoption.
func (c *Client) conditionalGet(ctx context.Context, reqURL string, v httpx.Validators) (httpx.ConditionalResult, error) {
	opts := make([]httpx.DoOption, 0, len(c.retryOpts)+2)
	for _, o := range c.retryOpts {
		opts = append(opts, o)
	}
	opts = append(opts, httpx.WithLabel("github"), httpx.WithLogger(c.logger))
	return httpx.Do(ctx, func(ctx context.Context) (httpx.ConditionalResult, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
		if err != nil {
			return httpx.ConditionalResult{}, err
		}
		c.setHeaders(req)
		res, err := httpx.DoConditional(c.http, req, v, bodyCap)
		if hse, ok := errors.AsType[*httpx.HTTPStatusError](err); ok && hse.IsServerError() && !hse.IsTransient() {
			err = transientStatusError{err}
		}
		return res, err
	}, opts...)
}

// transientStatusError marks a non-transient 5xx from the conditional door
// retryable, aligning it with the GetBytes door's all-5xx retry policy (the
// per-door divergence is deliberate in httpx; this client wants one policy
// across both of its paths).
type transientStatusError struct{ error }

// IsTransient implements httpx.Transient.
func (transientStatusError) IsTransient() bool { return true }

// Unwrap exposes the wrapped status error to errors.As chains
// (codeScanningNotFound, mapStatusError).
func (e transientStatusError) Unwrap() error { return e.error }

// mapStatusError maps only 401 and 429 to systemic domain errors; 403 remains per-repo.
func mapStatusError(err error) error {
	if se, ok := errors.AsType[*httpx.StatusError](err); ok {
		switch se.Code {
		case http.StatusUnauthorized:
			return fmt.Errorf("%w: %w", ghsignal.ErrTokenInvalid, err)
		case http.StatusTooManyRequests:
			return fmt.Errorf("%w: %w", ghsignal.ErrRateLimited, err)
		}
		return err
	}
	if _, ok := errors.AsType[*httpx.RateLimitError](err); ok {
		return fmt.Errorf("%w: %w", ghsignal.ErrRateLimited, err)
	}
	if ae, ok := errors.AsType[*httpx.AuthError](err); ok && strings.Contains(ae.Msg, "(401)") {
		return fmt.Errorf("%w: %w", ghsignal.ErrTokenInvalid, err)
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
	CreatedAt time.Time `json:"created_at"`
	// Title is tagged at the decode boundary: PR/issue titles are authored
	// by anyone, so the provenance travels with the value.
	Title         runesafe.Untrusted `json:"title"`
	HTMLURL       string             `json:"html_url"`
	RepositoryURL string             `json:"repository_url"`
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
func (c *Client) SearchOpenPRs(ctx context.Context, owner, exclude string) ([]ghsignal.PullRequest, error) {
	items, err := c.search(ctx, "is:open is:pr", owner, exclude)
	if err != nil {
		return nil, err
	}
	prs := make([]ghsignal.PullRequest, 0, len(items))
	for i := range items {
		it := &items[i]
		prs = append(prs, ghsignal.PullRequest{
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
func (c *Client) SearchOpenIssues(ctx context.Context, owner, exclude string) ([]ghsignal.Issue, error) {
	items, err := c.search(ctx, "is:open is:issue", owner, exclude)
	if err != nil {
		return nil, err
	}
	issues := make([]ghsignal.Issue, 0, len(items))
	for i := range items {
		it := &items[i]
		labels := make([]string, 0, len(it.Labels))
		for _, l := range it.Labels {
			labels = append(labels, l.Name)
		}
		issues = append(issues, ghsignal.Issue{
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
	// with ghsignal.Repo's contract that archived repos are skipped: an archived
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
			return items, nil // last page
		}
	}
	// maxPages exhausted with a full final page: the open set may extend past
	// the pagination bound, so this snapshot could be partial. Surface it
	// rather than presenting a truncated snapshot as complete.
	c.logger.Warn("search hit pagination bound; snapshot may be truncated",
		"query", base, "page_cap", maxPages, "item_cap", maxPages*perPage)
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

// ListCodeScanningAlerts maps a first-page 404 to no data; a 403 remains unreadable security data.
func (c *Client) ListCodeScanningAlerts(ctx context.Context, repo ghsignal.Repo) ([]ghsignal.CodeScanningAlert, error) {
	if !urlsafe.IsSafeURLSegment(repo.Owner) || !urlsafe.IsSafeURLSegment(repo.Name) {
		return nil, fmt.Errorf("unsafe repo segment: %q", repo.FullName())
	}
	var alerts []ghsignal.CodeScanningAlert
	for page := 1; page <= maxPages; page++ {
		v := url.Values{}
		v.Set("state", "open")
		v.Set("per_page", strconv.Itoa(perPage))
		v.Set("page", strconv.Itoa(page))
		reqURL := fmt.Sprintf("%s/repos/%s/%s/code-scanning/alerts?%s", c.baseURL, repo.Owner, repo.Name, v.Encode())

		var pageAlerts []apiCodeAlert
		if err := c.getJSONConditional(ctx, reqURL, &pageAlerts); err != nil {
			if codeScanningNotFound(err) && len(alerts) == 0 {
				// No analyses for this repo (404): not a read failure but a
				// benign "no data" outcome. Surface ghsignal.ErrNoCodeScanning so
				// the collector excludes this repo from the code-scanning
				// "blind" calculation instead of counting it as a clean read.
				// A 404 after earlier pages already returned alerts is a real
				// read failure, not no-data — surface it via the wrapped error.
				return nil, ghsignal.ErrNoCodeScanning
			}
			return nil, fmt.Errorf("list code scanning alerts page %d: %w", page, err)
		}
		for i := range pageAlerts {
			a := &pageAlerts[i]
			alerts = append(alerts, ghsignal.CodeScanningAlert{
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

// codeScanningNotFound accepts 404 from both HTTP client paths; a 403 remains unreadable security data.
func codeScanningNotFound(err error) bool {
	if se, ok := errors.AsType[*httpx.StatusError](err); ok {
		return se.Code == http.StatusNotFound
	}
	if hse, ok := errors.AsType[*httpx.HTTPStatusError](err); ok {
		return hse.Code == http.StatusNotFound
	}
	return false
}

// repoFromAPIURL extracts "owner/name" from a repository API URL of the
// form https://api.github.com/repos/<owner>/<name>. Returns the raw input
// if it doesn't match (defensive; the Search API always populates it).
func repoFromAPIURL(repoURL string) string {
	if _, fullName, found := strings.CutLast(repoURL, "/repos/"); found {
		return fullName
	}
	return repoURL
}
