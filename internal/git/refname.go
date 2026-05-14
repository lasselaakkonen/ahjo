package git

import (
	"regexp"
	"strings"
)

var (
	// Anything outside [A-Za-z0-9/_.-] becomes a separator. We then collapse
	// runs of those separators below.
	branchInvalidRE = regexp.MustCompile(`[^A-Za-z0-9/_.-]+`)
	branchSlashesRE = regexp.MustCompile(`/+`)
	branchDashesRE  = regexp.MustCompile(`-+`)
)

// SanitizeBranchName converts a freeform string into a branch name accepted
// by both `git check-ref-format --branch` and GitHub's ref rules: it keeps
// letters, digits, and `/ _ . -`, collapses runs of any other characters
// (including whitespace) to a single `-`, strips the leading/trailing
// trash that git rejects (`.`, `/`, `-`), drops `..` sequences and a
// `.lock` suffix. Case is preserved.
//
// Returns "" if nothing usable is left (caller should reject).
func SanitizeBranchName(s string) string {
	s = branchInvalidRE.ReplaceAllString(s, "-")
	s = branchSlashesRE.ReplaceAllString(s, "/")
	s = branchDashesRE.ReplaceAllString(s, "-")
	for strings.Contains(s, "..") {
		s = strings.ReplaceAll(s, "..", ".")
	}
	s = strings.Trim(s, "-/.")
	s = strings.TrimSuffix(s, ".lock")
	s = strings.Trim(s, "-/.")
	return s
}
