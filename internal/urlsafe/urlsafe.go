// Package urlsafe validates GitHub URL path segments.
//
// cplieger/registry-stats carries a separate copy of this character class with a
// 255-byte whole-reference bound and percent-decoding that this one does not have.
// Nothing synchronizes the two; a change here does not reach it.
package urlsafe

import "regexp"

// safeSegment allowlists known-safe bytes as the input surface grows.
var safeSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// IsSafeURLSegment reports whether s is a non-empty GitHub owner or repository path segment.
func IsSafeURLSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return safeSegment.MatchString(s)
}
