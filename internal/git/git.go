// Package git wraps the host git binary. Phase 1 of no-more-worktrees
// removed every helper that operated on a host-side bare repo or worktree
// (CloneBare, AddWorktree, RemoveWorktree, Fetch, DefaultBranch, RefExists,
// ListWorktrees) — every git operation now runs inside the branch
// container via `incus exec` from cli/repo.go and cli/new.go.
//
// The package is kept as a placeholder for any future host-side git work
// (e.g. validating a URL before container launch).
package git
