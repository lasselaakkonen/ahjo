// Package git wraps the host git binary for ahjo's bare-clone + worktree flow.
package git

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func run(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func runIn(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git -C %s %s: %w", dir, strings.Join(args, " "), err)
	}
	return out, nil
}

// CloneBare clones url into dest as a bare repo.
func CloneBare(url, dest string) error {
	cmd := exec.Command("git", "clone", "--bare", url, dest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone --bare %s %s: %w", url, dest, err)
	}
	return nil
}

// Fetch runs `git fetch origin` in the bare repo.
func Fetch(barePath string) error {
	cmd := exec.Command("git", "fetch", "origin")
	cmd.Dir = barePath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// AddWorktree creates worktreePath against the bare repo, forcing branch to base.
func AddWorktree(barePath, worktreePath, branch, base string) error {
	cmd := exec.Command("git", "worktree", "add", worktreePath, "-B", branch, base)
	cmd.Dir = barePath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// RemoveWorktree removes worktreePath from barePath. Tolerant of already-gone.
func RemoveWorktree(barePath, worktreePath string) error {
	cmd := exec.Command("git", "worktree", "remove", "--force", worktreePath)
	cmd.Dir = barePath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Ignore "not a working tree" / missing-path errors; the dir might
		// already be gone, in which case prune cleans up.
		_ = exec.Command("git", "-C", barePath, "worktree", "prune").Run()
		if _, statErr := os.Stat(worktreePath); os.IsNotExist(statErr) {
			return nil
		}
		return fmt.Errorf("git worktree remove %s: %w", worktreePath, err)
	}
	return nil
}

// DefaultBranch returns the short ref name HEAD points at in the bare repo
// (e.g. "main" or "master"). For bare clones, HEAD is set to the remote's
// default branch at clone time, so this matches the upstream default without
// a network round-trip.
func DefaultBranch(barePath string) (string, error) {
	out, err := runIn(barePath, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", fmt.Errorf("git symbolic-ref HEAD in %s returned empty", barePath)
	}
	return name, nil
}

// RefExists reports whether ref resolves to an object in the bare repo.
func RefExists(barePath, ref string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = barePath
	return cmd.Run() == nil
}

// ListWorktrees returns absolute paths of every worktree (excluding the bare).
func ListWorktrees(barePath string) ([]string, error) {
	out, err := runIn(barePath, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			p := strings.TrimPrefix(line, "worktree ")
			// Skip the bare repo itself; its `worktree` line points at the bare path.
			if strings.HasSuffix(p, ".git") {
				continue
			}
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// _ keeps run reachable in case future commands need it.
var _ = run
