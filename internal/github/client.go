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
	"github.com/cplieger/httpx/v3"
	"github.com/cplieger/runesafe"
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

// NewClient returns a Client using the provided *http.Client for all
// requests, authenticating with token, and applying retryOpts to each
// call. A nil logger falls back to slog.Default. condCachePath, when
// non-empty, persists the conditional-request cache (per-URL validators +
// the item subset they validate) across processes; empty keeps it
// in-memory only (tests).
func NewClient(client *http.Client, token string, retryOpts []httpx.Option, logger *slog.Logger, condCachePath string) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		http: client, token: token, baseURL: apiBase,
		retryOpts: retryOpts, logger: logger,
		cache: newCondCache(condCachePath, logger),
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
		if err := c.getJSONConditional(ctx, reqURL, &pageRepos); err != nil {
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

// getJSONConditional is getJSON over a conditional GET (httpx.DoConditional)
// backed by the client's per-URL cache: an unchanged resource revalidates as
// a 304 — which GitHub serves without charging the primary rate limit — and
// out is filled from the cached item subset; a 200 fills out from the body
// and refreshes the cache. Used by the endpoints whose URLs are stable
// across scans (the repo listing and per-repo code-scanning alerts) so
// their ETags actually match; the runs query stays on getJSON — its
// `created>=` window moves every scan, so its URL (and thus validator) can
// never stabilize.
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

// mapStatusError translates the SYSTEMIC transport failures the two request
// paths can return into the domain sentinels the collector escalates on, so
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
//
// Two wire families arrive here: GetBytes returns *httpx.StatusError (with
// the code), while DoConditional classifies through httpx.CheckHTTPStatus —
// *httpx.RateLimitError for a 429, and *httpx.AuthError for BOTH 401 and
// 403. The AuthError message embeds the status ("invalid API key (401)" /
// "access denied (403)"), and only the 401 may escalate to ErrTokenInvalid:
// a 403 is per-repo (GHAS off) and escalating it would page on every
// private repo without Advanced Security. An AuthError whose message
// matches neither stays generic — failing toward degraded-not-escalated,
// the app's safe direction.
func mapStatusError(err error) error {
	if se, ok := errors.AsType[*httpx.StatusError](err); ok {
		switch se.Code {
		case http.StatusUnauthorized:
			return fmt.Errorf("%w: %w", model.ErrTokenInvalid, err)
		case http.StatusTooManyRequests:
			return fmt.Errorf("%w: %w", model.ErrRateLimited, err)
		}
		return err
	}
	if _, ok := errors.AsType[*httpx.RateLimitError](err); ok {
		return fmt.Errorf("%w: %w", model.ErrRateLimited, err)
	}
	if ae, ok := errors.AsType[*httpx.AuthError](err); ok && strings.Contains(ae.Msg, "(401)") {
		return fmt.Errorf("%w: %w", model.ErrTokenInvalid, err)
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
// private repo without GHAS) via CODE_SCANNING_EXCLUDE_REPOS, which skips
// only that repo's code-scanning read (EXCLUDE_REPOS would drop the repo
// from every signal).
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
		if err := c.getJSONConditional(ctx, reqURL, &pageAlerts); err != nil {
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
// on a security signal — so a 403 is surfaced as an error instead. Both
// wire families are covered: GetBytes's *StatusError and the conditional
// door's *HTTPStatusError (CheckHTTPStatus leaves a 404 in the latter).
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
	const marker = "/repos/"
	if i := strings.LastIndex(repoURL, marker); i != -1 {
		return repoURL[i+len(marker):]
	}
	return repoURL
}
