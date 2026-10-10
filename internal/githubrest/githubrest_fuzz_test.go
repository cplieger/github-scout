package githubrest

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// FuzzDecodeCodeAlerts drives the code-scanning page decode with untrusted
// bytes: a decoded page is an array of at most one page's rows, each mapped to
// repo's code-scanning alert, and null is never a page.
func FuzzDecodeCodeAlerts(f *testing.F) {
	f.Add(`[{"number":3,"rule":{"id":"x","security_severity_level":"high"},"tool":{"name":"CodeQL"}}]`)
	f.Add(`[]`)
	f.Add(`null`)
	f.Add(``)
	f.Add(`[{"number":-1}]`)
	f.Add(`{"not":"an array"}`)
	f.Add("[" + strings.Repeat(`{"number":1},`, perPage) + `{"number":2}]`)
	f.Fuzz(func(t *testing.T, data string) {
		alerts, err := decodeAlerts("o/r", []byte(data))
		if err != nil {
			return
		}
		if bytes.Equal(bytes.TrimSpace([]byte(data)), []byte("null")) {
			t.Fatalf("decodeAlerts(null) = %d alerts, nil error, want a decode failure", len(alerts))
		}
		if len(alerts) > perPage {
			t.Fatalf("decodeAlerts = %d alerts, nil error, want a page of at most %d rows", len(alerts), perPage)
		}
		for _, a := range alerts {
			if a.Repo != "o/r" || a.Source != "code_scanning" {
				t.Fatalf("decodeAlerts(%q) row = %+v, want repo o/r and source code_scanning", data, a)
			}
		}
	})
}

// FuzzDecodeWorkflows drives the workflows page decode on the same terms:
// every row is kept as one of repo's definitions in its own state, never more
// rows than a page asks for, a page without a workflows array is never an
// empty page, and a page read whole names only GitHub's five states.
func FuzzDecodeWorkflows(f *testing.F) {
	f.Add(`{"total_count":1,"workflows":[{"name":"CI","path":".github/workflows/ci.yml","state":"disabled_inactivity"}]}`)
	f.Add(`{"workflows":[{"name":"CI","state":"active"},{"name":"Old","state":"disabled_manually"}]}`)
	f.Add(`{"workflows":[{"state":"deleted"},{"state":"disabled_fork"}]}`)
	f.Add(`{"workflows":[{"name":"nightly","state":"disabled_repository"}]}`)
	f.Add(`{"workflows":[{"name":"nameless"}]}`)
	f.Add(`{"workflows":null}`)
	f.Add(`{}`)
	f.Add(``)
	f.Add(`{"workflows":[{"state":7}]}`)
	f.Add(`[1,2]`)
	f.Add(`{"workflows":[` + strings.Repeat(`{"state":"disabled_manually"},`, perPage) + `{"state":"active"}]}`)
	known := map[string]bool{"active": true, "deleted": true, "disabled_fork": true, "disabled_inactivity": true, "disabled_manually": true}
	f.Fuzz(func(t *testing.T, data string) {
		defs, err := decodeWorkflows("o/r", []byte(data))
		if err != nil {
			return
		}
		if len(defs) > perPage {
			t.Fatalf("decodeWorkflows(%q) = %d rows, want at most %d", data, len(defs), perPage)
		}
		for _, w := range defs {
			if w.Repo != "o/r" || !known[w.State] {
				t.Fatalf("decodeWorkflows(%q) kept %+v, want o/r's definitions in a documented state", data, w)
			}
		}
		var oracle struct {
			Workflows []struct {
				State string `json:"state"`
			} `json:"workflows"`
		}
		if json.Unmarshal([]byte(data), &oracle) != nil || len(oracle.Workflows) != len(defs) {
			return
		}
		for i, w := range oracle.Workflows {
			if w.State != defs[i].State {
				t.Fatalf("decodeWorkflows(%q) row %d state %q, want %q", data, i, defs[i].State, w.State)
			}
		}
	})
}
