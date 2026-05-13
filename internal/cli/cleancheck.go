package cli

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

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
func ensureRepoCleanOrForce(br *registry.Branch, action string, force bool) error {
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
		if err := incus.WaitReady(br.IncusName, 15*time.Second); err != nil {
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
	out, err := incus.Exec(containerName, "git", "-C", paths.RepoMountPath, "status", "--porcelain=v2", "--branch")
	if err != nil {
		return "", err
	}
	var staged, unstaged, untracked, unmerged, ahead int
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# branch.ab "):
			// "# branch.ab +<ahead> -<behind>"
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				if v, err := strconv.Atoi(strings.TrimPrefix(fields[2], "+")); err == nil {
					ahead = v
				}
			}
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "):
			// "1 XY ..." or "2 XY ..."; XY is the third token (after the
			// leading "1"/"2" and a space). X = staged, Y = unstaged; "."
			// means unchanged for that half.
			fields := strings.Fields(line)
			if len(fields) < 2 || len(fields[1]) < 2 {
				continue
			}
			if fields[1][0] != '.' {
				staged++
			}
			if fields[1][1] != '.' {
				unstaged++
			}
		case strings.HasPrefix(line, "u "):
			unmerged++
		case strings.HasPrefix(line, "? "):
			untracked++
		}
	}
	var parts []string
	if staged > 0 {
		parts = append(parts, fmt.Sprintf("%d staged", staged))
	}
	if unstaged > 0 {
		parts = append(parts, fmt.Sprintf("%d unstaged", unstaged))
	}
	if untracked > 0 {
		parts = append(parts, fmt.Sprintf("%d untracked", untracked))
	}
	if unmerged > 0 {
		parts = append(parts, fmt.Sprintf("%d unmerged", unmerged))
	}
	if ahead > 0 {
		parts = append(parts, fmt.Sprintf("%d unpushed commit(s)", ahead))
	}
	return strings.Join(parts, ", "), nil
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
