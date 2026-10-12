package github

import (
	"strings"
)

// maxBranchName bounds a branch name that is sent to GitHub in a URL path.
const maxBranchName = 255

// checkBranchName refuses a branch name before it becomes part of a request path. It applies the
// rules of git check-ref-format, which GitHub enforces for the branches it stores, so that a name
// GitHub could never hold is answered here as invalid and not asked about, plus the sequences that
// have no business in a branch name even where GitHub allows them: a name is escaped per path
// segment (escapeSegments), and a segment of "." or ".." would be resolved by GitHub as a
// directory step out of /branches/ and into any other endpoint the installation token reaches.
func checkBranchName(name string) error {
	if strings.TrimSpace(name) == "" {
		return invalid("the branch name is required")
	}
	bad := func() error { return invalid("%q is not a valid Git branch name", name) }
	if len(name) > maxBranchName || name == "@" ||
		strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") {
		return bad()
	}
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f, r == ' ', r == '~', r == '^', r == ':', r == '?', r == '*', r == '[', r == '\\':
			return bad()
		}
	}
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return bad()
		}
	}
	// An encoded dot, slash or backslash would be read as a separator by a layer that decodes the
	// path once more than this one did.
	lower := strings.ToLower(name)
	for _, enc := range []string{"%2e", "%2f", "%5c"} {
		if strings.Contains(lower, enc) {
			return bad()
		}
	}
	return nil
}

// hasDotSegment reports whether the path part of a request path (before any query) has a segment
// that is "." or "..".
func hasDotSegment(path string) bool {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}
