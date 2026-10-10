// Package githubrest reads the two GitHub REST routes forgeapi does not model:
// a repository's open code-scanning alerts and its workflow definitions. Every
// status is classified on its code, never on message text, through one table
// of what each route's answers mean.
package githubrest

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/github-scout/internal/forge"
	"github.com/cplieger/github-scout/internal/ghquota"
	"github.com/cplieger/github-scout/internal/urlsafe"
	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/jsoncap/v2"
	"github.com/cplieger/ssrf/v4"
)

const (
	perPage = 100
	// maxStateEcho bounds the bytes of an unknown workflow state an error quotes.
	maxStateEcho = 32
	// bodyCap rejects a runaway response; the decoders bound a page's rows at
	// perPage before decoding them.
	bodyCap = 8 << 20
)

// errNotFound is a 404, which each route reads in its own way.
var errNotFound = errors.New("not found")

// Client reads the GitHub REST API at one base URL with one token. It is not
// safe for concurrent use.
type Client struct {
	http    *http.Client
	meter   *ghquota.Meter
	base    string
	token   string
	version string
	retry   []httpx.DoOption
}

// New returns a client for the API at base (see APIBase). version is sent as
// X-GitHub-Api-Version, the one forgeapi pins, so both clients read one
// version's shapes. retry overrides the default retry policy. hc is copied,
// never changed: every request it sends, each redirect hop included, is
// admitted by meter, the connection's quota, which sees its response.
func New(hc *http.Client, base, token, version string, meter *ghquota.Meter, logger *slog.Logger, retry ...httpx.Option) *Client {
	opts := make([]httpx.DoOption, 0, len(retry)+2)
	for _, o := range retry {
		opts = append(opts, o)
	}
	opts = append(opts, httpx.WithLabel("github rest"), httpx.WithLogger(logger))
	inner := hc.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	metered := *hc
	metered.Transport = admitting{next: meter.Observe(inner), meter: meter}
	return &Client{http: &metered, base: strings.TrimRight(base, "/"), token: token, version: version, retry: opts, meter: meter}
}

// BeginScan starts a scan on the connection's quota (see ghquota.Meter).
func (c *Client) BeginScan() { c.meter.BeginScan() }

// Budget is the connection's shared REST budget (see ghquota.Meter.Budget).
func (c *Client) Budget() forge.Budget { return c.meter.Budget() }

// admitting sends a request only when the meter admits it.
type admitting struct {
	next  http.RoundTripper
	meter *ghquota.Meter
}

func (a admitting) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := a.meter.Admit(); err != nil {
		return nil, err
	}
	return a.next.RoundTrip(req)
}

// HTTPClient is the client a connection's reads go through: the address a
// dial reaches must be public unless private is set, as forgeapi holds its
// own dials, and the port must be 443, 80 when plaintext is allowed, or
// base's own. A refused dial is logged to logger.
func HTTPClient(base string, private, plaintext bool, logger *slog.Logger) *http.Client {
	return httpClient(base, private, plaintext, ssrf.WithLogger(logger))
}

func httpClient(base string, private, plaintext bool, opts ...ssrf.TransportOption) *http.Client {
	ports := []uint16{443}
	if plaintext {
		ports = append(ports, 80)
	}
	if u, err := url.Parse(base); err == nil {
		if n, err := strconv.ParseUint(u.Port(), 10, 16); err == nil {
			ports = append(ports, uint16(n))
		}
	}
	opts = append([]ssrf.TransportOption{ssrf.WithAllowedPorts(ports...)}, opts...)
	if private {
		opts = append(opts, ssrf.WithAddressPolicy(func(netip.Addr) bool { return true }))
	}
	hc := httpx.NewClient(30 * time.Second)
	hc.Transport = ssrf.SafeTransport(opts...)
	return hc
}

// APIBase is the REST root for a connection: apiURL when set, else
// forge.GitHubDotcomAPI for a GitHub.com host (forge.GitHubDotcom), else the
// GitHub Enterprise Server layout under webURL.
func APIBase(webURL, apiURL string) string {
	if apiURL != "" {
		return strings.TrimRight(apiURL, "/")
	}
	if u, err := url.Parse(webURL); err == nil && forge.GitHubDotcom(u.Hostname()) {
		return forge.GitHubDotcomAPI
	}
	return strings.TrimRight(webURL, "/") + "/api/v3"
}

type apiAlert struct {
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

// maxAlertPages bounds one repository's code-scanning read at 10,000 open
// alerts, a safety ceiling rather than a working limit: the read returns
// every row it holds, about 300 bytes each, and each page costs one request
// of the connection's quota (see ghquota.Meter.Admit).
const maxAlertPages = 100

// route is one listing this package reads and what its answers mean.
type route struct {
	what string
	// firstPageNotFound is the End a 404 on the first page reads as; zero
	// makes every 404 a failed read.
	firstPageNotFound forge.End
	// maxPages bounds one repository's read; a full last page there ends it
	// EndCut.
	maxPages int
}

var (
	// codeScanning answers 404 on its first page for a repository with no
	// analyses
	// (https://docs.github.com/en/rest/code-scanning/code-scanning#list-code-scanning-alerts-for-a-repository).
	codeScanning = route{what: "code-scanning alerts", maxPages: maxAlertPages, firstPageNotFound: forge.EndNone}
	// workflows documents no 404.
	workflows = route{what: "workflows", maxPages: 3}
)

// list reads every page of rt for repo, decoding each with decode. Any
// answer its table does not name fails the read: a 403 is forge.ErrRateLimited
// when its headers report a rate limit, else forge.ErrForbidden (see
// statusError), never an empty listing.
func list[T any](ctx context.Context, c *Client, rt route, repo string, q url.Values, path string,
	decode func(repo string, body []byte) ([]T, error),
) (forge.Listing[T], error) {
	var rows []T
	for page := 1; page <= rt.maxPages; page++ {
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page))
		body, err := c.get(ctx, path, q)
		if errors.Is(err, errNotFound) && page == 1 && rt.firstPageNotFound != 0 {
			return forge.Listing[T]{End: rt.firstPageNotFound}, nil
		}
		var got []T
		if err == nil {
			got, err = decode(repo, body)
		}
		if err != nil {
			return forge.Listing[T]{}, fmt.Errorf("%s of %s page %d: %w", rt.what, repo, page, err)
		}
		rows = append(rows, got...)
		if len(got) < perPage {
			return forge.Listing[T]{Rows: rows, End: forge.EndWhole}, nil
		}
	}
	return forge.Listing[T]{Rows: rows, End: forge.EndCut}, nil
}

// CodeScanningAlerts reads repo's open code-scanning alerts: EndNone where
// the repository has no analyses (see codeScanning).
func (c *Client) CodeScanningAlerts(ctx context.Context, repo forge.Repo) (forge.Listing[forge.Alert], error) {
	owner, name, err := segments(repo.Path)
	if err != nil {
		return forge.Listing[forge.Alert]{}, err
	}
	return list(ctx, c, codeScanning, repo.Path, url.Values{"state": {"open"}}, "/repos/"+owner+"/"+name+"/code-scanning/alerts", decodeAlerts)
}

// decodeAlerts maps one code-scanning alerts page of repo. A body that is not
// a JSON array, null included, or that holds more rows than a page asks for,
// is a decode failure, never an empty or a cut page.
func decodeAlerts(repo string, body []byte) ([]forge.Alert, error) {
	d := jsoncap.NewDecoder(bytes.NewReader(body), perPage)
	rows, err := d.Array(nil, perPage, "alerts page", func(a *apiAlert) error { return d.Decode(a) })
	if err == nil {
		err = d.End()
	}
	if err != nil {
		return nil, fmt.Errorf("decode alerts page: %w", err)
	}
	if rows == nil {
		return nil, errors.New("decode alerts page: not an array")
	}
	alerts := make([]forge.Alert, 0, len(rows))
	for i := range rows {
		a := &rows[i]
		alerts = append(alerts, forge.Alert{
			CreatedAt: a.CreatedAt, Repo: repo, Source: "code_scanning", Rule: cmp.Or(a.Rule.ID, a.Rule.Description),
			Severity: a.Rule.SecuritySeverityLevel, Tool: a.Tool.Name, URL: a.HTMLURL, Number: a.Number,
		})
	}
	return alerts, nil
}

type apiWorkflow struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
}

// Workflows reads repo's workflow definitions in every state; a state
// outside the five GitHub documents fails the read.
func (c *Client) Workflows(ctx context.Context, repo forge.Repo) (forge.Listing[forge.Workflow], error) {
	owner, name, err := segments(repo.Path)
	if err != nil {
		return forge.Listing[forge.Workflow]{}, err
	}
	return list(ctx, c, workflows, repo.Path, url.Values{}, "/repos/"+owner+"/"+name+"/actions/workflows", decodeWorkflows)
}

// decodeWorkflows maps one workflows page of repo. A page without a
// workflows array, null included, holding more rows than a page asks for,
// or naming a state outside the five GitHub documents
// (https://docs.github.com/en/rest/actions/workflows#list-repository-workflows),
// is a decode failure.
func decodeWorkflows(repo string, body []byte) ([]forge.Workflow, error) {
	d := jsoncap.NewDecoder(bytes.NewReader(body), perPage)
	var page []apiWorkflow
	err := d.Object(func(key string) error {
		if !strings.EqualFold(key, "workflows") {
			return d.Skip()
		}
		var aerr error
		page, aerr = d.Array(page, perPage, "workflows page", func(w *apiWorkflow) error { return d.Decode(w) })
		return aerr
	})
	if err == nil {
		err = d.End()
	}
	if err != nil {
		return nil, fmt.Errorf("decode workflows page: %w", err)
	}
	if page == nil {
		return nil, errors.New("decode workflows page: no workflows array")
	}
	defs := make([]forge.Workflow, 0, len(page))
	for i := range page {
		w := &page[i]
		switch w.State {
		case "active", "deleted", "disabled_fork", "disabled_inactivity", "disabled_manually":
			defs = append(defs, forge.Workflow{Repo: repo, Name: w.Name, Path: w.Path, State: w.State, URL: w.HTMLURL})
		default:
			return nil, fmt.Errorf("decode workflows page: unknown workflow state %q", w.State[:min(len(w.State), maxStateEcho)])
		}
	}
	return defs, nil
}

// segments splits a GitHub display path into its owner and name, each safe
// to place in a URL path.
func segments(path string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(path, "/")
	if !ok || !urlsafe.IsSafeURLSegment(owner) || !urlsafe.IsSafeURLSegment(name) {
		return "", "", fmt.Errorf("repository path %q is not a safe owner/name pair", path)
	}
	return owner, name, nil
}

// get reads one page's body under the client's retry policy, which repeats a
// 5xx and the transport errors httpx.IsTransient accepts.
func (c *Client) get(ctx context.Context, route string, q url.Values) ([]byte, error) {
	return httpx.Do(ctx, func(ctx context.Context) ([]byte, error) {
		return c.attempt(ctx, c.base+route+"?"+q.Encode())
	}, c.retry...)
}

func (c *Client) attempt(ctx context.Context, reqURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", c.version)
	resp, err := c.http.Do(req)
	for _, held := range []error{forge.ErrReadDeferred, forge.ErrRefused} {
		if errors.Is(err, held) {
			return nil, held
		}
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := statusError(resp); err != nil {
		httpx.Drain(resp.Body)
		return nil, err
	}
	return httpx.ReadLimitedBody(resp.Body, bodyCap)
}

// statusError classifies a non-2xx answer on its status code and headers,
// never its body. A 5xx is marked transient so the retry loop repeats it. A
// 403 with X-RateLimit-Remaining at 0 or a Retry-After header is a rate
// limit. Any other 403 is forge.ErrForbidden, which GitHub also answers for a
// secondary limit that sets neither header, so the quota meter sends no more
// requests after it (see ghquota.Meter.Admit).
func statusError(resp *http.Response) error {
	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		return nil
	case code == http.StatusUnauthorized:
		return fmt.Errorf("%w (status %d)", forge.ErrTokenInvalid, code)
	case code == http.StatusForbidden && ghquota.Throttled(resp.Header):
		return fmt.Errorf("%w (status %d, rate limit)", forge.ErrRateLimited, code)
	case code == http.StatusForbidden:
		return fmt.Errorf("%w (status %d)", forge.ErrForbidden, code)
	case code == http.StatusNotFound:
		return errNotFound
	case code == http.StatusTooManyRequests:
		return fmt.Errorf("%w: %w", forge.ErrRateLimited, httpx.CheckHTTPStatus(resp))
	case code >= 500:
		return httpx.MarkTransient(httpx.CheckHTTPStatus(resp))
	default:
		return httpx.CheckHTTPStatus(resp)
	}
}
