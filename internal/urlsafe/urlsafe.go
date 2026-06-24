// Package urlsafe holds URL path segment safety predicates. It depends
// only on the standard library and on no other internal/* package so it
// can be imported from any layer without risking an import cycle. The
// predicate here is the single source of truth for "is this string safe
// to embed in a URL path segment"; every caller that builds a GitHub API
// URL from external input (the GITHUB_OWNER env var, repo names returned
// by the API) routes through this function.
package urlsafe

import "regexp"

// safeSegment matches the characters GitHub permits in an owner or repo
// path segment: ASCII letters, digits, dot, underscore, and hyphen. An
// allowlist (rather than a denylist of known-dangerous bytes) keeps the
// guarantee robust as the input surface broadens — anything outside this
// set is rejected by construction.
var safeSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// IsSafeURLSegment returns true if s is safe to embed in a URL path
// segment: it must be non-empty, not a traversal element ("." or ".."),
// and consist solely of GitHub's permitted owner/repo characters
// ([A-Za-z0-9._-]). Every caller that builds a GitHub API URL from external
// input routes through this allowlist, the single source of truth for
// path-segment safety.
func IsSafeURLSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return safeSegment.MatchString(s)
}
