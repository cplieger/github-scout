// Package forge holds the forge-neutral domain types forge-scout reads: the
// rows each adapter produces, the error sentinels the collector classifies on,
// and the product a connection answered as. It declares no interfaces.
package forge

import (
	"errors"
	"strings"
	"time"
)

// Product is the forge product a connection answered as.
type Product int

// The Product members.
const (
	ProductUnknown Product = iota
	ProductGitHub
	ProductGitLab
	ProductGitea
	ProductForgejo
)

// String returns the spelling the log contract carries in its forge field.
func (p Product) String() string {
	switch p {
	case ProductGitHub:
		return "github"
	case ProductGitLab:
		return "gitlab"
	case ProductGitea:
		return "gitea"
	case ProductForgejo:
		return "forgejo"
	default:
		return "unknown"
	}
}

// PRHeads reports whether the product's owner-scoped pull-request listing
// names each row's head commit, which a checks read needs. Gitea's and
// Forgejo's reads issue-search rows, which carry none (forgeapi's documented
// PullRequests.ListMyPRs departure on both).
func (p Product) PRHeads() bool { return p != ProductGitea && p != ProductForgejo }

// GitHubDotcomAPI is GitHub.com's REST root.
const GitHubDotcomAPI = "https://api.github.com"

// githubDotcomHosts are the hosts that address GitHub.com, whose API is
// GitHubDotcomAPI whatever the scheme or path, as forgeapi resolves them.
var githubDotcomHosts = map[string]bool{"github.com": true, "www.github.com": true, "api.github.com": true}

// GitHubDotcom reports whether host, in any case, addresses GitHub.com.
func GitHubDotcom(host string) bool { return githubDotcomHosts[strings.ToLower(host)] }

// RunState is a completed run's verdict.
type RunState string

// The RunState members. A run still pending, or in a state the adapter could
// not map, has no State.
const (
	RunPassing RunState = "passing"
	RunFailing RunState = "failing"
	RunNeutral RunState = "neutral"
)

// Repo is one discovered repository. Path is the forge's display path
// (owner/name, or group/sub/project on GitLab).
type Repo struct {
	handle        any
	Path          string
	DefaultBranch string
	Archived      bool
	Fork          bool
	Private       bool
}

// WithHandle returns r carrying h, the adapter's opaque address for the
// repository. Only the adapter that set it reads it back.
func (r Repo) WithHandle(h any) Repo { r.handle = h; return r }

// Handle returns the adapter's opaque address set by WithHandle.
func (r Repo) Handle() any { return r.handle }

// TriggerPullRequest is the Run.Trigger of a run a pull request started,
// whatever branch name it reports.
const TriggerPullRequest = "pull_request"

// Run is one completed CI run.
type Run struct {
	CreatedAt time.Time
	// StartedAt is zero where the product sends no start time.
	StartedAt time.Time
	UpdatedAt time.Time
	Repo      string
	Workflow  string
	Branch    string
	// Trigger is the normalized trigger spelling: push, pull_request,
	// schedule, manual, other or unknown.
	Trigger string
	URL     string
	State   RunState
	// ID is the run's identity within its repository, stable across re-runs
	// where the product keeps one run for its attempts.
	ID     int64
	Number int64
}

// Duration is r's wall time, known only where the product sent a start time
// no later than the last update.
func (r *Run) Duration() (time.Duration, bool) {
	if r.StartedAt.IsZero() || r.UpdatedAt.Before(r.StartedAt) {
		return 0, false
	}
	return r.UpdatedAt.Sub(r.StartedAt), true
}

// PullRequest is one open pull request (merge request on GitLab).
type PullRequest struct {
	CreatedAt time.Time
	UpdatedAt time.Time
	repo      any
	Repo      string
	// Ref is the number with the product's sigil, #12 or !12.
	Ref    string
	Title  string
	Author string
	URL    string
	// HeadSHA is empty where the product's listing row names no head.
	HeadSHA string
	Labels  []string
	Number  int
	Draft   bool
	// ChecksRefused reports a row whose checks the forge refused to the
	// token, which a checks read answers CheckUnreadable without a request.
	ChecksRefused bool
}

// SetRepoHandle records the adapter's opaque address for pr's repository,
// which a checks read addresses.
func (pr *PullRequest) SetRepoHandle(h any) { pr.repo = h }

// RepoHandle returns the address set by SetRepoHandle.
func (pr *PullRequest) RepoHandle() any { return pr.repo }

// Issue is one open issue.
type Issue struct {
	CreatedAt time.Time
	UpdatedAt time.Time
	Repo      string
	Ref       string
	Title     string
	Author    string
	URL       string
	Labels    []string
	Number    int
}

// Alert is one open security alert.
type Alert struct {
	CreatedAt time.Time
	Repo      string
	// Source names the scanner family: code_scanning.
	Source   string
	Rule     string
	Severity string
	Tool     string
	URL      string
	Number   int64
}

// Workflow is one workflow definition (GitHub only).
type Workflow struct {
	Repo string
	Name string
	Path string
	// State is active, deleted, disabled_fork, disabled_inactivity or
	// disabled_manually.
	State string
	URL   string
}

// Disabled reports a workflow GitHub disabled for inactivity or a person
// disabled by hand; one disabled because its repository is a fork is not.
func (w *Workflow) Disabled() bool {
	return w.State == "disabled_inactivity" || w.State == "disabled_manually"
}

// Deleted reports a workflow whose file is gone while its runs remain.
func (w *Workflow) Deleted() bool { return w.State == "deleted" }

// ReadReserve is the request budget a scan never spends below: a read that
// would take the forge's remaining budget under it is not made, and the
// connection's scan stops until the budget renews.
const ReadReserve = 250

// Budget is the connection's last reported request budget.
type Budget struct {
	Reset time.Time
	// Remaining is -1 where the instance reports none.
	Remaining int
}

// Known reports whether the instance reported a remaining budget.
func (b Budget) Known() bool { return b.Remaining >= 0 }

// Reserved reports a known budget at or below ReadReserve.
func (b Budget) Reserved() bool { return b.Known() && b.Remaining <= ReadReserve }

// Discovery is the repositories found under each configured owner.
type Discovery struct {
	ByOwner map[string][]Repo
	// Unresolved lists the owners the instance reports unresolved. An owner
	// that resolves to no repository is a complete listing, not unresolved.
	Unresolved []string
	// End is EndCut where a listing was cut short: the repository set is not
	// complete, so nothing may be forgotten because it is absent.
	End End
}

// End is how a listing ended. The zero value is no End: a source that set
// none is read as failed, never as whole.
type End int

// The End members.
const (
	// EndWhole is a listing read to its end.
	EndWhole End = iota + 1
	// EndCut is a listing cut short: rows past the cut may be missing.
	EndCut
	// EndNone is a signal the repository does not have: no code-scanning
	// analyses.
	EndNone
)

// Listing is a source's rows and how its read ended. Only the adapters build
// one.
type Listing[T any] struct {
	Rows []T
	End  End
}

// RunList is one repository's completed runs created since a bound, in Rows,
// with the runs in a state the adapter could not map and the creation time
// of the oldest row of any state, zero when the listing held none. A run
// still pending is in neither; OldestPending is the creation time of the
// oldest, zero when there is none.
type RunList struct {
	Oldest        time.Time
	OldestPending time.Time
	Unmapped      []Run
	Listing[Run]
}

// CheckState is the folded verdict of one commit's checks.
type CheckState string

// The CheckState members. CheckNone is a whole read that found no check;
// CheckUnknown is a fold that could not give a verdict, as a check it left
// out may be failing; CheckUnreadable is checks the forge refuses this token,
// as GitHub refuses check runs to a fine-grained token, which no retry reads.
const (
	CheckPassing    CheckState = "passing"
	CheckFailing    CheckState = "failing"
	CheckPending    CheckState = "pending"
	CheckNeutral    CheckState = "neutral"
	CheckNone       CheckState = "none"
	CheckUnknown    CheckState = "unknown"
	CheckUnreadable CheckState = "unreadable"
)

// CheckResult is the folded check verdict of one commit and whether its read
// was whole.
type CheckResult struct {
	State CheckState
	End   End
}

// Sentinels the adapters wrap and the collector classifies on.
var (
	// ErrTokenInvalid is a credential the forge rejected.
	ErrTokenInvalid = errors.New("forge rejected the token")
	// ErrRateLimited is the forge refusing further requests.
	ErrRateLimited = errors.New("forge rate limit reached")
	// ErrReadDeferred is a read held back to keep the read reserve.
	ErrReadDeferred = errors.New("read deferred to keep the read reserve")
	// ErrOwnerUnresolved is an owner the instance does not resolve.
	ErrOwnerUnresolved = errors.New("owner not resolved by the instance")
	// ErrSnapshotPartial is a listing the forge answered incompletely.
	ErrSnapshotPartial = errors.New("listing answered partially")
	// ErrForbidden is a read the forge refused with 403 for a reason it did
	// not mark as a rate limit: on GitHub a missing permission or a
	// secondary limit without its headers.
	ErrForbidden = errors.New("read forbidden for this token")
	// ErrRefused is a read not sent because GitHub refused an earlier one of
	// the scan with ErrForbidden.
	ErrRefused = errors.New("request not sent: GitHub refused an earlier read of this scan")
	// ErrConnection is a connection that could not be opened or identified.
	ErrConnection = errors.New("connection failed")
	// ErrRunsUndated is a run listing whose rows carry no creation time.
	ErrRunsUndated = errors.New("run listing carries no creation time")
)

// OpenError is an open that failed after the connection named its product,
// so the failure is still reported under that product.
type OpenError struct {
	Err error
	// Budget is the budget the open's responses reported, nil when the
	// failure carries none.
	Budget  *Budget
	Product Product
}

func (e *OpenError) Error() string { return e.Err.Error() }

// Unwrap returns the failure, which keeps its sentinel.
func (e *OpenError) Unwrap() error { return e.Err }
