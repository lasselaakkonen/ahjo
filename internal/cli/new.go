package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/coi"
	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	sshpkg "github.com/lasselaakkonen/ahjo/internal/ssh"
)

func newNewCmd() *cobra.Command {
	var base string
	var noFetch bool
	cmd := &cobra.Command{
		Use:   "new <repo> <branch>",
		Short: "Create a worktree + .coi/config.toml for (repo, branch). Does not start the container.",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return runNew(args[0], args[1], base, noFetch)
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "branch/ref to create from (default: repo's default-base)")
	cmd.Flags().BoolVar(&noFetch, "no-fetch", false, "skip `git fetch origin` on the bare clone")
	return cmd
}

func runNew(repoName, branch, base string, noFetch bool) error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	reg, err := registry.Load()
	if err != nil {
		return err
	}
	repo := reg.FindRepo(repoName)
	if repo == nil {
		return fmt.Errorf("repo %q not registered (try `ahjo repo add`)", repoName)
	}
	if existing := reg.FindWorktree(repoName, branch); existing != nil {
		// Idempotent: re-render config.toml + ssh-config and exit OK.
		if err := rerender(cfg, reg, existing, repo); err != nil {
			return err
		}
		fmt.Printf("worktree already exists at %s; re-rendered .coi/config.toml\n", existing.WorktreePath)
		return nil
	}

	if !noFetch {
		if err := git.Fetch(repo.BarePath); err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
	}

	slug := reg.MakeSlug(repoName, branch)
	worktreePath := paths.WorktreePath(repoName, branch)
	hostKeysDir := paths.SlugHostKeysDir(slug)

	from := base
	if from == "" {
		from = repo.DefaultBase
	}
	if from == "" {
		from = "main"
	}
	if err := git.AddWorktree(repo.BarePath, worktreePath, branch, from); err != nil {
		return fmt.Errorf("worktree add: %w", err)
	}

	if err := sshpkg.EnsureHostKeys(hostKeysDir); err != nil {
		return err
	}
	if err := sshpkg.WriteAuthorizedKeys(hostKeysDir); err != nil {
		return err
	}

	pp, err := ports.Load()
	if err != nil {
		return err
	}
	if cfg.PortRange.Min != 0 || cfg.PortRange.Max != 0 {
		pp.Range = ports.Range{Min: cfg.PortRange.Min, Max: cfg.PortRange.Max}
	}
	port, err := pp.Allocate(slug, ports.PurposeSSH)
	if err != nil {
		return err
	}
	if err := pp.Save(); err != nil {
		return err
	}

	if err := sshpkg.WriteKnownHosts(hostKeysDir, port); err != nil {
		return err
	}

	if err := coi.RenderConfig(worktreePath, coi.TemplateData{
		Image:       paths.AhjoBaseProfile,
		Slug:        slug,
		HostKeysDir: hostKeysDir,
		ForwardEnv:  cfg.ForwardEnv,
	}); err != nil {
		return err
	}

	w := registry.Worktree{
		Repo:           repoName,
		Branch:         branch,
		Slug:           slug,
		WorktreePath:   worktreePath,
		ContainerAlias: slug,
		SSHPort:        port,
		SSHHostKeysDir: hostKeysDir,
		CreatedAt:      time.Now().UTC(),
	}
	reg.Worktrees = append(reg.Worktrees, w)
	if err := reg.Save(); err != nil {
		return err
	}
	if err := sshpkg.RegenerateConfig(reg); err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "ssh port %d; run: ahjo shell %s %s\n", port, repoName, branch)
	return nil
}

// rerender updates .coi/config.toml + known_hosts + ssh-config for an
// existing worktree without touching the worktree itself, registry rows,
// or port allocations.
func rerender(cfg *config.Config, reg *registry.Registry, w *registry.Worktree, _ *registry.Repo) error {
	if err := sshpkg.EnsureHostKeys(w.SSHHostKeysDir); err != nil {
		return err
	}
	if err := sshpkg.WriteAuthorizedKeys(w.SSHHostKeysDir); err != nil {
		return err
	}
	if err := sshpkg.WriteKnownHosts(w.SSHHostKeysDir, w.SSHPort); err != nil {
		return err
	}
	if err := coi.RenderConfig(w.WorktreePath, coi.TemplateData{
		Image:       paths.AhjoBaseProfile,
		Slug:        w.Slug,
		HostKeysDir: w.SSHHostKeysDir,
		ForwardEnv:  cfg.ForwardEnv,
	}); err != nil {
		return err
	}
	return sshpkg.RegenerateConfig(reg)
}
