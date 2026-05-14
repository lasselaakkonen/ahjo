package cli

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	"github.com/lasselaakkonen/ahjo/internal/repotoken"
	sshpkg "github.com/lasselaakkonen/ahjo/internal/ssh"
)

func newRmCmd() *cobra.Command {
	var forceDefault bool
	var yes bool
	cmd := &cobra.Command{
		Use:   "rm <alias>",
		Short: "Stop+delete the branch container, free its port, drop the registry entry",
		Long: `Removes the branch's Incus container, frees its SSH port, and drops the
registry entry. Refuses to remove a repo's default-branch container (the COW
source for every other branch in the repo) unless --force-default is passed.

If the container is running, ahjo inspects /repo inside the container for
uncommitted/unpushed work and prompts for confirmation before proceeding.
Pass -y/--yes to skip that prompt.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRm(args[0], forceDefault, yes)
		},
	}
	cmd.Flags().BoolVar(&forceDefault, "force-default", false, "permit removing a repo's default-branch container; the repo will be unable to spawn new branches until re-added")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the dirty-/repo confirmation prompt")
	return cmd
}

func runRm(alias string, forceDefault, yes bool) error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return err
	}
	br := reg.FindBranchByAlias(alias)
	if br == nil {
		fmt.Printf("no branch with alias %q; nothing to do\n", alias)
		return nil
	}
	if !yes && br.IncusName != "" {
		if abort, err := confirmDirtyBeforeRm(br); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: could not inspect /repo in %s: %v\n", br.IncusName, err)
		} else if abort {
			fmt.Println("aborted")
			return nil
		}
	}
	return removeBranchLocked(reg, br, forceDefault)
}

// confirmDirtyBeforeRm decides whether to prompt the user before tearing
// down a container, based on whether /repo inside it has unsaved work.
//
//   - Running container: check now. If dirty, ask "remove anyway?".
//   - Stopped/missing container: ask the user whether to start it for a
//     check first. If they decline, proceed without a check. If they
//     accept, start, check, and (when dirty) ask "remove anyway?" — we
//     leave the container running on abort, because the user just opted
//     into starting it.
//
// Returns abort=true when the user said "no" to removal.
func confirmDirtyBeforeRm(br *registry.Branch) (abort bool, err error) {
	status, err := incus.ContainerStatus(br.IncusName)
	if err != nil {
		return false, err
	}
	if status == "" {
		return false, nil
	}
	if !strings.EqualFold(status, "Running") {
		if !promptYesNo(fmt.Sprintf("%s is %s — start it to check /repo for unsaved work?", br.IncusName, strings.ToLower(status))) {
			return false, nil
		}
		if err := incus.Start(br.IncusName); err != nil {
			return false, fmt.Errorf("start for clean-check: %w", err)
		}
		if err := incus.WaitReady(br.IncusName, 15*time.Second); err != nil {
			return false, fmt.Errorf("wait ready: %w", err)
		}
	}
	summary, err := repoDirtySummary(br.IncusName)
	if err != nil {
		return false, err
	}
	if summary == "" {
		return false, nil
	}
	fmt.Printf("/repo in %s is not clean: %s\n", br.IncusName, summary)
	if promptYesNo(fmt.Sprintf("Remove %s anyway?", br.Aliases[0])) {
		return false, nil
	}
	return true, nil
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

// removeBranchLocked tears down one branch's container + ports + host-keys +
// registry rows, persists the registry, and regenerates ssh-config. Caller
// must already hold the ahjo lockfile.
func removeBranchLocked(reg *registry.Registry, br *registry.Branch, forceDefault bool) error {
	if br.IsDefault && !forceDefault {
		return fmt.Errorf("%s is the repo's default-branch container; pass --force-default to remove it (other branches in this repo will need `ahjo repo add` again before new branches can be spawned)", br.Aliases[0])
	}

	if name := br.IncusName; name != "" {
		if err := stopAndRemoveMirror(name); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: stop mirror on %s: %v\n", name, err)
		}
		fmt.Printf("→ incus stop %s\n", name)
		if err := incus.Stop(name); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: incus stop: %v\n", err)
		}
		fmt.Printf("→ incus delete --force %s\n", name)
		if err := incus.ContainerDeleteForce(name); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: incus delete: %v\n", err)
		}
	}

	_ = os.RemoveAll(paths.SlugHostKeysDir(br.Slug))

	pp, err := ports.Load()
	if err != nil {
		return err
	}
	pp.FreeSlug(br.Slug)
	if err := pp.Save(); err != nil {
		return err
	}

	primary := br.Aliases[0]
	repoName := br.Repo
	wasDefault := br.IsDefault
	reg.RemoveBranch(repoName, br.Branch)

	// If we just removed the default branch, the repo entry can no longer
	// spawn new branches. Drop the repo row too so `ahjo repo ls` doesn't
	// dangle a half-broken entry.
	if wasDefault {
		reg.RemoveRepo(repoName)
		// The per-repo PAT lives at ~/.ahjo/repo-tokens/<slug>.env; rm it
		// once the repo is actually gone from the registry. Best-effort —
		// missing file is normal (non-GitHub repo, or user pre-cleaned).
		if err := repotoken.Delete(repoName); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: rm token for %s: %v\n", repoName, err)
		} else {
			fmt.Printf("note: revoke the PAT for %s in GitHub if you no longer need it:\n  https://github.com/settings/personal-access-tokens\n", repoName)
		}
	}

	if err := reg.Save(); err != nil {
		return err
	}
	if err := sshpkg.RegenerateConfig(reg); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", primary)
	return nil
}
