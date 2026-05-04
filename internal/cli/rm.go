package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/coi"
	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	sshpkg "github.com/lasselaakkonen/ahjo/internal/ssh"
)

func newRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <alias>",
		Short: "Stop+delete the container, remove the worktree, free ports, drop the registry entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRm(args[0])
		},
	}
}

func runRm(alias string) error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return err
	}
	w := reg.FindWorktreeByAlias(alias)
	if w == nil {
		fmt.Printf("no worktree with alias %q; nothing to do\n", alias)
		return nil
	}
	repo := reg.FindRepo(w.Repo)

	if containerName, err := resolveContainerName(w); err != nil {
		fmt.Fprintf(cobraOutErr(), "note: skipping container shutdown/delete: %v\n", err)
	} else {
		if err := coi.Shutdown(containerName); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: coi shutdown: %v\n", err)
		}
		if err := coi.ContainerDelete(containerName); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: container delete: %v\n", err)
		}
	}

	if repo != nil {
		if err := git.RemoveWorktree(repo.BarePath, w.WorktreePath); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: worktree remove: %v\n", err)
		}
	}
	// Defensive: ensure the dir is gone even if git refused.
	_ = os.RemoveAll(w.WorktreePath)
	_ = os.RemoveAll(w.SSHHostKeysDir)

	pp, err := ports.Load()
	if err != nil {
		return err
	}
	pp.FreeSlug(w.Slug)
	if err := pp.Save(); err != nil {
		return err
	}

	primary := w.Aliases[0]
	reg.RemoveWorktree(w.Repo, w.Branch)
	if err := reg.Save(); err != nil {
		return err
	}
	if err := sshpkg.RegenerateConfig(reg); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", primary)
	return nil
}
