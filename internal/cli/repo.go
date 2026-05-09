package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/ahjoconfig"
	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lima"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	sshpkg "github.com/lasselaakkonen/ahjo/internal/ssh"
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
		Short: "Register a repo: clone it inside a fresh ahjo-base container at /repo and warm-install dependencies",
		Long: `Add a repo. The auto alias is derived from the URL as <owner>/<repo>.
Pass --as <alias> to register an additional alias for the same repo.
On auto-alias collision (e.g. github.com/acme/api vs gitlab.com/acme/api),
ahjo appends -2/-3/... to keep aliases unique.

The repo's default-branch container becomes the COW source from which every
subsequent ` + "`ahjo new`" + ` clones — its node_modules and pnpm store survive
into branch containers via btrfs reflinks, eliminating the cold-install tax.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRepoAdd(args[0], asAlias, defaultBase)
		},
	}
	cmd.Flags().StringVar(&defaultBase, "default-base", "", "default branch to base new branches on (default: detect from the remote's HEAD)")
	cmd.Flags().StringVar(&asAlias, "as", "", "additional alias for this repo (must not collide with any existing alias)")
	return cmd
}

func runRepoAdd(input, asAlias, defaultBase string) error {
	url := resolveRepoURL(input)
	slug, primary, aliases, err := repoAddPlan(url, asAlias)
	if err != nil {
		return err
	}
	return repoAddSetup(slug, primary, aliases, url, defaultBase)
}

// resolveRepoURL accepts either a git URL or a bare "<owner>/<repo>" alias
// and returns a usable URL. Aliases get the same SSH-then-HTTPS GitHub
// inference EnsureRepo uses, so `ahjo repo add lasselaakkonen/foo` works
// the same way `ahjo new lasselaakkonen/foo bar` does.
func resolveRepoURL(input string) string {
	if strings.Contains(input, "://") || strings.Contains(input, "@") {
		return input
	}
	if owner, name, ok := splitRepoAlias(input); ok {
		return pickGitHubURL(owner, name)
	}
	return input
}

// repoAddPlan validates the alias/slug allocation under the lockfile but
// does NOT yet write the registry. The actual repo + branch rows are
// inserted at the end of repoAddSetup so a mid-flow failure leaves no
// half-state behind.
func repoAddPlan(url, asAlias string) (slug, primary string, aliases []string, err error) {
	release, err := lockfile.Acquire()
	if err != nil {
		return "", "", nil, err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return "", "", nil, err
	}

	primary, err = reg.AllocateRepoAlias(url)
	if err != nil {
		return "", "", nil, err
	}
	aliases = []string{primary}
	if asAlias != "" {
		if err := registry.ValidateAlias(asAlias); err != nil {
			return "", "", nil, err
		}
		if asAlias != primary {
			if reg.AliasInUse(asAlias) {
				return "", "", nil, fmt.Errorf("alias %q already in use; pick another --as value", asAlias)
			}
			aliases = append(aliases, asAlias)
		}
	}
	slug = reg.AllocateRepoSlug(primary)
	return slug, primary, aliases, nil
}

// repoAddSetup creates the default-branch container, clones the repo at /repo,
// runs warm-install, .ahjoconfig run commands, then stops the container so
// `incus copy` can COW it cheaply for branch creation. Inserts the repo +
// default-branch rows into the registry only on success — a mid-flow
// failure leaves no half-state.
//
// Lockfile is acquired only for the final registry write; the long
// container/network operations run unlocked so concurrent ahjo invocations
// (e.g. `ahjo top` refresh) aren't starved.
func repoAddSetup(slug, primary string, aliases []string, url, defaultBase string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Printf("Adding repo %s (aliases: %s)\n", slug, strings.Join(aliases, ", "))

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

	hostKeysDir := paths.SlugHostKeysDir(slug)
	if err := sshpkg.EnsureHostKeys(hostKeysDir); err != nil {
		return err
	}
	if err := sshpkg.WriteAuthorizedKeys(hostKeysDir); err != nil {
		return err
	}
	if err := sshpkg.WriteKnownHosts(hostKeysDir, port); err != nil {
		return err
	}

	containerName := "ahjo-" + slug
	fmt.Printf("→ incus init ahjo-base %s\n", containerName)
	if err := incus.LaunchStopped(paths.AhjoBaseProfile, containerName); err != nil {
		return err
	}
	if err := wireBranchContainer(containerName, hostKeysDir); err != nil {
		return err
	}

	fmt.Printf("→ incus start %s\n", containerName)
	if err := incus.Start(containerName); err != nil {
		return err
	}
	if err := incus.WaitReady(containerName, 30*time.Second); err != nil {
		return err
	}
	// ssh-agent proxy must be attached post-start.
	if err := attachSSHAgent(containerName); err != nil {
		return fmt.Errorf("attach ssh-agent: %w", err)
	}

	if err := pushClaudeConfig(containerName); err != nil {
		return fmt.Errorf("push claude config: %w", err)
	}
	if err := incus.ExecAs(containerName, 1000, nil, "/", "/usr/local/bin/ahjo-claude-prepare"); err != nil {
		return fmt.Errorf("ahjo-claude-prepare: %w", err)
	}

	// /repo is at the container root, where uid 1000 can't `mkdir`. Create
	// it as code:code first so `git clone` runs unprivileged.
	if err := incus.ExecAs(containerName, 0, nil, "/", "install", "-d", "-m", "0755", "-o", "code", "-g", "code", paths.RepoMountPath); err != nil {
		return fmt.Errorf("create %s: %w", paths.RepoMountPath, err)
	}
	fmt.Printf("→ git clone %s %s (in container as code)\n", url, paths.RepoMountPath)
	if err := incus.ExecAs(containerName, 1000, nil, "/", "git", "clone", url, paths.RepoMountPath); err != nil {
		return wrapCloneErr(err)
	}

	if defaultBase == "" {
		defaultBase, err = detectContainerDefaultBranch(containerName)
		if err != nil {
			return fmt.Errorf("detect default branch (pass --default-base to override): %w", err)
		}
	}

	// Resolve the host-env keys to forward into warm-install (and any
	// .ahjoconfig run commands). The clone has just landed, so
	// LoadFromContainer can pick up /repo/.ahjoconfig if present.
	envKeys := append([]string(nil), cfg.ForwardEnv...)
	var ahjoConf *ahjoconfig.Config
	if rcfg, found, err := ahjoconfig.LoadFromContainer(containerName); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: read /repo/.ahjoconfig: %v\n", err)
	} else if found {
		ahjoConf = rcfg
		envKeys = append(envKeys, rcfg.ForwardEnv...)
	}
	hostEnv := resolveHostEnv(envKeys)

	if err := runWarmInstall(containerName, hostEnv); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: warm install: %v\n", err)
	}

	if ahjoConf != nil {
		for _, cmd := range ahjoConf.Run {
			fmt.Printf("→ %s\n", cmd)
			if err := incus.ExecAs(containerName, 1000, hostEnv, paths.RepoMountPath, "bash", "-c", cmd); err != nil {
				return fmt.Errorf(".ahjoconfig run %q: %w", cmd, err)
			}
		}
	}

	// Wire SSH proxy + sshd before stopping the container so subsequent
	// `incus copy` clones inherit the device set.
	if err := incus.AddProxyDevice(
		containerName, "ahjo-ssh",
		fmt.Sprintf("tcp:127.0.0.1:%d", port),
		"tcp:127.0.0.1:22",
	); err != nil {
		return err
	}
	if _, err := incus.Exec(containerName, "systemctl", "start", "ssh"); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: could not start sshd: %v\n", err)
	}

	// Stop so future `incus copy` is fast (btrfs CoW requires stopped source).
	fmt.Printf("→ incus stop %s\n", containerName)
	if err := incus.Stop(containerName); err != nil {
		return err
	}

	// Acquire lock for final registry write — both repo + default branch
	// rows go in atomically so a partial state is impossible.
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return err
	}
	// Re-validate the slug + aliases under the lock in case a parallel
	// ahjo invocation grabbed them while setup was running.
	if reg.FindRepo(slug) != nil {
		return fmt.Errorf("repo slug %q taken by a parallel run; container %s is orphaned and can be removed with `incus delete --force %s`", slug, containerName, containerName)
	}
	for _, a := range aliases {
		if reg.AliasInUse(a) {
			return fmt.Errorf("alias %q taken by a parallel run; container %s is orphaned", a, containerName)
		}
	}

	branchAlias := registry.MakeBranchAlias(primary, defaultBase)
	branchSlug := reg.MakeSlug(slug, defaultBase)
	reg.Repos = append(reg.Repos, registry.Repo{
		Name:              slug,
		Aliases:           aliases,
		Remote:            url,
		DefaultBase:       defaultBase,
		BaseContainerName: containerName,
	})
	reg.Branches = append(reg.Branches, registry.Branch{
		Repo:           slug,
		Aliases:        []string{branchAlias},
		Branch:         defaultBase,
		Slug:           branchSlug,
		ContainerAlias: branchSlug,
		SSHPort:        port,
		IncusName:      containerName,
		IsDefault:      true,
		CreatedAt:      time.Now().UTC(),
	})
	if err := reg.Save(); err != nil {
		return err
	}
	if err := sshpkg.RegenerateConfig(reg); err != nil {
		return err
	}
	fmt.Printf("Repo %s ready (default: %s, ssh port %d). Try: ahjo shell %s\n",
		slug, defaultBase, port, branchAlias)
	return nil
}

// wireBranchContainer applies the per-container config + devices that COI
// used to set up via its [mounts.default] block, runtime security flags,
// and ssh-agent proxy. Runs while the container is still in `incus
// init`-stopped state so first start honors raw.idmap and the security
// keys.
//
// `incus copy` propagates instance config + device definitions to branch
// containers, so flags applied here on the default container are inherited
// by every COW branch. raw.idmap and the ssh-agent socket path are the
// exceptions — both must be reapplied after copy (see new.go cloneFromBase).
func wireBranchContainer(containerName, hostKeysDir string) error {
	for _, kv := range securityConfigFlags() {
		if err := incus.ConfigSet(containerName, kv[0], kv[1]); err != nil {
			return fmt.Errorf("set %s: %w", kv[0], err)
		}
	}
	if err := incus.AddDiskDevice(
		containerName, "ahjo-host-keys",
		hostKeysDir, "/etc/ssh/ahjo-host-keys",
		true,
	); err != nil {
		return err
	}
	if err := incus.AddDiskDevice(
		containerName, "ahjo-authorized-keys",
		hostKeysDir+"/authorized_keys", "/home/code/.ssh/authorized_keys",
		true,
	); err != nil {
		return err
	}
	// SSH_AUTH_SOCK env can be set on a stopped container; the listen
	// socket itself can only be created post-start (see attachSSHAgent).
	if os.Getenv("SSH_AUTH_SOCK") != "" {
		if err := incus.ConfigSet(containerName, "environment.SSH_AUTH_SOCK", "/tmp/ssh-agent.sock"); err != nil {
			return err
		}
	}
	return applyRawIdmap(containerName)
}

// attachSSHAgent (re)wires the ssh-agent proxy device pointing at the
// host's current SSH_AUTH_SOCK. Must run while the container is RUNNING:
// `bind=container` proxy devices need a live container namespace to create
// the listen socket. No-op when the host has no SSH_AUTH_SOCK.
func attachSSHAgent(containerName string) error {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	return incus.EnsureSSHAgentProxy(containerName, sock)
}

// securityConfigFlags are the per-container Incus config keys ahjo applies
// so containers have the same posture COI wired at runtime in v0.8.x:
// nesting (for docker-in-container), mknod/setxattr syscall intercepts (so
// pnpm/npm postinstall scripts that touch xattrs work), unprivileged-port
// binding (so a dev server on :80 works without sudo), and disabling the
// guest-API mount (which exposes the host's incus socket inside).
//
// `incus copy` carries these keys to branch containers, so the default
// container's wireBranchContainer call covers the whole repo.
func securityConfigFlags() [][2]string {
	return [][2]string{
		{"security.nesting", "true"},
		{"security.syscalls.intercept.mknod", "true"},
		{"security.syscalls.intercept.setxattr", "true"},
		{"linux.sysctl.net.ipv4.ip_unprivileged_port_start", "0"},
		{"security.guestapi", "false"},
	}
}

// detectContainerDefaultBranch runs `git symbolic-ref --short HEAD` inside
// containerName's /repo so the caller doesn't need a host-side bare clone.
// Runs as the `code` user (uid 1000) — `/repo` is owned by code, and git
// refuses to run on a tree it considers "dubiously owned" when invoked
// from another uid.
func detectContainerDefaultBranch(containerName string) (string, error) {
	out, err := exec.Command(
		"incus", "exec", containerName, "--user", "1000", "--cwd", paths.RepoMountPath,
		"--", "git", "symbolic-ref", "--short", "HEAD",
	).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Stderr.Write(ee.Stderr)
		}
		return "", fmt.Errorf("incus exec %s: %w", containerName, err)
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", fmt.Errorf("git symbolic-ref HEAD in %s returned empty", containerName)
	}
	return name, nil
}

// runWarmInstall detects per-language lockfiles in /repo and runs the
// matching installer inside the container so branch containers (cloned via
// `incus copy` with btrfs/zfs reflinks) inherit a hot dependency cache.
// hostEnv is forwarded into each installer (NPM_TOKEN etc.).
func runWarmInstall(containerName string, hostEnv map[string]string) error {
	type installer struct {
		lockfile string
		cmd      []string
	}
	candidates := []installer{
		{"pnpm-lock.yaml", []string{"pnpm", "install", "--frozen-lockfile"}},
		{"package-lock.json", []string{"npm", "ci"}},
		{"bun.lockb", []string{"bun", "install", "--frozen-lockfile"}},
		{"uv.lock", []string{"uv", "sync", "--frozen"}},
		{"Cargo.lock", []string{"cargo", "fetch"}},
	}
	any := false
	for _, c := range candidates {
		if _, err := incus.Exec(containerName, "test", "-f", paths.RepoMountPath+"/"+c.lockfile); err != nil {
			continue
		}
		any = true
		fmt.Printf("→ %s (lockfile %s detected)\n", strings.Join(c.cmd, " "), c.lockfile)
		if err := incus.ExecAs(containerName, 1000, hostEnv, paths.RepoMountPath, c.cmd...); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(c.cmd, " "), err)
		}
	}
	if !any {
		fmt.Println("→ no lockfile detected; skipping warm install")
	}
	return nil
}

// resolveHostEnv looks each key up in the host environment and returns the
// keys that have a value set. Empty / missing keys are dropped — passing
// `--env KEY=` to incus exec would clobber an inherited value with empty.
func resolveHostEnv(keys []string) map[string]string {
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		if v, ok := os.LookupEnv(k); ok {
			out[k] = v
		}
	}
	return out
}

// pushClaudeConfig copies the host's ~/.claude/* and ~/.claude.json into
// containerName at the corresponding paths under /home/code, then chowns
// the files that actually pushed to uid 1000. Replaces COI's setupCLIConfig
// pipeline with `incus file push` calls. Files missing on the host silently
// no-op (the chown step skips them so it doesn't error on partial coverage).
func pushClaudeConfig(containerName string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := incus.ExecAs(containerName, 0, nil, "/", "install", "-d", "-m", "0755", "-o", "code", "-g", "code", "/home/code/.claude"); err != nil {
		return fmt.Errorf("mkdir /home/code/.claude: %w", err)
	}
	files := []struct{ src, dst string }{
		{home + "/.claude/settings.json", "/home/code/.claude/settings.json"},
		{home + "/.claude/.credentials.json", "/home/code/.claude/.credentials.json"},
		{home + "/.claude/CLAUDE.md", "/home/code/.claude/CLAUDE.md"},
		{home + "/.claude.json", "/home/code/.claude.json"},
	}
	var pushed []string
	for _, f := range files {
		ok, err := incus.FilePush(containerName, f.src, f.dst)
		if err != nil {
			return fmt.Errorf("push %s: %w", f.src, err)
		}
		if ok {
			pushed = append(pushed, f.dst)
		}
	}
	if len(pushed) > 0 {
		args := append([]string{"chown", "1000:1000"}, pushed...)
		if err := incus.ExecAs(containerName, 0, nil, "/", args...); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: chown claude config: %v\n", err)
		}
	}
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
		Short: "Remove a repo by any of its aliases (refuses if any branches exist; --force overrides)",
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
			if reg.RepoHasBranches(repo.Name) && !force {
				return fmt.Errorf("repo %q has branches; remove them or pass --force", repo.Aliases[0])
			}
			name := repo.Name
			fmt.Printf("Removed repo %s (%s) from registry (containers were not touched; use `ahjo rm` per-branch)\n", repo.Aliases[0], name)
			reg.RemoveRepo(name)
			return reg.Save()
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove even if branches exist (registry only — does NOT touch branch containers)")
	return cmd
}

// EnsureRepo returns the repo registered under repoAlias. If the repo
// isn't registered and the alias has the canonical "<owner>/<repo>"
// shape, it auto-adds the repo by deriving a GitHub URL (SSH if
// reachable, else HTTPS) and running the standard `repo add` flow.
// Idempotent: a second call on a registered repo just returns it.
func EnsureRepo(repoAlias string) (*registry.Repo, error) {
	reg, err := registry.Load()
	if err != nil {
		return nil, err
	}
	if r := reg.FindRepoByAlias(repoAlias); r != nil {
		return r, nil
	}

	owner, name, ok := splitRepoAlias(repoAlias)
	if !ok {
		return nil, fmt.Errorf("no repo with alias %q (try `ahjo repo add` or `ahjo repo ls`)", repoAlias)
	}

	url := pickGitHubURL(owner, name)
	fmt.Printf("repo %q not registered; adding from %s...\n", repoAlias, url)
	if err := runRepoAdd(url, "", ""); err != nil {
		return nil, err
	}

	reg, err = registry.Load()
	if err != nil {
		return nil, err
	}
	if r := reg.FindRepoByAlias(repoAlias); r != nil {
		return r, nil
	}
	if r := reg.FindRepoByAlias(strings.ToLower(repoAlias)); r != nil {
		return r, nil
	}
	return nil, fmt.Errorf("internal: just-added repo %q not in registry", repoAlias)
}

// splitRepoAlias parses "<owner>/<repo>" — exactly two non-empty
// slash-separated segments, no `@`. Branch aliases (which contain `@`)
// and arbitrary user-provided aliases are rejected so we don't try to
// GitHub-clone them.
func splitRepoAlias(alias string) (owner, repo string, ok bool) {
	if strings.Contains(alias, "@") {
		return "", "", false
	}
	parts := strings.Split(alias, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func pickGitHubURL(owner, name string) string {
	sshURL := fmt.Sprintf("git@github.com:%s/%s.git", owner, name)
	if probeSSHReachable(sshURL) {
		return sshURL
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, name)
}

func probeSSHReachable(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--exit-code", url, "HEAD")
	cmd.Env = append(os.Environ(),
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=3 -o StrictHostKeyChecking=accept-new",
		"GIT_TERMINAL_PROMPT=0",
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// wrapCloneErr decorates an in-container clone failure with a Lima-aware hint
// when the failure pattern (publickey rejection) matches the most common
// SSH-agent forwarding gap on macOS hosts.
func wrapCloneErr(err error) error {
	if err == nil {
		return nil
	}
	if !lima.IsGuest() || !strings.Contains(err.Error(), "Permission denied (publickey)") {
		return err
	}
	return fmt.Errorf("%w\nhint: ssh agent forwarding from your Mac into the VM may be empty.\n      run `ahjo doctor` for diagnostics, or see CONTAINER-ISOLATION.md", err)
}
