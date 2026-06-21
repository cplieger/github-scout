package github

import (
	"encoding/json"
	"testing"
)

// FuzzDecodeRunsPage exercises the JSON decode of the /actions/runs
// response — the boundary where github-scout ingests untrusted bytes from
// the GitHub API (or anything impersonating it via a redirect/MITM). The
// decode must never panic regardless of input; malformed input must surface
// as an error, and well-formed input must populate the run fields. This is
// a parser over an external-input boundary, so it gets a fuzz target per
// the repo's testing policy.
func FuzzDecodeRunsPage(f *testing.F) {
	f.Add(`{"workflow_runs":[{"id":1,"name":"CI","conclusion":"failure","created_at":"2026-06-20T10:00:00Z"}]}`)
	f.Add(`{"workflow_runs":[]}`)
	f.Add(`{}`)
	f.Add(``)
	f.Add(`{"workflow_runs":null}`)
	f.Add(`{"workflow_runs":[{"id":"not-a-number"}]}`)
	f.Add(`{"workflow_runs":[{"created_at":"garbage-timestamp"}]}`)
	f.Add(`[1,2,3]`)

	f.Fuzz(func(t *testing.T, data string) {
		var page apiRunsPage
		// The contract: Unmarshal either succeeds or errors, never panics.
		// On success, every decoded run must have a non-negative ID (the
		// dedup key) — a negative ID would corrupt the seen-set.
		if err := json.Unmarshal([]byte(data), &page); err != nil {
			return // malformed input rejected cleanly — acceptable
		}
		for _, r := range page.WorkflowRuns {
			if r.ID < 0 {
				t.Errorf("decoded negative run ID %d from %q", r.ID, data)
			}
		}
	})
}
