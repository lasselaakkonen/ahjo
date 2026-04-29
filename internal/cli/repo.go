package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func newRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Manage the repo registry",
	}
	cmd.AddCommand(newRepoAddCmd(), newRepoLsCmd(), newRepoRmCmd())
	return cmd
}

func newRepoAddCmd() *cobra.Command {
	var defaultBase string
	cmd := &cobra.Command{
		Use:   "add <name> <git-url>",
		Short: "Register a repo and bare-clone it under ~/.ahjo/repos/",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			name, url := args[0], args[1]
			if err := registry.ValidateRepoName(name); err != nil {
				return err
			}
			release, err := lockfile.Acquire()
			if err != nil {
				return err
			}
			defer release()

			reg, err := registry.Load()
			if err != nil {
				return err
			}
			if reg.FindRepo(name) != nil {
				return fmt.Errorf("repo %q already registered", name)
			}
			bare := paths.RepoBarePath(name)
			if err := os.MkdirAll(paths.ReposDir(), 0o755); err != nil {
				return err
			}
			if _, err := os.Stat(bare); err == nil {
				return fmt.Errorf("%s already exists; remove it before re-adding", bare)
			}
			if err := git.CloneBare(url, bare); err != nil {
				return err
			}
			reg.Repos = append(reg.Repos, registry.Repo{
				Name:        name,
				Remote:      url,
				BarePath:    bare,
				DefaultBase: defaultBase,
			})
			if err := reg.Save(); err != nil {
				return err
			}
			fmt.Printf("Added repo %s (default base: %s)\n", name, defaultBase)
			return nil
		},
	}
	cmd.Flags().StringVar(&defaultBase, "default-base", "main", "default branch to base new worktrees on")
	return cmd
}

func newRepoLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List registered repos",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			reg, err := registry.Load()
			if err != nil {
				return err
			}
			if len(reg.Repos) == 0 {
				fmt.Println("no repos registered")
				return nil
			}
			for _, r := range reg.Repos {
				fmt.Printf("%-20s  %s  (base: %s)\n", r.Name, r.Remote, r.DefaultBase)
			}
			return nil
		},
	}
}

func newRepoRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a repo (refuses if any worktrees exist; --force overrides)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			release, err := lockfile.Acquire()
			if err != nil {
				return err
			}
			defer release()

			reg, err := registry.Load()
			if err != nil {
				return err
			}
			repo := reg.FindRepo(name)
			if repo == nil {
				return fmt.Errorf("repo %q not registered", name)
			}
			if reg.RepoHasWorktrees(name) && !force {
				return fmt.Errorf("repo %q has worktrees; remove them or pass --force", name)
			}
			if err := os.RemoveAll(repo.BarePath); err != nil {
				return fmt.Errorf("rm %s: %w", repo.BarePath, err)
			}
			reg.RemoveRepo(name)
			if err := reg.Save(); err != nil {
				return err
			}
			fmt.Printf("Removed repo %s\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove even if worktrees exist (registry only — does NOT touch worktree containers)")
	return cmd
}
