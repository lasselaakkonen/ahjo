package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/ahjoconfig"
	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/coi"
	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/idmap"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	sshpkg "github.com/lasselaakkonen/ahjo/internal/ssh"
)

func newNewCmd() *cobra.Command {
	var base string
	var noFetch bool
	var asAlias string
	cmd := &cobra.Command{
		Use:   "new <repo-alias> <branch>",
		Short: "Create a worktree + .coi/config.toml for (repo, branch). Does not start the container.",
		Long: `Create a new worktree. The auto alias is "<repo-primary-alias>@<branch>".
Pass --as <alias> to register an additional alias for the worktree.`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return runNew(args[0], args[1], base, asAlias, noFetch)
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "branch/ref to create from (default: repo's default-base)")
	cmd.Flags().BoolVar(&noFetch, "no-fetch", false, "skip `git fetch origin` on the bare clone")
	cmd.Flags().StringVar(&asAlias, "as", "", "additional alias for this worktree (must not collide with any existing alias)")
	return cmd
}

func runNew(repoAlias, branch, base, asAlias string, noFetch bool) error {
	// EnsureRepo runs without holding the lockfile — runRepoAdd acquires
	// the lock itself for the registration phase, then releases before
	// recursing into runNew for the default-branch worktree. Calling it
	// here lets `ahjo new <new-repo> <branch>` auto-clone from GitHub
	// before we proceed with worktree creation.
	if _, err := EnsureRepo(repoAlias); err != nil {
		return err
	}

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
	repo := reg.FindRepoByAlias(repoAlias)
	if repo == nil {
		return fmt.Errorf("no repo with alias %q (try `ahjo repo add` or `ahjo repo ls`)", repoAlias)
	}
	if existing := reg.FindWorktree(repo.Name, branch); existing != nil {
		// Idempotent: re-render config.toml + ssh-config and exit OK.
		if err := rerender(cfg, reg, existing, repo); err != nil {
			return err
		}
		fmt.Printf("worktree already exists at %s; re-rendered .coi/config.toml\n", existing.WorktreePath)
		return nil
	}

	primary := registry.MakeWorktreeAlias(repo.Aliases[0], branch)
	if reg.AliasInUse(primary) {
		return fmt.Errorf("auto alias %q is already taken by another repo or worktree; remove the conflicting one or pick a different branch name", primary)
	}
	aliases := []string{primary}
	if asAlias != "" {
		if err := registry.ValidateAlias(asAlias); err != nil {
			return err
		}
		if asAlias != primary {
			if reg.AliasInUse(asAlias) {
				return fmt.Errorf("alias %q already in use; pick another --as value", asAlias)
			}
			aliases = append(aliases, asAlias)
		}
	}

	if !noFetch {
		if err := git.Fetch(repo.BarePath); err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
	}

	slug := reg.MakeSlug(repo.Name, branch)
	worktreePath := paths.WorktreePath(repo.Name, branch)
	hostKeysDir := paths.SlugHostKeysDir(slug)

	from, err := resolveBase(repo, base)
	if err != nil {
		return err
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

	// Merge any .ahjoconfig forward_env into the template data.
	extraForwardEnv := []string(nil)
	if ahjoConf, found, err := ahjoconfig.Load(worktreePath); err != nil {
		return fmt.Errorf(".ahjoconfig: %w", err)
	} else if found {
		extraForwardEnv = ahjoConf.ForwardEnv
	}

	if err := coi.RenderConfig(worktreePath, coi.TemplateData{
		Image:       paths.AhjoBaseProfile,
		Slug:        slug,
		HostKeysDir: hostKeysDir,
		ForwardEnv:  append(cfg.ForwardEnv, extraForwardEnv...),
	}); err != nil {
		return err
	}

	w := registry.Worktree{
		Repo:           repo.Name,
		Aliases:        aliases,
		Branch:         branch,
		Slug:           slug,
		WorktreePath:   worktreePath,
		ContainerAlias: slug,
		SSHPort:        port,
		SSHHostKeysDir: hostKeysDir,
		CreatedAt:      time.Now().UTC(),
	}

	// COW copy: clone the default-branch container instead of starting fresh.
	if repo.BaseContainerName != "" {
		cowName := "ahjo-" + slug
		baseWorktreePath := paths.WorktreePath(repo.Name, repo.DefaultBase)
		if err := incus.CopyContainer(repo.BaseContainerName, cowName); err != nil {
			return fmt.Errorf("incus copy base container: %w", err)
		}
		// Rebase workspace-relative disk device sources (workspace, protect-*).
		if err := incus.UpdateWorktreeMounts(cowName, baseWorktreePath, worktreePath); err != nil {
			return fmt.Errorf("rebase worktree mounts in COW container: %w", err)
		}
		// SSH host key mounts (mount-0, mount-1) are keyed by slug, not worktree
		// path, so UpdateWorktreeMounts leaves them untouched. Update explicitly.
		if err := incus.ConfigDeviceSet(cowName, "mount-0", "source", hostKeysDir); err != nil {
			return fmt.Errorf("update host-keys mount in COW container: %w", err)
		}
		if err := incus.ConfigDeviceSet(cowName, "mount-1", "source", hostKeysDir+"/authorized_keys"); err != nil {
			return fmt.Errorf("update authorized_keys mount in COW container: %w", err)
		}
		// Remove the stale ahjo-ssh proxy (source container's port); shell.go
		// re-adds it with the correct port on first attach.
		_ = incus.RemoveDevice(cowName, "ahjo-ssh")
		// Apply raw.idmap so the rebased workspace bind mount surfaces inside
		// the container as code:code. The COW copy is stopped (incus copy
		// --stateless), so a plain ConfigSet is enough; the first start later
		// in prepareWorktreeContainer picks the mapping up.
		// See CONTAINER-ISOLATION.md "Workspace UID mapping".
		if err := incus.ConfigSet(cowName, "raw.idmap", idmap.RawIdmapValue(os.Getuid(), os.Getgid())); err != nil {
			return fmt.Errorf("set raw.idmap on COW container: %w", err)
		}
		w.IncusName = cowName
	}

	reg.Worktrees = append(reg.Worktrees, w)
	if err := reg.Save(); err != nil {
		return err
	}
	if err := sshpkg.RegenerateConfig(reg); err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "ssh port %d; run: ahjo shell %s\n", port, primary)
	return nil
}

// EnsureWorktree returns the worktree registered under worktreeAlias.
// If the worktree isn't registered and the alias has the canonical
// "<repo-alias>@<branch>" shape, it auto-adds the parent repo (via
// EnsureRepo) and runs runNew to create the worktree. Idempotent for
// already-registered worktrees.
func EnsureWorktree(worktreeAlias string) (*registry.Worktree, error) {
	reg, err := registry.Load()
	if err != nil {
		return nil, err
	}
	if w := reg.FindWorktreeByAlias(worktreeAlias); w != nil {
		return w, nil
	}

	repoAlias, branch, ok := splitWorktreeAlias(worktreeAlias)
	if !ok {
		return nil, fmt.Errorf("no worktree with alias %q; create with `ahjo new`", worktreeAlias)
	}

	if _, err := EnsureRepo(repoAlias); err != nil {
		return nil, err
	}

	// runNew is idempotent for an existing worktree (re-renders config);
	// for a missing one it creates the branch + container scaffolding.
	if err := runNew(repoAlias, branch, "", "", false); err != nil {
		return nil, err
	}

	reg, err = registry.Load()
	if err != nil {
		return nil, err
	}
	if w := reg.FindWorktreeByAlias(worktreeAlias); w != nil {
		return w, nil
	}
	if w := reg.FindWorktreeByAlias(strings.ToLower(worktreeAlias)); w != nil {
		return w, nil
	}
	return nil, fmt.Errorf("internal: just-created worktree %q not in registry", worktreeAlias)
}

// splitWorktreeAlias parses "<repo-alias>@<branch>" — exactly one `@`,
// repo-alias must satisfy splitRepoAlias's shape, branch must be non-empty.
// Branch may contain slashes (e.g. "feature/x"); repo-alias may not.
func splitWorktreeAlias(alias string) (repoAlias, branch string, ok bool) {
	at := strings.Index(alias, "@")
	if at < 0 || strings.LastIndex(alias, "@") != at {
		return "", "", false
	}
	repoAlias = alias[:at]
	branch = alias[at+1:]
	if branch == "" {
		return "", "", false
	}
	if _, _, ok := splitRepoAlias(repoAlias); !ok {
		return "", "", false
	}
	return repoAlias, branch, true
}

// resolveBase picks the ref to base a new worktree on. Order: explicit
// --base, then the repo's stored default, then a live lookup of the bare
// repo's HEAD. The last step also kicks in when the stored default is stale
// (e.g. registry says "main" but the actual default is "master") so the user
// doesn't have to re-add the repo.
func resolveBase(repo *registry.Repo, baseFlag string) (string, error) {
	if baseFlag != "" {
		return baseFlag, nil
	}
	if repo.DefaultBase != "" && git.RefExists(repo.BarePath, repo.DefaultBase) {
		return repo.DefaultBase, nil
	}
	detected, err := git.DefaultBranch(repo.BarePath)
	if err != nil {
		return "", fmt.Errorf("detect default branch (pass --base to override): %w", err)
	}
	if repo.DefaultBase != "" && repo.DefaultBase != detected {
		fmt.Fprintf(os.Stderr, "warning: registry default-base %q not found in %s; using detected %q. "+
			"Edit ~/.ahjo/registry.toml to silence this.\n", repo.DefaultBase, repo.Name, detected)
	}
	return detected, nil
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

	extraForwardEnv := []string(nil)
	if ahjoConf, found, _ := ahjoconfig.Load(w.WorktreePath); found {
		extraForwardEnv = ahjoConf.ForwardEnv
	}

	if err := coi.RenderConfig(w.WorktreePath, coi.TemplateData{
		Image:       paths.AhjoBaseProfile,
		Slug:        w.Slug,
		HostKeysDir: w.SSHHostKeysDir,
		ForwardEnv:  append(cfg.ForwardEnv, extraForwardEnv...),
	}); err != nil {
		return err
	}
	return sshpkg.RegenerateConfig(reg)
}
