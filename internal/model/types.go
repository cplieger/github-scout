// Package model holds the pure data types describing the GitHub signals
// github-scout surfaces. Types here carry no behavior beyond JSON struct
// tags. The tags define the structured-log field names that Loki indexes
// and the Grafana dashboard queries, so they are a contract: changing a
// tag renames a Loki field and silently breaks dashboard panels and the
// Loki ruler alert. Treat tag changes as a behavior change, not a refactor.
package model

import "time"

// Repo is a GitHub repository discovered for an owner. Only the fields
// github-scout needs to scope follow-up API calls and to decide whether
// a repo is worth polling are retained.
type Repo struct {
	// Owner is the login of the account or org that owns the repo.
	Owner string `json:"owner"`
	// Name is the repository name (without the owner prefix).
	Name string `json:"name"`
	// Private reports whether the repo is private. github-scout polls
	// every repo it can read; the flag is retained only for logging and
	// for callers that want to scope a signal type to public repos.
	Private bool `json:"private"`
	// Archived repos are skipped: they produce no new workflow runs and
	// their old failures are not actionable.
	Archived bool `json:"archived"`
}

// FullName returns the canonical "owner/name" identifier.
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// FailedRun is a single failed (or timed-out / startup-failed) GitHub
// Actions workflow run. It is the primary actionable signal github-scout
// emits: each one is a build/release/scheduled job that needs a human to
// look at it. Every field maps to a structured-log key consumed by the
// Grafana "Failed Actions" table and is chosen to be directly actionable
// (the URL is clickable, the repo/workflow/branch locate the failure).
type FailedRun struct {
	// CreatedAt is when the run was created on GitHub.
	CreatedAt time.Time `json:"created_at"`
	// Repo is the "owner/name" the run belongs to.
	Repo string `json:"repo"`
	// Workflow is the human-readable workflow name (e.g. "CI", "Release").
	Workflow string `json:"workflow"`
	// Branch is the head branch the run executed against.
	Branch string `json:"branch"`
	// Event is the trigger (push, pull_request, schedule, release, ...).
	// It distinguishes a scheduled gremlins failure from a CI failure.
	Event string `json:"event"`
	// Conclusion is the failure flavour: failure, timed_out, or
	// startup_failure.
	Conclusion string `json:"conclusion"`
	// URL is the html_url of the run — the clickable "go fix it" link.
	URL string `json:"url"`
	// RunID is GitHub's globally-unique run identifier. It is the dedup
	// key: github-scout emits each RunID at most once per process lifetime
	// so a plain log count equals the number of distinct failures.
	RunID int64 `json:"run_id"`
	// RunNumber is the per-workflow incrementing run counter shown in the
	// GitHub UI (e.g. "#1060").
	RunNumber int64 `json:"run_number"`
}

// FailureConclusions is the set of run conclusions github-scout treats as
// actionable failures. success/neutral/skipped/cancelled are deliberately
// excluded: a cancelled run is usually a human superseding it, and skipped
// runs are non-events. The GitHub Actions API accepts each of these as a
// `status` query value, so the collector issues one query per conclusion.
//
// Ordering is stable so logs and tests see a deterministic query sequence.
var FailureConclusions = []string{"failure", "timed_out", "startup_failure"}
