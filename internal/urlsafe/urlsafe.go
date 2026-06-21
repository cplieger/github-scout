// Package urlsafe holds URL path segment safety predicates. It depends
// only on the standard library and on no other internal/* package so it
// can be imported from any layer without risking an import cycle. The
// predicate here is the single source of truth for "is this string safe
// to embed in a URL path segment"; every caller that builds a GitHub API
// URL from external input (the GITHUB_OWNER env var, repo names returned
// by the API) routes through this function.
package urlsafe

import "strings"

// IsSafeURLSegment returns true if s contains no characters that could
// break URL path construction or enable path traversal. Empty, ".", and
// ".." are rejected so the guarantee holds even as the input surface
// broadens.
func IsSafeURLSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, "/%\\?#@:")
}
