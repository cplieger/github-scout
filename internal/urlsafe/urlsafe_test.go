package urlsafe

import (
	"net/url"
	"strings"
	"testing"
)

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

		// Allowlist rejects: characters that survive url.Parse but fall
		// outside GitHub's owner/repo charset, so the allowlist denies
		// them where the former denylist let them through.
		{"space", "a b", false},
		{"pipe", "a|b", false},
		{"caret", "a^b", false},
		{"semicolon", "a;b", false},
		{"backtick", "a`b", false},
		{"open bracket", "a[b", false},
		{"tilde", "a~b", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSafeURLSegment(tt.in); got != tt.want {
				t.Errorf("IsSafeURLSegment(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// FuzzIsSafeURLSegment asserts the safety contract through a code path
// independent of the predicate's implementation: any segment the predicate
// accepts must be non-empty, must not be a traversal element, must contain
// none of the URL-structure-breaking characters, and must be
// percent-decode-inert (url.PathUnescape returns it unchanged). The
// decode-inert invariant runs through a different code path than the
// allowlist, killing mutants that would weaken it to admit a percent-encoded
// byte.
func FuzzIsSafeURLSegment(f *testing.F) {
	for _, seed := range []string{
		"cplieger", "github-scout", "v1.2.3", "repo_name",
		"", ".", "..", "a/b", "a%2e", "a b", "a|b", "../etc",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !IsSafeURLSegment(s) {
			return // rejected inputs carry no obligations
		}
		if s == "" || s == "." || s == ".." {
			t.Errorf("accepted %q, but it is empty or a traversal element", s)
		}
		if strings.ContainsAny(s, "/\\?#") {
			t.Errorf("accepted %q, but it contains a URL-structure-breaking character", s)
		}
		if dec, err := url.PathUnescape(s); err != nil || dec != s {
			t.Errorf("accepted %q, but it is not percent-decode-inert (decoded=%q, err=%v)", s, dec, err)
		}
	})
}
