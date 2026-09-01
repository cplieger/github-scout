// Package urlsafe validates GitHub URL path segments.
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
