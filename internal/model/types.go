// Package model holds the pure data types describing the GitHub signals
// github-scout surfaces. Types here carry no behavior beyond JSON struct
// tags. The tags define the structured-log field names that Loki indexes
// and the Grafana dashboard queries, so they are a contract: renaming a
// tag renames a Loki field and silently breaks dashboard panels and any
// Loki ruler alert. Treat tag changes as a behavior change, not a refactor.
//
// Two emission models are used (see internal/collect):
//
//   - Event-once: FailedRun. A failure happens once; github-scout emits
//     each run ID a single time so a plain log count equals the number of
//     distinct failures.
//   - Snapshot: PullRequest, Issue, CodeScanningAlert. These are current
//     STATE (an item stays open across scans), so github-scout emits the
//     full current set every scan. When an item is closed/merged/fixed it
//     simply stops appearing in future snapshots, and the dashboard reads
//     the most recent snapshot as "what is open right now".
package model

import "time"

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

// FailedRun is a single failed (or timed-out / startup-failed) GitHub
// Actions workflow run — the event-once signal. Each is a build / release /
// scheduled job that needs a human to look at it.
type FailedRun struct {
	CreatedAt  time.Time `json:"created_at"`
	Repo       string    `json:"repo"`
	Workflow   string    `json:"workflow"`
	Branch     string    `json:"branch"`
	Event      string    `json:"event"`
	Conclusion string    `json:"conclusion"`
	URL        string    `json:"url"`
	// RunID is GitHub's globally-unique run identifier and the dedup key.
	RunID     int64 `json:"run_id"`
	RunNumber int64 `json:"run_number"`
}

// PullRequest is an open pull request — a snapshot signal. github-scout
// emits the full set of currently-open PRs each scan.
type PullRequest struct {
	CreatedAt time.Time `json:"created_at"`
	Repo      string    `json:"repo"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	URL       string    `json:"url"`
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
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Labels    string    `json:"labels"` // comma-joined for flat log rendering
	URL       string    `json:"url"`
	Number    int64     `json:"number"`
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

// FailureConclusions is the set of run conclusions github-scout treats as
// actionable failures. success/neutral/skipped/cancelled are excluded: a
// cancelled run is usually a human superseding it. The Actions API accepts
// each as a `status` query value, so the collector issues one query per
// conclusion. Ordering is stable for deterministic logs and tests.
var FailureConclusions = []string{"failure", "timed_out", "startup_failure"}
