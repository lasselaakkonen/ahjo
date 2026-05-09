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
	cmd := &cobra.Command{
		Use:   "rm <alias>",
		Short: "Stop+delete the branch container, free its port, drop the registry entry",
		Long: `Removes the branch's Incus container, frees its SSH port, and drops the
registry entry. Refuses to remove a repo's default-branch container (the COW
source for every other branch in the repo) unless --force-default is passed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRm(args[0], forceDefault)
		},
	}
	cmd.Flags().BoolVar(&forceDefault, "force-default", false, "permit removing a repo's default-branch container; the repo will be unable to spawn new branches until re-added")
	return cmd
}

func runRm(alias string, forceDefault bool) error {
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
	if br.IsDefault && !forceDefault {
		return fmt.Errorf("%s is the repo's default-branch container; pass --force-default to remove it (other branches in this repo will need `ahjo repo add` again before new branches can be spawned)", br.Aliases[0])
	}

	if name := br.IncusName; name != "" {
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
	reg.RemoveBranch(repoName, br.Branch)

	// If we just removed the default branch, the repo entry can no longer
	// spawn new branches. Drop the repo row too so `ahjo repo ls` doesn't
	// dangle a half-broken entry.
	if br.IsDefault {
		reg.RemoveRepo(repoName)
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
