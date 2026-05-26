package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	sshpkg "github.com/lasselaakkonen/ahjo/internal/ssh"
)

func newRmCmd() *cobra.Command {
	var forceDefault bool
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <alias>",
		Short: "Stop+delete the branch container, free its port, drop the registry entry",
		Long: `Removes the branch's Incus container, frees its SSH port, and drops the
registry entry. Refuses to remove a repo's default-branch container (the COW
source for every other branch in the repo) unless --force-default is passed.

Before tearing down, ahjo inspects /repo for uncommitted/unpushed work:
  - running container: checked in place
  - stopped container: ahjo prompts to start it for the check

If /repo is dirty (or the user declines to start a stopped container), the
command refuses to proceed. Pass --force to skip the check and remove anyway.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRm(cmd.Context(), args[0], forceDefault, force)
		},
	}
	cmd.Flags().BoolVar(&forceDefault, "force-default", false, "permit removing a repo's default-branch container; the repo will be unable to spawn new branches until re-added")
	cmd.Flags().BoolVar(&force, "force", false, "skip the /repo cleanliness check and remove even when uncommitted/unpushed work is present")
	return cmd
}

func runRm(ctx context.Context, alias string, forceDefault, force bool) error {
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
	if err := ensureRepoCleanOrForce(ctx, br, "remove", force); err != nil {
		return err
	}
	wasNonDefault := !br.IsDefault
	repoName := br.Repo
	if err := removeBranchLocked(reg, br, forceDefault); err != nil {
		return err
	}
	if wasNonDefault {
		spawnRefreshBase(repoName)
	}
	return nil
}

// spawnRefreshBase fires off `ahjo _refresh-base <repo-name>` as a detached
// subprocess so the repo's base container is restarted and fast-forwarded
// against origin while the user's shell returns from `ahjo rm`. The child
// blocks on the ahjo lockfile (held by this process until runRm returns),
// so a follow-up `ahjo create` queues behind it instead of COWing a base
// that's mid-pull. Best-effort: any failure here just means no prefetch —
// the next `ahjo create` still works, it just won't have the latest commits.
//
// Detachment specifics:
//   - SysProcAttr.Setsid=true puts the child in its own session/process group
//     so a Ctrl+C in the parent's terminal between cmd.Start() and runRm's
//     return doesn't take the prefetch down with it.
//   - Stdout/Stderr are redirected to ~/.ahjo/refresh-base.log (append) so a
//     silent failure is debuggable; if the log can't be opened we fall back
//     to /dev/null rather than skipping the prefetch.
//   - cmd.Process.Release() drops Go's bookkeeping; the kernel reparents to
//     init so the orphan isn't waiting on us to reap.
func spawnRefreshBase(repoName string) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: skip base refresh (resolve ahjo binary): %v\n", err)
		return
	}

	logPath := paths.RefreshBaseLogPath()
	var logFile *os.File
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		logFile = f
	} else {
		fmt.Fprintf(cobraOutErr(), "warn: base refresh log %s unavailable (%v); output dropped\n", logPath, err)
	}

	cmd := exec.Command(exe, "_refresh-base", repoName)
	cmd.Stdin = nil
	cmd.Stdout = logFile // nil → /dev/null
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: spawn base refresh: %v\n", err)
		if logFile != nil {
			logFile.Close()
		}
		return
	}
	// Parent's copy of the log fd was dup'd into the child by os/exec; close
	// ours so we don't keep an extra fd open after rm exits.
	if logFile != nil {
		logFile.Close()
	}
	_ = cmd.Process.Release()
	if logFile != nil {
		fmt.Printf("→ refreshing %s base container in background (log: %s)\n", repoName, logPath)
	} else {
		fmt.Printf("→ refreshing %s base container in background\n", repoName)
	}
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

	_ = os.RemoveAll(paths.SlugHostKeysDir(br.HostKeysSlug()))

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
	// dangle a half-broken entry — and drop the per-repo PAT so a future
	// `ahjo repo add` of the same URL prompts for a fresh one instead of
	// silently reusing the stale one. dropRepoToken handles both Linux (file
	// remove) and Mac (Keychain cleanup marker for the shim to sweep).
	if wasDefault {
		reg.RemoveRepo(repoName)
		dropRepoToken(repoName)
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
