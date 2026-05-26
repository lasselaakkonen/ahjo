package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

// ensureRepoCleanOrForce gates a destructive op on /repo being clean inside
// br's container:
//
//   - force=true: skip every check.
//   - container running: inspect /repo; dirty → error pointing at --force.
//   - container stopped/missing: prompt to start it for the check. "No" →
//     error pointing at --force. "Yes" → start, then check.
//
// action is the verb-phrase ("remove", "recreate") spliced into the error so
// the user sees which op --force would unlock.
func ensureRepoCleanOrForce(ctx context.Context, br *registry.Branch, action string, force bool) error {
	if force {
		return nil
	}
	if br.IncusName == "" {
		return nil
	}
	status, err := incus.ContainerStatus(br.IncusName)
	if err != nil {
		return fmt.Errorf("inspect %s: %w; pass --force to %s without checking", br.IncusName, err, action)
	}
	if status == "" {
		return nil
	}
	if !strings.EqualFold(status, "Running") {
		if !promptYesNo(fmt.Sprintf("%s is %s — start it to check /repo for unsaved work?", br.IncusName, strings.ToLower(status))) {
			return fmt.Errorf("cannot verify /repo in %s is clean (container not started); pass --force to %s without checking", br.IncusName, action)
		}
		if err := incus.Start(br.IncusName); err != nil {
			return fmt.Errorf("start %s for clean-check: %w", br.IncusName, err)
		}
		if err := incus.WaitReady(ctx, br.IncusName, 15*time.Second); err != nil {
			return fmt.Errorf("wait %s ready: %w", br.IncusName, err)
		}
	}
	summary, err := repoDirtySummary(br.IncusName)
	if err != nil {
		return fmt.Errorf("inspect /repo in %s: %w; pass --force to %s without checking", br.IncusName, err, action)
	}
	if summary == "" {
		return nil
	}
	return fmt.Errorf("/repo in %s is not clean: %s; pass --force to %s anyway", br.IncusName, summary, action)
}

// repoDirtySummary execs git status inside containerName (which must be
// running) and returns a short human-readable description of any
// uncommitted/untracked/unmerged/unpushed state, or "" when clean.
func repoDirtySummary(containerName string) (string, error) {
	// -c safe.directory=<repo>: incus.Exec runs as root, but /repo is owned
	// by the container's ubuntu user — without this override git refuses
	// with "fatal: detected dubious ownership". Same workaround as
	// branch_status.go's status call.
	out, err := incus.Exec(containerName, "git", "-c", "safe.directory="+paths.RepoMountPath,
		"-C", paths.RepoMountPath, "status", "--porcelain=v2", "--branch")
	if err != nil {
		return "", err
	}
	return git.ParseStatusV2(string(out)).Summary(), nil
}

func promptYesNo(question string) bool {
	fmt.Printf("%s [y/N] ", question)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(sc.Text()))
	return ans == "y" || ans == "yes"
}
