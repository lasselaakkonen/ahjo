package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	sshpkg "github.com/lasselaakkonen/ahjo/internal/ssh"
)

func newCreateCmd() *cobra.Command {
	var base string
	var noFetch bool
	var asAlias string
	cmd := &cobra.Command{
		Use:   "create <repo-alias> <branch>",
		Short: "Create a branch container by COW-cloning the repo's default-branch container and `git checkout -b <branch>` inside it",
		Long: `Create a new branch container. The auto alias is "<repo-primary-alias>@<branch>".
Pass --as <alias> to register an additional alias for the branch.

<branch> accepts any string — spaces and shell-unfriendly characters are
sanitized into a git+GitHub-safe name (e.g. "JIRA-123 ticket title" becomes
"JIRA-123-ticket-title"). Quote the argument when it contains spaces.

The new container is an ` + "`incus copy`" + ` of the repo's default-branch
container — on btrfs that's a near-free reflink that inherits node_modules,
the pnpm store, and any other warm dependencies. ` + "`git checkout -b <branch>`" + `
runs inside the clone after copy completes.`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return runCreate(args[0], args[1], base, asAlias, noFetch)
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "branch/ref to create from (default: repo's default-base)")
	cmd.Flags().BoolVar(&noFetch, "no-fetch", false, "skip `git fetch origin` inside the new container")
	cmd.Flags().StringVar(&asAlias, "as", "", "additional alias for this branch (must not collide with any existing alias)")
	return cmd
}

func runCreate(repoAlias, branch, base, asAlias string, noFetch bool) error {
	raw := branch
	branch = git.SanitizeBranchName(branch)
	if branch == "" {
		return fmt.Errorf("branch name %q has no characters usable in a git ref", raw)
	}
	if branch != raw {
		fmt.Printf("branch %q → %q\n", raw, branch)
	}

	// EnsureRepo runs without holding the lockfile — runRepoAdd acquires
	// the lock itself for the registration phase, then releases before
	// recursing into the warm-install phase. Calling it here lets
	// `ahjo create <new-repo> <branch>` auto-register from GitHub before we
	// proceed.
	if _, err := EnsureRepo(repoAlias); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Phase 1: short-lived lock to allocate slug + port + reserve registry
	// row, then drop it so the long `incus copy` + checkout can run unlocked.
	br, repo, err := createReserveBranch(cfg, repoAlias, branch, asAlias)
	if err != nil {
		return err
	}
	if br == nil {
		// Idempotent return: existing branch — re-render ssh config and exit.
		return nil
	}

	// Phase 2: COW + checkout, no lock held.
	containerName := br.IncusName
	if err := cloneFromBase(repo, br); err != nil {
		return err
	}

	fmt.Printf("→ starting container\n")
	if err := incus.Start(containerName); err != nil {
		return err
	}
	if err := incus.WaitReady(containerName, 30*time.Second); err != nil {
		return err
	}
	if err := attachSSHAgent(containerName); err != nil {
		return fmt.Errorf("attach ssh-agent: %w", err)
	}

	if !noFetch {
		fmt.Printf("→ git fetch origin (in container)\n")
		if err := incus.ExecAs(containerName, 1000, nil, paths.RepoMountPath, "git", "fetch", "origin"); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: git fetch: %v\n", err)
		}
	}

	// Default-base resolves to origin/<DefaultBase> (fresh after the fetch
	// above), not local <DefaultBase> which drifts. Explicit --base is used
	// verbatim — may be any revspec (tag, SHA), not just a branch.
	ref := base
	if ref == "" {
		ref = "origin/" + repo.DefaultBase
	}
	fmt.Printf("→ git checkout -B %s %s (in container)\n", branch, ref)
	if err := incus.ExecAs(containerName, 1000, nil, paths.RepoMountPath, "git", "checkout", "-B", branch, ref); err != nil {
		return fmt.Errorf("git checkout: %w", err)
	}

	if err := incus.AddProxyDevice(
		containerName, "ahjo-ssh",
		fmt.Sprintf("tcp:127.0.0.1:%d", br.SSHPort),
		"tcp:127.0.0.1:22",
	); err != nil {
		return err
	}
	if _, err := incus.Exec(containerName, "systemctl", "start", "ssh"); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: could not start sshd: %v\n", err)
	}

	alias := br.Aliases[0]
	fmt.Fprintf(os.Stdout, "\nahjo shell %s\nahjo claude %s\n", alias, alias)
	return nil
}

// createReserveBranch holds the lockfile for just the allocation phase: it
// validates the alias, picks a slug + container name + ssh port, ensures
// host keys, and inserts the registry row. Returns (nil, ...) when the
// branch already exists (idempotent path; ssh-config is re-rendered).
func createReserveBranch(cfg *config.Config, repoAlias, branch, asAlias string) (*registry.Branch, *registry.Repo, error) {
	release, err := lockfile.Acquire()
	if err != nil {
		return nil, nil, err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return nil, nil, err
	}
	repo := reg.FindRepoByAlias(repoAlias)
	if repo == nil {
		return nil, nil, fmt.Errorf("no repo with alias %q (try `ahjo repo add` or `ahjo repo ls`)", repoAlias)
	}
	if repo.BaseContainerName == "" {
		return nil, nil, fmt.Errorf("repo %q has no base container; re-add it with `ahjo repo add`", repo.Aliases[0])
	}
	if existing := reg.FindBranch(repo.Name, branch); existing != nil {
		// Idempotent: re-render ssh-config + exit OK.
		if err := sshpkg.RegenerateConfig(reg); err != nil {
			return nil, nil, err
		}
		fmt.Printf("branch %s already exists (container %s)\n", existing.Aliases[0], existing.IncusName)
		return nil, repo, nil
	}

	primary := registry.MakeBranchAlias(repo.Aliases[0], branch)
	if reg.AliasInUse(primary) {
		return nil, nil, fmt.Errorf("auto alias %q is already taken by another repo or branch", primary)
	}
	aliases := []string{primary}
	if asAlias != "" {
		if err := registry.ValidateAlias(asAlias); err != nil {
			return nil, nil, err
		}
		if asAlias != primary {
			if reg.AliasInUse(asAlias) {
				return nil, nil, fmt.Errorf("alias %q already in use; pick another --as value", asAlias)
			}
			aliases = append(aliases, asAlias)
		}
	}

	fmt.Printf("Creating container %s\n", primary)

	slug := reg.MakeSlug(repo.Name, branch)
	hostKeysDir := paths.SlugHostKeysDir(slug)

	// See internal/ssh/keygen.go: no-op on Mac/Lima, mints a layer-local
	// key when running inside an Incus container (ahjo-in-ahjo) so
	// WriteAuthorizedKeys always has a pubkey source.
	if _, created, err := sshpkg.EnsureLocalKey(); err != nil {
		return nil, nil, fmt.Errorf("ensure local ssh key: %w", err)
	} else if created {
		fmt.Println("  → generated ~/.ssh/id_ed25519 (no prior client key found)")
	}

	if err := sshpkg.EnsureHostKeys(hostKeysDir); err != nil {
		return nil, nil, err
	}
	if err := sshpkg.WriteAuthorizedKeys(hostKeysDir); err != nil {
		return nil, nil, err
	}

	pp, err := ports.Load()
	if err != nil {
		return nil, nil, err
	}
	if cfg.PortRange.Min != 0 || cfg.PortRange.Max != 0 {
		pp.Range = ports.Range{Min: cfg.PortRange.Min, Max: cfg.PortRange.Max}
	}
	port, err := pp.Allocate(slug, ports.PurposeSSH)
	if err != nil {
		return nil, nil, err
	}
	if err := pp.Save(); err != nil {
		return nil, nil, err
	}
	if err := sshpkg.WriteKnownHosts(hostKeysDir, port); err != nil {
		return nil, nil, err
	}

	br := registry.Branch{
		Repo:           repo.Name,
		Aliases:        aliases,
		Branch:         branch,
		Slug:           slug,
		ContainerAlias: slug,
		SSHPort:        port,
		IncusName:      "ahjo-" + slug,
		CreatedAt:      time.Now().UTC(),
	}
	reg.Branches = append(reg.Branches, br)
	if err := reg.Save(); err != nil {
		return nil, nil, err
	}
	if err := sshpkg.RegenerateConfig(reg); err != nil {
		return nil, nil, err
	}
	return &br, repo, nil
}

// cloneFromBase COW-copies the repo's default-branch container, rewires its
// per-branch SSH-key mounts + raw.idmap, and removes the stale ahjo-ssh proxy.
// Leaves the new container stopped, ready for Start.
//
// Reused by shell.go's --update / missing-container path.
func cloneFromBase(repo *registry.Repo, br *registry.Branch) error {
	containerName := br.IncusName

	// btrfs `incus copy` requires the source stopped. Tolerant: if it's
	// already running, stop it; cli/repo.go stops it after warm install,
	// so this is usually a no-op.
	if err := incus.Stop(repo.BaseContainerName); err != nil {
		return fmt.Errorf("stop base container %s: %w", repo.BaseContainerName, err)
	}

	fmt.Printf("→ incus copy %s %s\n", repo.BaseContainerName, containerName)
	if err := incus.CopyContainer(repo.BaseContainerName, containerName); err != nil {
		return err
	}

	fmt.Printf("→ configuring container\n")
	hostKeysDir := paths.SlugHostKeysDir(br.Slug)
	if err := incus.ConfigDeviceSet(containerName, "ahjo-host-keys", "source", hostKeysDir); err != nil {
		return fmt.Errorf("rewire ahjo-host-keys mount: %w", err)
	}
	if err := incus.ConfigDeviceSet(containerName, "ahjo-authorized-keys", "source", hostKeysDir+"/authorized_keys"); err != nil {
		return fmt.Errorf("rewire ahjo-authorized-keys mount: %w", err)
	}
	if err := incus.ConfigDeviceSet(containerName, "ahjo-ancestor-pubkeys", "source", hostKeysDir+"/ancestor-pubkeys"); err != nil {
		return fmt.Errorf("rewire ahjo-ancestor-pubkeys mount: %w", err)
	}
	// The base's ahjo-ssh proxy listens on the base's port; remove so shell
	// can re-add with this branch's port. Likewise drop the inherited
	// ssh-agent proxy — it points at the (possibly stale) base-time
	// SSH_AUTH_SOCK, and bind=container proxies must be added post-start
	// anyway. attachSSHAgent re-creates it after Start.
	_ = incus.RemoveDevice(containerName, "ahjo-ssh")
	_ = incus.RemoveDevice(containerName, "ssh-agent")

	// raw.idmap doesn't survive `incus copy` — reapply.
	if err := applyRawIdmap(containerName); err != nil {
		return err
	}
	return nil
}

// EnsureBranch returns the branch registered under branchAlias.
// If the branch isn't registered and the alias has the canonical
// "<repo-alias>@<branch>" shape, it auto-adds the parent repo (via
// EnsureRepo) and runs runCreate to create the branch container. Idempotent
// for already-registered branches.
func EnsureBranch(branchAlias string) (*registry.Branch, error) {
	reg, err := registry.Load()
	if err != nil {
		return nil, err
	}
	if br := reg.FindBranchByAlias(branchAlias); br != nil {
		return br, nil
	}

	repoAlias, branch, ok := splitBranchAlias(branchAlias)
	if !ok {
		return nil, fmt.Errorf("no branch with alias %q; create with `ahjo create`", branchAlias)
	}

	if _, err := EnsureRepo(repoAlias); err != nil {
		return nil, err
	}

	// runCreate is idempotent for an existing branch (re-renders config); for
	// a missing one it creates the COW container.
	if err := runCreate(repoAlias, branch, "", "", false); err != nil {
		return nil, err
	}

	reg, err = registry.Load()
	if err != nil {
		return nil, err
	}
	if br := reg.FindBranchByAlias(branchAlias); br != nil {
		return br, nil
	}
	if br := reg.FindBranchByAlias(strings.ToLower(branchAlias)); br != nil {
		return br, nil
	}
	return nil, fmt.Errorf("internal: just-created branch %q not in registry", branchAlias)
}

// splitBranchAlias parses "<repo-alias>@<branch>" — exactly one `@`,
// repo-alias must satisfy splitRepoAlias's shape, branch must be non-empty.
// Branch may contain slashes (e.g. "feature/x"); repo-alias may not.
func splitBranchAlias(alias string) (repoAlias, branch string, ok bool) {
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
