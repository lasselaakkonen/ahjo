package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/lima"
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
	var asAlias string
	cmd := &cobra.Command{
		Use:   "add <git-url>",
		Short: "Register a repo and bare-clone it under ~/.ahjo/repos/",
		Long: `Add a repo. The auto alias is derived from the URL as <owner>/<repo>.
Pass --as <alias> to register an additional alias for the same repo.
On auto-alias collision (e.g. github.com/acme/api vs gitlab.com/acme/api),
ahjo appends -2/-3/... to keep aliases unique.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRepoAdd(args[0], asAlias, defaultBase)
		},
	}
	cmd.Flags().StringVar(&defaultBase, "default-base", "", "default branch to base new worktrees on (default: detect from the remote's HEAD)")
	cmd.Flags().StringVar(&asAlias, "as", "", "additional alias for this repo (must not collide with any existing alias)")
	return cmd
}

func runRepoAdd(url, asAlias, defaultBase string) error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return err
	}

	primary, err := reg.AllocateRepoAlias(url)
	if err != nil {
		return err
	}
	aliases := []string{primary}
	if asAlias != "" {
		if err := registry.ValidateAlias(asAlias); err != nil {
			return err
		}
		if asAlias == primary {
			// Already covered; silently de-dupe.
		} else if reg.AliasInUse(asAlias) {
			return fmt.Errorf("alias %q already in use; pick another --as value", asAlias)
		} else {
			aliases = append(aliases, asAlias)
		}
	}

	slug := reg.AllocateRepoSlug(primary)
	bare := paths.RepoBarePath(slug)
	if err := os.MkdirAll(paths.ReposDir(), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(bare); err == nil {
		return fmt.Errorf("%s already exists; remove it before re-adding", bare)
	}
	if err := git.CloneBare(url, bare); err != nil {
		return wrapCloneErr(err)
	}
	if defaultBase == "" {
		detected, err := git.DefaultBranch(bare)
		if err != nil {
			return fmt.Errorf("detect default branch (pass --default-base to override): %w", err)
		}
		defaultBase = detected
	}
	reg.Repos = append(reg.Repos, registry.Repo{
		Name:        slug,
		Aliases:     aliases,
		Remote:      url,
		BarePath:    bare,
		DefaultBase: defaultBase,
	})
	if err := reg.Save(); err != nil {
		return err
	}
	fmt.Printf("Added repo %s (aliases: %s, default base: %s)\n",
		slug, strings.Join(aliases, ", "), defaultBase)
	return nil
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
				fmt.Printf("%-30s  %s  (base: %s)\n",
					strings.Join(r.Aliases, ","), r.Remote, r.DefaultBase)
			}
			return nil
		},
	}
}

func newRepoRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <alias>",
		Short: "Remove a repo by any of its aliases (refuses if any worktrees exist; --force overrides)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			alias := args[0]
			release, err := lockfile.Acquire()
			if err != nil {
				return err
			}
			defer release()

			reg, err := registry.Load()
			if err != nil {
				return err
			}
			repo := reg.FindRepoByAlias(alias)
			if repo == nil {
				return fmt.Errorf("no repo with alias %q", alias)
			}
			if reg.RepoHasWorktrees(repo.Name) && !force {
				return fmt.Errorf("repo %q has worktrees; remove them or pass --force", repo.Aliases[0])
			}
			if err := os.RemoveAll(repo.BarePath); err != nil {
				return fmt.Errorf("rm %s: %w", repo.BarePath, err)
			}
			name := repo.Name
			fmt.Printf("Removed repo %s (%s)\n", repo.Aliases[0], name)
			reg.RemoveRepo(name)
			return reg.Save()
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove even if worktrees exist (registry only — does NOT touch worktree containers)")
	return cmd
}

// wrapCloneErr decorates a clone failure with a Lima-aware hint when the
// failure pattern (publickey rejection from inside the VM) matches the most
// common SSH-agent forwarding gap on macOS hosts.
func wrapCloneErr(err error) error {
	if err == nil {
		return nil
	}
	if !lima.IsGuest() || !strings.Contains(err.Error(), "Permission denied (publickey)") {
		return err
	}
	return fmt.Errorf("%w\nhint: ssh agent forwarding from your Mac into the VM may be empty.\n      run `ahjo doctor` for diagnostics, or see CONTAINER-ISOLATION.md", err)
}
