// Package model holds the pure data types describing the GitHub signals
// github-scout surfaces. Types here carry no behavior beyond JSON struct
// tags. The structs are never JSON-marshaled on the emit path, though: each
// signal reaches Loki as a slog.Info line whose field names are the literal
// key strings passed in internal/collect, so those keys — not these tags —
// are the Loki field-name contract. Renaming a tag here does NOT rename a
// Loki field; renaming a slog key in internal/collect does, and silently
// breaks dashboard panels and any Loki ruler alert. The tags mirror the slog
// keys for documentation and must be kept in sync with them;
// TestLogKeysMatchModelTags (internal/collect) fails the build if they drift.
//
// Two emission models are used (see internal/collect):
//
//   - Event-once: WorkflowRun. A completed run happens once; github-scout
//     emits each run ID a single time so a plain log count equals the
//     number of distinct runs. The Conclusion field (success / failure /
//     timed_out / startup_failure / cancelled / skipped / neutral) lets the
//     dashboard filter failures out of the all-runs stream and compute a
//     failure rate without a separate query per conclusion.
//   - Snapshot: PullRequest, Issue, CodeScanningAlert. These are current
//     STATE (an item stays open across scans), so github-scout emits the
//     full current set every scan. When an item is closed/merged/fixed it
//     simply stops appearing in future snapshots, and the dashboard reads
//     the most recent snapshot as "what is open right now".
package model

import (
	"errors"
	"slices"
	"time"

	"github.com/cplieger/runesafe"
)

// ErrNoCodeScanning marks a repo that has no code-scanning analyses — GitHub
// returns 404 when the feature was never enabled (no GitHub Advanced Security)
// or no CodeQL run has completed. It is a benign "no data" outcome, NOT a read
// failure: the collector counts such a repo as neither readable nor blind, so
// it never dilutes the "code scanning blind across every repo" escalation.
var ErrNoCodeScanning = errors.New("repo has no code-scanning analyses")

// ErrTokenInvalid and ErrRateLimited mark the two SYSTEMIC collection failures:
// classes that poison every call in a scan, so any signal's reported 0 this
// scan is unverified rather than confirmed empty. The GitHub client (the
// adapter) translates the transport status into these domain sentinels — 401
// to ErrTokenInvalid, 429 to ErrRateLimited — exactly as it maps 404 to
// ErrNoCodeScanning, so internal/collect classifies on meaning and never
// imports the HTTP transport's error types. A 403 is deliberately NOT mapped
// here: on code scanning it usually means one private repo lacks GitHub
// Advanced Security, so it is a per-repo failure that escalates only via the
// "blind for every repo" path, not an org-wide one.
var (
	ErrTokenInvalid = errors.New("github token rejected (401)")
	ErrRateLimited  = errors.New("github rate limit exceeded (429)")
)

// Repo is a GitHub repository discovered for an owner. Only the fields
// github-scout needs to scope follow-up API calls and to decide whether a
// repo is worth polling are retained.
type Repo struct {
	// Owner is the login of the account or org that owns the repo.
	Owner string `json:"owner"`
	// Name is the repository name (without the owner prefix).
	Name string `json:"name"`
	// Private reports whether the repo is private. github-scout polls every
	// repo it can read; the flag is retained for logging and so callers can
	// skip features (code scanning) unavailable on private repos.
	Private bool `json:"private"`
	// Archived repos are skipped: no new runs, and old signals are not
	// actionable.
	Archived bool `json:"archived"`
}

// FullName returns the canonical "owner/name" identifier.
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// WorkflowRun is a single completed GitHub Actions workflow run — the
// event-once signal. github-scout emits every completed run once,
// whatever its conclusion, so a plain log count equals the number of
// distinct runs and the Conclusion field drives both the actionable
// failures view (Conclusion in failureConclusions) and the failure-rate
// panel. Each failed run is a build / release / scheduled job that needs a
// human to look at it.
type WorkflowRun struct {
	CreatedAt time.Time `json:"created_at"`
	Repo      string    `json:"repo"`
	Workflow  string    `json:"workflow"`
	// Branch is user-authored text (fork PR branch names arrive verbatim), so
	// it is tagged runesafe.Untrusted: raw in, sanitized automatically at
	// every slog/fmt/encoder sink.
	Branch     runesafe.Untrusted `json:"branch"`
	Event      string             `json:"event"`
	Conclusion string             `json:"conclusion"`
	URL        string             `json:"url"`
	// RunID is GitHub's globally-unique run identifier and the dedup key.
	RunID     int64 `json:"run_id"`
	RunNumber int64 `json:"run_number"`
}

// PullRequest is an open pull request — a snapshot signal. github-scout
// emits the full set of currently-open PRs each scan.
type PullRequest struct {
	CreatedAt time.Time `json:"created_at"`
	Repo      string    `json:"repo"`
	// Title is untrusted upstream text (any PR author controls it); the
	// runesafe.Untrusted tag sanitizes it at every emit automatically.
	Title  runesafe.Untrusted `json:"title"`
	Author string             `json:"author"`
	URL    string             `json:"url"`
	// Number is the PR number, unique within its repo (dedup key is
	// repo+number).
	Number int64 `json:"number"`
	// Draft marks work-in-progress PRs so the dashboard can de-emphasise them.
	Draft bool `json:"draft"`
}

// Issue is an open issue — a snapshot signal. Renovate "Dependency
// Dashboard" issues and auto-generated (gremlins) trackers are excluded at
// the query level (see internal/github), so what reaches here is real,
// actionable work.
type Issue struct {
	CreatedAt time.Time `json:"created_at"`
	Repo      string    `json:"repo"`
	// Title is untrusted upstream text (any issue author controls it); the
	// runesafe.Untrusted tag sanitizes it at every emit automatically.
	Title  runesafe.Untrusted `json:"title"`
	Author string             `json:"author"`
	Labels string             `json:"labels"` // comma-joined for flat log rendering
	URL    string             `json:"url"`
	Number int64              `json:"number"`
}

// CodeScanningAlert is an open CodeQL / code-scanning alert — a snapshot
// signal, collected per-repo (the API has no cross-repo variant).
type CodeScanningAlert struct {
	CreatedAt time.Time `json:"created_at"`
	Repo      string    `json:"repo"`
	Rule      string    `json:"rule"`
	Severity  string    `json:"severity"` // security severity: critical/high/medium/low
	Tool      string    `json:"tool"`
	URL       string    `json:"url"`
	Number    int64     `json:"number"`
}

// failureConclusions is the set of run conclusions github-scout treats as
// actionable failures. It classifies a WorkflowRun's Conclusion: the
// collector counts these as failures in its scan summary, and the
// dashboard filters the all-runs stream by them for the failures view and
// uses them as the numerator of the failure-rate panel (with success the
// rest of the denominator). success/neutral/skipped/cancelled are not
// failures — a cancelled run is usually a human superseding it. Ordering
// is stable for deterministic logs and tests.
var failureConclusions = []string{"failure", "timed_out", "startup_failure"}

// IsFailureConclusion reports whether conclusion is one github-scout treats
// as an actionable failure (i.e. is in failureConclusions). It is the
// single definition of "failed" shared by the collector's summary counts
// and any caller classifying a WorkflowRun.
func IsFailureConclusion(conclusion string) bool {
	return slices.Contains(failureConclusions, conclusion)
}
