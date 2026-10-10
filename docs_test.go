package main

import (
	"os"
	"strings"
	"testing"
)

// githubTokenPermissions is the read-only permission set of a fine-grained
// GitHub token that every read of a default scan needs, by GitHub's table:
// https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens
var githubTokenPermissions = []string{"Actions", "Commit statuses", "Pull requests", "Issues", "Code scanning alerts"}

func TestDocs_every_github_token_instruction_names_every_permission_a_scan_needs(t *testing.T) {
	for _, path := range []string{"README.md", "docs/configuration.md", "config.example.yaml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("Setup: %v", err)
		}
		text := strings.Join(strings.Fields(strings.NewReplacer("#", " ", "*", "").Replace(string(raw))), " ")
		_, rest, found := strings.Cut(text, "fine-grained token with")
		sentence, _, _ := strings.Cut(rest, ".")
		if !found {
			t.Errorf("%s names no fine-grained GitHub token, want its permissions listed", path)
			continue
		}
		for _, p := range githubTokenPermissions {
			if !strings.Contains(sentence, p) {
				t.Errorf("%s fine-grained token permissions %q omit %q", path, sentence, p)
			}
		}
	}
}
