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

	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/devcontainer"
	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lima"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	sshpkg "github.com/lasselaakkonen/ahjo/internal/ssh"
	"github.com/lasselaakkonen/ahjo/internal/tokenstore"
)

// featureConsentForNew is the seed FeatureConsent map for a not-yet-
// registered repo. Empty: a brand-new repo has no prior trust
// decisions, so applyRepoFeatures prompts on every non-curated source.
var featureConsentForNew = map[string]bool{}

func newRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Manage the repo registry",
	}
	cmd.AddCommand(newRepoAddCmd(), newRepoLsCmd(), newRepoRmCmd(), newRepoPullCmd(), newRepoSetTokenCmd())
	return cmd
}

func newRepoAddCmd() *cobra.Command {
	var defaultBase string
	var asAlias string
	var yes bool
	cmd := &cobra.Command{
		Use:   "add <git-url>",
		Short: "Register a repo: clone it inside a fresh ahjo-base container at /repo and warm-install dependencies",
		Long: `Add a repo. The auto alias is derived from the URL as <owner>/<repo>.
Pass --as <alias> to register an additional alias for the same repo.
On auto-alias collision (e.g. github.com/acme/api vs gitlab.com/acme/api),
ahjo appends -2/-3/... to keep aliases unique.

The repo's default-branch container becomes the COW source from which every
subsequent ` + "`ahjo create`" + ` clones — its node_modules and pnpm store survive
into branch containers via btrfs reflinks, eliminating the cold-install tax.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRepoAdd(args[0], asAlias, defaultBase, yes)
		},
	}
	cmd.Flags().StringVar(&defaultBase, "default-base", "", "default branch to base new branches on (default: detect from the remote's HEAD)")
	cmd.Flags().StringVar(&asAlias, "as", "", "additional alias for this repo (must not collide with any existing alias)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the GitHub PAT prompt (the repo is added without a per-repo GH_TOKEN; set one later with `ahjo repo set-token`)")
	return cmd
}

func runRepoAdd(input, asAlias, defaultBase string, yes bool) error {
	url := resolveRepoURL(input)
	slug, primary, aliases, err := repoAddPlan(url, asAlias)
	if err != nil {
		return err
	}
	return repoAddSetup(slug, primary, aliases, url, defaultBase, yes)
}

// resolveRepoURL accepts either a git URL or a bare "<owner>/<repo>" alias
// and returns a usable URL. Aliases get the same SSH-then-HTTPS GitHub
// inference EnsureRepo uses, so `ahjo repo add lasselaakkonen/foo` works
// the same way `ahjo create lasselaakkonen/foo bar` does.
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
// runs warm-install, then the devcontainer.json lifecycle hooks (onCreate +
// postCreate), then stops the container so `incus copy` can COW it cheaply
// for branch creation. Inserts the repo + default-branch rows into the
// registry only on success — a mid-flow failure leaves no half-state.
//
// Lockfile is acquired only for the final registry write; the long
// container/network operations run unlocked so concurrent ahjo invocations
// (e.g. `ahjo top` refresh) aren't starved.
func repoAddSetup(slug, primary string, aliases []string, url, defaultBase string, yes bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Resolve a git identity for the in-container ubuntu user before any
	// container work — failing here is cheaper than discovering it after
	// `git clone` + warm-install.
	identity, err := git.ResolveHost()
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
	if err := seedGitIdentity(containerName, identity); err != nil {
		return fmt.Errorf("seed git identity: %w", err)
	}

	// /repo is at the container root, where uid 1000 can't `mkdir`. Create
	// it as ubuntu:ubuntu first so `git clone` runs unprivileged.
	if err := incus.ExecAs(containerName, 0, nil, "/", "install", "-d", "-m", "0755", "-o", "ubuntu", "-g", "ubuntu", paths.RepoMountPath); err != nil {
		return fmt.Errorf("create %s: %w", paths.RepoMountPath, err)
	}
	fmt.Printf("→ git clone %s %s (in container as ubuntu)\n", url, paths.RepoMountPath)
	if err := incus.ExecAs(containerName, 1000, nil, "/", "git", "clone", url, paths.RepoMountPath); err != nil {
		return wrapCloneErr(err)
	}

	// Per-repo GitHub token prompt. Non-fatal: skipped on --yes / non-TTY /
	// already-set / empty paste. See `ahjo repo set-token` to add later.
	if err := promptRepoGHToken(slug, primary, yes); err != nil {
		return err
	}

	if defaultBase == "" {
		defaultBase, err = detectContainerDefaultBranch(containerName)
		if err != nil {
			return fmt.Errorf("detect default branch (pass --default-base to override): %w", err)
		}
	}

	// Refuse to set up against a legacy .ahjoconfig — Phase 2 retires the
	// schema entirely. Users self-migrate per ahjo's no-runtime-migration
	// rule; the design doc explains the move.
	if has, err := devcontainer.HasLegacyAhjoconfig(containerName); err != nil {
		return fmt.Errorf("probe legacy .ahjoconfig: %w", err)
	} else if has {
		return fmt.Errorf("/repo/.ahjoconfig is no longer supported. " +
			"Migrate it to .devcontainer/devcontainer.json: " +
			"`run` → `postCreateCommand`, `forward_env` / `auto_expose` → `customizations.ahjo.*`. " +
			"See designdocs/adopt-devcontainer-spec.md")
	}

	// Parse devcontainer.json (if present). Docker-flavored fields and
	// `features:` (Phase 2b) are rejected by the parser itself, so by the
	// time we have a *Config the schema is already valid for ahjo.
	dcConf, _, err := devcontainer.LoadFromContainer(containerName)
	if err != nil {
		return err
	}
	if msg := remoteUserWarning(dcConf); msg != "" {
		fmt.Fprintln(cobraOutErr(), msg)
	}

	// Apply containerEnv via Incus's `environment.<KEY>` config keys so
	// every subsequent `incus exec` (including warm-install, lifecycle
	// hooks, and the user's shell) sees the values. Already-running
	// services aren't restarted; the spec doesn't promise that.
	if err := dcConf.ApplyContainerEnv(func(k, v string) error {
		return incus.ConfigSet(containerName, k, v)
	}); err != nil {
		return err
	}

	// Resolve the host-env keys to forward into warm-install and lifecycle
	// hooks: global config.ForwardEnv ∪ customizations.ahjo.forward_env.
	envKeys := append([]string(nil), cfg.ForwardEnv...)
	if dcConf != nil {
		envKeys = append(envKeys, dcConf.Customizations.Ahjo.ForwardEnv...)
	}
	hostEnv := resolveHostEnv(envKeys)
	// Merge containerEnv into the per-exec env too — covers both stale
	// containers and the case where Incus's environment.* propagation
	// races a freshly-issued exec.
	if dcConf != nil {
		for k, v := range dcConf.ContainerEnv {
			if _, set := hostEnv[k]; !set {
				if hostEnv == nil {
					hostEnv = map[string]string{}
				}
				hostEnv[k] = v
			}
		}
	}

	// Apply user-declared devcontainer Features (Phase 2b). Runs
	// before warm-install so a Feature that installs a runtime (Node,
	// Bun, …) is available to the lockfile-detected installer that
	// follows.
	consent, err := applyRepoFeatures(
		context.Background(), containerName, dcConf,
		featureConsentForNew, os.Stdin, cobraOut(),
	)
	if err != nil {
		return err
	}

	if err := runWarmInstall(containerName, hostEnv); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: warm install: %v\n", err)
	}

	if dcConf != nil {
		// onCreate runs before postCreate per spec. Sequential; a failure
		// aborts so the half-set-up container surfaces a clear error
		// rather than getting registered in a broken state.
		if err := devcontainer.RunLifecycle(
			containerName, devcontainer.StageOnCreate, dcConf.OnCreateCommand,
			1000, hostEnv, paths.RepoMountPath, cobraOut(),
		); err != nil {
			return err
		}
		if err := devcontainer.RunLifecycle(
			containerName, devcontainer.StagePostCreate, dcConf.PostCreateCommand,
			1000, hostEnv, paths.RepoMountPath, cobraOut(),
		); err != nil {
			return err
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
	repoRow := registry.Repo{
		Name:              slug,
		Aliases:           aliases,
		Remote:            url,
		DefaultBase:       defaultBase,
		BaseContainerName: containerName,
	}
	if len(consent) > 0 {
		repoRow.FeatureConsent = consent
	}
	reg.Repos = append(reg.Repos, repoRow)
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

// wireBranchContainer applies the per-container config + devices ahjo
// needs on every container: runtime security flags, host-keys disk
// devices, and ssh-agent proxy. Runs while the container is still in
// `incus init`-stopped state so first start honors raw.idmap and the
// security keys.
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
	// Pre-seed the user-session env on the container so every `incus exec`
	// inherits HOME/USER/LOGNAME/SHELL the way a Docker exec inherits them
	// from the image's ENV layer. Without these, `incus exec --user 1000`
	// hands the child an empty HOME — bash -l skips ~/.profile, and tools
	// that key off HOME (go's GOCACHE, gh's token store, claude's config)
	// either refuse to start or write to the wrong place. Docker dev
	// containers get these from the image; Incus system containers don't,
	// so ahjo sets them at the container level once. `incus copy` carries
	// environment.* keys to branch containers, so branches inherit
	// automatically.
	for k, v := range map[string]string{
		"HOME":    "/home/ubuntu",
		"USER":    "ubuntu",
		"LOGNAME": "ubuntu",
		"SHELL":   "/bin/bash",
	} {
		if err := incus.ConfigSet(containerName, "environment."+k, v); err != nil {
			return fmt.Errorf("set environment.%s: %w", k, err)
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
		hostKeysDir+"/authorized_keys", "/home/ubuntu/.ssh/authorized_keys",
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

// securityConfigFlags are the per-container Incus config keys ahjo applies:
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
// Runs as the `ubuntu` user (uid 1000) — `/repo` is owned by ubuntu, and
// git refuses to run on a tree it considers "dubiously owned" when invoked
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

// remoteUserWarning returns the spec-vs-ahjo user-mismatch message for cfg
// or empty string when there's no mismatch (or no devcontainer.json).
// `ubuntu` is the canonical in-image account; ahjo never honors a different
// remoteUser/containerUser declaration because git config / SSH keys are
// pre-staged at /home/ubuntu.
func remoteUserWarning(cfg *devcontainer.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.CheckRemoteUser("ubuntu")
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
// containerName at the corresponding paths under /home/ubuntu, then chowns
// the pushed paths to uid 1000.
//
// Source home resolution: $AHJO_HOST_HOME wins when set. The Mac shim
// (cmd/ahjo/main_darwin.go) forwards the user's Mac home through
// `limactl shell ... env AHJO_HOST_HOME=$HOME` so the in-VM ahjo reads
// from /Users/<user>/.claude/* (reverse-mounted by Lima) instead of the
// sparse VM home where claude was set up but no CLAUDE.md / skills /
// agents live. On Linux bare-metal AHJO_HOST_HOME is unset and
// os.UserHomeDir() is the right answer.
//
// Optional file pushes (.credentials.json, ~/.claude.json) silently
// no-op when missing — that's a normal state for some auth modes.
// CLAUDE.md and settings.json *also* no-op silently but emit a warn,
// since their absence is almost always a misconfigured source rather
// than a deliberate choice.
func pushClaudeConfig(containerName string) error {
	home := os.Getenv("AHJO_HOST_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = h
	}
	if err := incus.ExecAs(containerName, 0, nil, "/", "install", "-d", "-m", "0755", "-o", "ubuntu", "-g", "ubuntu", "/home/ubuntu/.claude"); err != nil {
		return fmt.Errorf("mkdir /home/ubuntu/.claude: %w", err)
	}
	// .credentials.json is intentionally NOT copied. ahjo authenticates via
	// the env-var OAuth token (rank 5, see CLAUDE-SETTING.md), and the
	// only thing that file ever carries is subscription OAuth state with a
	// single-use refresh token — propagating it to N containers would
	// reintroduce the cross-container refresh race the design avoids.
	files := []struct {
		src, dst    string
		warnMissing bool
	}{
		{home + "/.claude/settings.json", "/home/ubuntu/.claude/settings.json", true},
		{home + "/.claude/CLAUDE.md", "/home/ubuntu/.claude/CLAUDE.md", true},
		{home + "/.claude.json", "/home/ubuntu/.claude.json", false},
	}
	var pushed []string
	for _, f := range files {
		ok, err := incus.FilePush(containerName, f.src, f.dst)
		if err != nil {
			return fmt.Errorf("push %s: %w", f.src, err)
		}
		if ok {
			pushed = append(pushed, f.dst)
			continue
		}
		if f.warnMissing {
			fmt.Fprintf(cobraOutErr(), "warn: %s not found, skipping\n", f.src)
		}
	}
	// Recursive trees of user-authored config that's safe to clone:
	// markdown definitions and reference docs. hooks/ and plugins/ are
	// intentionally excluded — hooks shell out to host binaries that may
	// not exist in the container, and plugins/ is runtime install state
	// (caches, marketplace clones, blocklists) that's per-machine.
	dirs := []struct{ src, dst string }{
		{home + "/.claude/agents", "/home/ubuntu/.claude/agents"},
		{home + "/.claude/commands", "/home/ubuntu/.claude/commands"},
		{home + "/.claude/skills", "/home/ubuntu/.claude/skills"},
		{home + "/.claude/rules", "/home/ubuntu/.claude/rules"},
	}
	for _, d := range dirs {
		info, err := os.Stat(d.src)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat %s: %w", d.src, err)
		}
		if !info.IsDir() {
			continue
		}
		if err := incus.FilePushRecursive(containerName, d.src, d.dst); err != nil {
			return fmt.Errorf("push %s: %w", d.src, err)
		}
		pushed = append(pushed, d.dst)
	}
	if len(pushed) > 0 {
		args := append([]string{"chown", "-R", "1000:1000"}, pushed...)
		if err := incus.ExecAs(containerName, 0, nil, "/", args...); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: chown claude config: %v\n", err)
		}
	}
	return nil
}

// seedGitIdentity writes user.name + user.email into ubuntu's
// /home/ubuntu/.gitconfig so commits inside the container have an author.
// `incus copy` carries the file into every COW branch container, so this
// runs once on the repo's default-branch container.
//
// Signing settings (commit.gpgsign, user.signingkey) are intentionally not
// copied — the container has no access to the host keychain or GPG agent.
//
// HOME is set explicitly because `incus exec` doesn't read /etc/passwd
// the way sshd+PAM does — without it `git config --global` errors with
// "$HOME not set" (the global config target is $HOME/.gitconfig, and
// git refuses to guess when HOME is empty). The interactive attach path
// (incus.ExecAttach) already seeds this for uid 1000; ExecAs is the
// one-shot equivalent and intentionally minimal, so callers like this
// one specify what they need.
func seedGitIdentity(containerName string, id git.Identity) error {
	fmt.Printf("→ seeding git identity (%s): %s <%s>\n", id.Source, id.Name, id.Email)
	env := map[string]string{"HOME": "/home/ubuntu"}
	if err := incus.ExecAs(containerName, 1000, env, "/home/ubuntu",
		"git", "config", "--global", "user.name", id.Name); err != nil {
		return err
	}
	if err := incus.ExecAs(containerName, 1000, env, "/home/ubuntu",
		"git", "config", "--global", "user.email", id.Email); err != nil {
		return err
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
		Short: "Stop+delete every branch container in the repo (including the default), free ports, drop registry entries",
		Long: `Removes a repo end-to-end: every branch container in the repo (including the
default-branch container that 'repo add' created as the COW source) is stopped
and deleted, its SSH port is freed, host-keys are removed, the registry rows
are dropped, and ssh-config is regenerated.

If any non-default branch containers exist, the command refuses unless --force
is passed — those branches typically hold in-flight work.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRepoRm(args[0], force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "also delete non-default branch containers in this repo (loses any in-flight work in those branches)")
	return cmd
}

func runRepoRm(alias string, force bool) error {
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

	var defaultBranchKey string
	var nonDefaultKeys []string
	for _, b := range reg.Branches {
		if b.Repo != repo.Name {
			continue
		}
		if b.IsDefault {
			defaultBranchKey = b.Branch
		} else {
			nonDefaultKeys = append(nonDefaultKeys, b.Branch)
		}
	}
	if len(nonDefaultKeys) > 0 && !force {
		return fmt.Errorf("repo %q has %d branch container(s) besides default; pass --force to delete them too", repo.Aliases[0], len(nonDefaultKeys))
	}

	// Remove non-default branches first so the default-branch row is the
	// last write that also drops the repo row (see removeBranchLocked).
	for _, branchKey := range nonDefaultKeys {
		br := reg.FindBranch(repo.Name, branchKey)
		if br == nil {
			continue
		}
		if err := removeBranchLocked(reg, br, false); err != nil {
			return err
		}
	}

	if defaultBranchKey != "" {
		br := reg.FindBranch(repo.Name, defaultBranchKey)
		if br != nil {
			return removeBranchLocked(reg, br, true)
		}
	}

	// Legacy state: repo row exists with no default-branch row (e.g. left
	// behind by the old registry-only repo rm). Best-effort: delete the
	// base container if its name is recorded, then drop the repo row.
	if name := repo.BaseContainerName; name != "" {
		fmt.Printf("→ incus delete --force %s\n", name)
		if err := incus.ContainerDeleteForce(name); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: incus delete %s: %v\n", name, err)
		}
	}
	// Remove the per-repo .env (mode 0600 secret). Best-effort: a missing
	// file is fine, a permission failure is logged but doesn't block the
	// rest of the cleanup.
	if err := os.Remove(paths.SlugEnvPath(repo.Name)); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(cobraOutErr(), "warn: remove %s: %v\n", paths.SlugEnvPath(repo.Name), err)
	}
	reg.RemoveRepo(repo.Name)
	if err := reg.Save(); err != nil {
		return err
	}
	if err := sshpkg.RegenerateConfig(reg); err != nil {
		return err
	}
	fmt.Printf("removed repo %s\n", repo.Aliases[0])
	return nil
}

func newRepoPullCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pull <repo-alias>",
		Short: "git pull --ff-only in the repo's default-branch container",
		Long: `Updates the default-branch container (the COW source for every branch
container in this repo) against origin. Starts the container if it was
stopped, pulls fast-forward only, and leaves it running. Failures surface
verbatim from git — no silent recovery.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRepoPull(args[0])
		},
	}
}

func runRepoPull(repoAlias string) error {
	reg, err := registry.Load()
	if err != nil {
		return err
	}
	repo := reg.FindRepoByAlias(repoAlias)
	if repo == nil {
		return fmt.Errorf("no repo with alias %q (try `ahjo repo ls`)", repoAlias)
	}
	if repo.BaseContainerName == "" {
		return fmt.Errorf("repo %q has no base container; re-add it with `ahjo repo add`", repo.Aliases[0])
	}

	status, err := incus.ContainerStatus(repo.BaseContainerName)
	if err != nil {
		return err
	}
	if !strings.EqualFold(status, "Running") {
		fmt.Printf("→ incus start %s\n", repo.BaseContainerName)
		if err := incus.Start(repo.BaseContainerName); err != nil {
			return err
		}
		if err := incus.WaitReady(repo.BaseContainerName, 30*time.Second); err != nil {
			return err
		}
	}

	fmt.Printf("→ git pull --ff-only (in %s)\n", repo.BaseContainerName)
	return incus.ExecAs(
		repo.BaseContainerName, 1000, nil, paths.RepoMountPath,
		"git", "pull", "--ff-only",
	)
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
	if err := runRepoAdd(url, "", "", false); err != nil {
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

// promptRepoGHToken interactively asks for a GitHub PAT to forward into
// containers for this repo. Non-fatal in every skip path: --yes, non-TTY
// stdin, an existing per-repo PAT, and an empty paste all return nil.
//
// We prompt at `ahjo repo add` rather than `ahjo init` because least
// privilege wants a fine-grained PAT scoped to *this* repo — a question only
// answerable once the repo identity exists. Fine-grained PATs cannot be
// API-minted; the user creates them through the GitHub UI.
func promptRepoGHToken(slug, primary string, yes bool) error {
	envPath := paths.SlugEnvPath(slug)
	if _, found, err := tokenstore.GetAt(envPath, tokenstore.GHTokenEnv); err != nil {
		return err
	} else if found {
		fmt.Fprintln(cobraOut(), "  → GH_TOKEN already set for this repo; skipping prompt.")
		return nil
	}
	if yes {
		return nil
	}
	if !isTerminal(os.Stdin) {
		return nil
	}

	owner, name, ok := splitRepoAlias(primary)
	scopeURL := "https://github.com/settings/personal-access-tokens/new"
	scopeNote := ""
	if ok {
		scopeNote = fmt.Sprintf("        Repository access:  Only select repositories → %s/%s\n", owner, name)
	}

	out := cobraOut()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Set a GitHub token for this repo? gh inside containers will use it.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Fine-grained PAT (recommended) — scope it to JUST this repo:")
	fmt.Fprintln(out, "    → "+scopeURL)
	if scopeNote != "" {
		fmt.Fprint(out, scopeNote)
	}
	fmt.Fprintln(out, "        Permissions:        Contents (read or RW), Pull requests (RW),")
	fmt.Fprintln(out, "                            Issues (RW), Metadata (read — required)")
	fmt.Fprintln(out, "        Expiration:         your call")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Classic PAT — broader, easier, less safe.")
	fmt.Fprintln(out)

	tok, err := readSecret(os.Stdin, out, cobraOutErr(), "Paste a token now, or press Enter to skip: ")
	if err != nil {
		return err
	}
	if tok == "" {
		fmt.Fprintln(out, "  → skipped. `gh` inside containers for this repo will require manual auth.")
		fmt.Fprintln(out, "     Add later:  ahjo repo set-token "+primary)
		fmt.Fprintln(out, "     Or globally (warning: exposes all your repos):")
		fmt.Fprintln(out, "       ahjo env set GH_TOKEN \"$(gh auth token)\"")
		return nil
	}
	return saveRepoGHToken(slug, tok)
}

// saveRepoGHToken validates tok permissively and writes it to the per-repo
// .env file. The non-canonical hint is printed to stderr but doesn't reject.
func saveRepoGHToken(slug, tok string) error {
	canonical, hint, err := looksLikeGitHubToken(tok)
	if err != nil {
		return fmt.Errorf("token rejected: %w", err)
	}
	if !canonical && hint != "" {
		fmt.Fprintln(cobraOutErr(), "warn: "+hint)
	}
	envPath := paths.SlugEnvPath(slug)
	if err := tokenstore.SetAt(envPath, tokenstore.GHTokenEnv, tok); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	fmt.Fprintf(cobraOut(), "  → saved to %s\n", envPath)
	return nil
}

func newRepoSetTokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-token <alias>",
		Short: "Set or rotate the GitHub PAT forwarded into containers for one repo",
		Long: `Prompts (with hidden input) for a GitHub token and stores it at
~/.ahjo/repo-env/<slug>.env (mode 0600). The token is forwarded into every
container for this repo via GH_TOKEN.

Prefer fine-grained PATs scoped to a single repo:
  → https://github.com/settings/personal-access-tokens/new`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRepoSetToken(args[0])
		},
	}
}

func runRepoSetToken(alias string) error {
	reg, err := registry.Load()
	if err != nil {
		return err
	}
	repo := reg.FindRepoByAlias(alias)
	if repo == nil {
		return fmt.Errorf("no repo with alias %q (try `ahjo repo ls`)", alias)
	}
	tok, err := readSecret(os.Stdin, cobraOut(), cobraOutErr(), fmt.Sprintf("Paste GitHub token for %s: ", repo.Aliases[0]))
	if err != nil {
		return err
	}
	if tok == "" {
		return fmt.Errorf("no token entered")
	}
	return saveRepoGHToken(repo.Name, tok)
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
