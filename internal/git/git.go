// Package git wraps the host git binary. Phase 1 of no-more-worktrees
// removed every helper that operated on a host-side bare repo or worktree
// (CloneBare, AddWorktree, RemoveWorktree, Fetch, DefaultBranch, RefExists,
// ListWorktrees) — every git operation now runs inside the branch
// container via `incus exec` from cli/repo.go and cli/new.go.
//
// The package now hosts host-side identity resolution (see identity.go)
// used by `ahjo repo add` to seed `/home/ubuntu/.gitconfig` inside the
// repo's default-branch container.
package git
