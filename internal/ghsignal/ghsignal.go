// Package ghsignal defines GitHub signals emitted by github-scout.
package ghsignal

import (
	"errors"
	"slices"
	"time"

	"github.com/cplieger/runesafe/v2"
)

// ErrNoCodeScanning marks a benign 404 with no code-scanning analyses.
var ErrNoCodeScanning = errors.New("repo has no code-scanning analyses")

// ErrTokenInvalid and ErrRateLimited mark failures that affect every signal.
var (
	ErrTokenInvalid = errors.New("github token rejected (401)")
	ErrRateLimited  = errors.New("github rate limit exceeded (429)")
)

// Repo is a GitHub repository discovered for an owner.
type Repo struct {
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
	// Fork alerts describe inherited upstream code; other signals remain in scope.
	Fork bool `json:"fork"`
}

// FullName returns the canonical owner/name identifier.
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// WorkflowRun is a completed GitHub Actions workflow run.
type WorkflowRun struct {
	CreatedAt time.Time `json:"created_at"`
	Repo      string    `json:"repo"`
	Workflow  string    `json:"workflow"`
	// Branch is untrusted upstream text.
	Branch     runesafe.Untrusted `json:"branch"`
	Event      string             `json:"event"`
	Conclusion string             `json:"conclusion"`
	URL        string             `json:"url"`
	// RunID is globally unique and serves as the deduplication key.
	RunID     int64 `json:"run_id"`
	RunNumber int64 `json:"run_number"`
}

// PullRequest is an open pull-request snapshot.
type PullRequest struct {
	CreatedAt time.Time `json:"created_at"`
	Repo      string    `json:"repo"`
	// Title is untrusted upstream text.
	Title  runesafe.Untrusted `json:"title"`
	Author string             `json:"author"`
	URL    string             `json:"url"`
	// Number is unique within Repo.
	Number int64 `json:"number"`
	Draft  bool  `json:"draft"`
}

// Issue is an open issue snapshot.
type Issue struct {
	CreatedAt time.Time `json:"created_at"`
	Repo      string    `json:"repo"`
	// Title is untrusted upstream text.
	Title  runesafe.Untrusted `json:"title"`
	Author string             `json:"author"`
	// Labels are flattened for log output.
	Labels string `json:"labels"`
	URL    string `json:"url"`
	Number int64  `json:"number"`
}

// CodeScanningAlert is an open code-scanning alert snapshot.
type CodeScanningAlert struct {
	CreatedAt time.Time `json:"created_at"`
	Repo      string    `json:"repo"`
	Rule      string    `json:"rule"`
	Severity  string    `json:"severity"`
	Tool      string    `json:"tool"`
	URL       string    `json:"url"`
	Number    int64     `json:"number"`
}

// failureConclusions excludes cancelled runs, which are not failures.
var failureConclusions = []string{"failure", "timed_out", "startup_failure"}

// IsFailureConclusion reports whether conclusion is actionable.
func IsFailureConclusion(conclusion string) bool {
	return slices.Contains(failureConclusions, conclusion)
}
