package urlsafe

import "testing"

func TestIsSafeURLSegment(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// Safe: ordinary owner/repo path segments.
		{"plain login", "cplieger", true},
		{"hyphenated repo", "github-scout", true},
		{"single char", "a", true},
		{"underscore", "repo_name", true},
		{"dotted tag-like", "v1.2.3", true},
		{"dot in middle", "a.b", true},

		// Rejected sentinels: empty and the traversal dots.
		{"empty", "", false},
		{"single dot", ".", false},
		{"double dot", "..", false},

		// Rejected: every character that could break URL path
		// construction or enable traversal/injection.
		{"slash", "a/b", false},
		{"percent", "a%2e", false},
		{"backslash", "a\\b", false},
		{"question", "a?b", false},
		{"hash", "a#b", false},
		{"at", "a@b", false},
		{"colon", "a:b", false},
		{"leading slash", "/etc", false},
		{"traversal", "../etc", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSafeURLSegment(tt.in); got != tt.want {
				t.Errorf("IsSafeURLSegment(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
