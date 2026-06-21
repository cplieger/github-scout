package github

import (
	"encoding/json"
	"testing"
)

// These fuzz targets cover the JSON-decode boundaries where github-scout
// ingests untrusted bytes from the GitHub API (or anything impersonating it
// via a redirect / MITM). The invariant is robustness, not field validation:
// json.Unmarshal must either succeed or error — never panic — on arbitrary
// input, and the downstream pure helpers that consume the decoded structs
// (repoFromAPIURL) must not panic on whatever was decoded. Numeric fields are
// display-only (or harmless map keys), so no value-range invariant applies.

// FuzzDecodeRunsPage covers the /actions/runs response decode.
func FuzzDecodeRunsPage(f *testing.F) {
	f.Add(`{"workflow_runs":[{"id":1,"name":"CI","conclusion":"failure","created_at":"2026-06-20T10:00:00Z"}]}`)
	f.Add(`{"workflow_runs":[]}`)
	f.Add(`{"workflow_runs":null}`)
	f.Add(`{}`)
	f.Add(``)
	f.Add(`{"workflow_runs":[{"id":"not-a-number"}]}`)
	f.Add(`{"workflow_runs":[{"created_at":"garbage-timestamp"}]}`)
	f.Add(`[1,2,3]`)

	f.Fuzz(func(_ *testing.T, data string) {
		var page apiRunsPage
		_ = json.Unmarshal([]byte(data), &page) // must not panic
	})
}

// FuzzDecodeSearchResp covers the /search/issues response decode (PRs +
// issues) and the repoFromAPIURL helper applied to decoded input.
func FuzzDecodeSearchResp(f *testing.F) {
	f.Add(`{"items":[{"number":1,"title":"x","user":{"login":"a"},"labels":[{"name":"bug"}],"repository_url":"https://api.github.com/repos/o/r"}]}`)
	f.Add(`{"items":[]}`)
	f.Add(`{"items":null}`)
	f.Add(`{}`)
	f.Add(``)
	f.Add(`{"items":[{"number":"nope"}]}`)
	f.Add(`{"items":[{"draft":true,"created_at":"bad-ts","repository_url":"garbage"}]}`)

	f.Fuzz(func(_ *testing.T, data string) {
		var resp apiSearchResp
		if json.Unmarshal([]byte(data), &resp) != nil {
			return
		}
		for i := range resp.Items {
			_ = repoFromAPIURL(resp.Items[i].RepositoryURL) // must not panic
		}
	})
}

// FuzzDecodeCodeAlerts covers the code-scanning alerts response decode (an
// array, not an envelope).
func FuzzDecodeCodeAlerts(f *testing.F) {
	f.Add(`[{"number":3,"rule":{"id":"x","security_severity_level":"high"},"tool":{"name":"CodeQL"}}]`)
	f.Add(`[]`)
	f.Add(`null`)
	f.Add(``)
	f.Add(`[{"number":-1}]`)
	f.Add(`{"not":"an array"}`)

	f.Fuzz(func(_ *testing.T, data string) {
		var alerts []apiCodeAlert
		_ = json.Unmarshal([]byte(data), &alerts) // must not panic
	})
}
