package model

import "testing"

func TestRepoFullName(t *testing.T) {
	r := Repo{Owner: "cplieger", Name: "github-scout"}
	if got := r.FullName(); got != "cplieger/github-scout" {
		t.Errorf("FullName() = %q, want cplieger/github-scout", got)
	}
}

func TestFailureConclusionsStable(t *testing.T) {
	// The dashboard and any Loki ruler alert assume these exact values;
	// guard against an accidental reorder/rename that would silently drop
	// a failure flavour from the scan.
	want := []string{"failure", "timed_out", "startup_failure"}
	if len(failureConclusions) != len(want) {
		t.Fatalf("failureConclusions = %v, want %v", failureConclusions, want)
	}
	for i, w := range want {
		if failureConclusions[i] != w {
			t.Errorf("failureConclusions[%d] = %q, want %q", i, failureConclusions[i], w)
		}
	}
}

func TestIsFailureConclusion(t *testing.T) {
	// Only the failureConclusions set counts as a failure. success and
	// cancelled/skipped/neutral (and the empty conclusion of an in-flight
	// run that slipped through) must NOT count — they would dilute the
	// failure rate and over-report new_failures.
	cases := map[string]bool{
		"failure":         true,
		"timed_out":       true,
		"startup_failure": true,
		"success":         false,
		"cancelled":       false,
		"skipped":         false,
		"neutral":         false,
		"":                false,
	}
	for conclusion, want := range cases {
		if got := IsFailureConclusion(conclusion); got != want {
			t.Errorf("IsFailureConclusion(%q) = %v, want %v", conclusion, got, want)
		}
	}
}
