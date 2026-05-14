package cli

import (
	"fmt"
	"os"

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
		RunE: func(_ *cobra.Command, args []string) error {
			return runRm(args[0], forceDefault, force)
		},
	}
	cmd.Flags().BoolVar(&forceDefault, "force-default", false, "permit removing a repo's default-branch container; the repo will be unable to spawn new branches until re-added")
	cmd.Flags().BoolVar(&force, "force", false, "skip the /repo cleanliness check and remove even when uncommitted/unpushed work is present")
	return cmd
}

func runRm(alias string, forceDefault, force bool) error {
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
	if err := ensureRepoCleanOrForce(br, "remove", force); err != nil {
		return err
	}
	return removeBranchLocked(reg, br, forceDefault)
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
