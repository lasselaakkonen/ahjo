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

	"github.com/lasselaakkonen/ahjo/internal/ahjocontainer"
	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lima"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	sshpkg "github.com/lasselaakkonen/ahjo/internal/ssh"
)

func newRepoAddCmd() *cobra.Command {
	var defaultBase string
	var asAlias string
	var yes bool
	var containerConfig string
	cmd := &cobra.Command{
		Use:   "add <git-url>",
		Short: "Register a repo: clone it inside a fresh ahjo-base container at /repo and warm-install dependencies",
		Long: `Add a repo. The auto alias is derived from the URL as <owner>/<repo>.
Pass --as <alias> to register an additional alias for the same repo.
On auto-alias collision (e.g. github.com/acme/api vs gitlab.com/acme/api),
ahjo appends -2/-3/... to keep aliases unique.

The repo's default-branch container becomes the COW source from which every
subsequent ` + "`ahjo create`" + ` clones — its node_modules and pnpm store survive
into branch containers via btrfs reflinks, eliminating the cold-install tax.

URL handling: pass the URL you actually want to use. SSH remotes
(git@github.com:…) keep using the host's ssh-agent (forwarded into the
container). HTTPS remotes (https://github.com/…) authenticate via the
per-repo PAT prompted for before clone — ` + "`gh auth setup-git`" + ` wires git's
HTTPS credential helper to read it. ahjo does not auto-rewrite SSH ↔ HTTPS.
The same per-repo PAT is also solicited on the first ` + "`ahjo create <owner/repo>`" + `
(so its auto-add clones over HTTPS too). For an HTTPS origin covered by a PAT
the host ssh-agent is not forwarded into the repo's containers — git uses the
token instead; override with ` + "`forward_ssh_agent`" + ` in config.toml.

` + containerConfigHelpBlock,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoAdd(cmd.Context(), args[0], asAlias, defaultBase, yes, containerConfig)
		},
	}
	cmd.Flags().StringVar(&defaultBase, "default-base", "", "default branch to base new branches on (default: detect from the remote's HEAD)")
	cmd.Flags().StringVar(&asAlias, "as", "", "additional alias for this repo (must not collide with any existing alias)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the GitHub PAT prompt (the repo is added without a per-repo GH_TOKEN; set one later with `ahjo repo set-token`)")
	cmd.Flags().StringVar(&containerConfig, "container-config", "", containerConfigFlagShort)
	return cmd
}

func runRepoAdd(ctx context.Context, input, asAlias, defaultBase string, yes bool, containerConfig string) error {
	src := parseRepoSource(input)
	slug, primary, aliases, err := repoAddPlan(src.canonicalURL(), asAlias)
	if err != nil {
		return err
	}
	return repoAddSetup(ctx, slug, primary, aliases, src, defaultBase, yes, containerConfig)
}

// repoSource captures how a repo's clone URL should be derived. An explicit
// URL (the user typed git@… or https://…) is used verbatim — ahjo never
// auto-rewrites SSH↔HTTPS. A bare "<owner>/<repo>" GitHub alias defers the
// protocol choice to cloneURL: with a per-repo PAT in hand it clones over
// HTTPS so the token authenticates the clone and every later fetch/push;
// without one it keeps the historical SSH-then-HTTPS probe. Deferring is
// what lets `ahjo create owner/repo branch` honor a PAT the user only pastes
// at the prompt that runs *after* the alias is parsed.
type repoSource struct {
	explicitURL string // non-empty → used verbatim
	owner, name string // set when explicitURL == "" → protocol picked later
}

// parseRepoSource classifies raw `repo add` / `create` input. Anything
// containing "://" or "@" is an explicit URL; a bare "<owner>/<repo>" is a
// GitHub alias; anything else passes through verbatim (a local path, etc.).
func parseRepoSource(input string) repoSource {
	if strings.Contains(input, "://") || strings.Contains(input, "@") {
		return repoSource{explicitURL: input}
	}
	if owner, name, ok := splitRepoAlias(input); ok {
		return repoSource{owner: owner, name: name}
	}
	return repoSource{explicitURL: input}
}

func (s repoSource) httpsURL() string {
	return fmt.Sprintf("https://github.com/%s/%s.git", s.owner, s.name)
}

// canonicalURL returns a protocol-independent URL for alias/slug
// allocation, which only cares about owner/repo. Never used to clone.
func (s repoSource) canonicalURL() string {
	if s.explicitURL != "" {
		return s.explicitURL
	}
	return s.httpsURL()
}

// cloneURL resolves the URL ahjo actually clones (and records as the repo's
// remote). Explicit URLs are verbatim. An inferred GitHub alias prefers
// HTTPS when a PAT is available so the clone — and every later fetch/push —
// authenticates via the token instead of silently falling onto the host's
// SSH key; with no token it keeps the SSH-then-HTTPS probe.
func (s repoSource) cloneURL(hasToken bool) string {
	if s.explicitURL != "" {
		return s.explicitURL
	}
	if hasToken {
		return s.httpsURL()
	}
	return pickGitHubURL(s.owner, s.name)
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
	slug = reg.AllocateRepoSlug(primary, func(s string) bool {
		// Reject slugs whose `ahjo-<slug>` container already exists in incus
		// but is unknown to the registry — the signature of a prior `repo add`
		// that crashed between `incus init` and the registry write. Suffix
		// past the orphan instead of failing inside the upcoming `incus init`.
		// Probe errors (incus unreachable, etc.) degrade to "not taken": the
		// same error will surface seconds later from `incus init` with full
		// context, which is more useful than aborting allocation here.
		exists, _ := incus.ContainerExists(registry.ContainerName(s))
		return exists
	})
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
func repoAddSetup(ctx context.Context, slug, primary string, aliases []string, src repoSource, defaultBase string, yes bool, containerConfig string) error {
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

	// Make sure this layer has at least one client SSH key on disk before
	// WriteAuthorizedKeys runs. On Mac/Lima this is a no-op; from layer 2
	// down (ahjo-in-ahjo), this is the floor that keeps the recursion
	// self-sustaining when no Mac virtiofs window is reachable. See
	// internal/ssh/keygen.go for the trigger conditions.
	if _, created, err := sshpkg.EnsureLocalKey(); err != nil {
		return fmt.Errorf("ensure local ssh key: %w", err)
	} else if created {
		fmt.Println("  → generated ~/.ssh/id_ed25519 (no prior client key found)")
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

	containerName := registry.ContainerName(slug)
	fmt.Printf("→ incus init ahjo-base %s\n", containerName)
	if err := incus.LaunchStopped(ctx, paths.AhjoBaseProfile, containerName); err != nil {
		return err
	}
	if err := wireBranchContainer(containerName, hostKeysDir); err != nil {
		return err
	}

	fmt.Printf("→ incus start %s\n", containerName)
	if err := incus.Start(containerName); err != nil {
		return err
	}
	if err := incus.WaitReady(ctx, containerName, 30*time.Second); err != nil {
		return err
	}
	// ssh-agent proxy must be attached post-start.
	if err := attachSSHAgent(ctx, containerName); err != nil {
		return fmt.Errorf("attach ssh-agent: %w", err)
	}
	// Paste-shim wiring: best-effort, never blocks repo add.
	if err := attachPasteShim(containerName); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: paste shim: %v\n", err)
	}

	if err := pushClaudeConfig(ctx, containerName); err != nil {
		return fmt.Errorf("push claude config: %w", err)
	}
	// Install the static AHJO.md (imported by CLAUDE.md) and seed an initial
	// ahjo-state.md. Best-effort: a missing bridge doc must not fail repo add.
	// COW clones inherit AHJO.md + the import; each gets its own state on attach.
	if err := installAhjoDoc(containerName); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: install AHJO.md: %v\n", err)
	}
	refreshAhjoStateByName(containerName, slug, primary, "")
	if err := incus.ExecAs(containerName, 1000, nil, "/", "/usr/local/bin/ahjo-claude-prepare"); err != nil {
		return fmt.Errorf("ahjo-claude-prepare: %w", err)
	}
	if err := seedGitIdentity(containerName, identity); err != nil {
		return fmt.Errorf("seed git identity: %w", err)
	}

	url, defaultBase, err := cloneRepo(ctx, containerName, slug, primary, src, defaultBase, yes)
	if err != nil {
		return err
	}

	dcConfs, err := repoAddResolveConfigs(containerName, containerConfig, yes)
	if err != nil {
		return err
	}

	// Apply containerEnv via Incus's `environment.<KEY>` config keys so
	// every subsequent `incus exec` (including warm-install, lifecycle
	// hooks, and the user's shell) sees the values. Already-running
	// services aren't restarted; the spec doesn't promise that.
	// Per-config in pick order: when two configs set the same key, the
	// later write wins via Incus's last-write-wins config semantics.
	for _, c := range dcConfs {
		if err := c.ApplyContainerEnv(func(k, v string) error {
			return incus.ConfigSet(containerName, k, v)
		}); err != nil {
			return err
		}
	}

	if anyNestedIncus(dcConfs) {
		if err := wireLoopDevices(containerName); err != nil {
			return fmt.Errorf("wire loop devices: %w", err)
		}
		fmt.Fprintln(cobraOutErr(),
			"warn: customizations.ahjo.nested_incus=true — kernel attack surface widened; see CONTAINER-ISOLATION.md")
	}

	// Resolve the host-env keys to forward into warm-install and lifecycle
	// hooks: global config.ForwardEnv ∪ union of each config's
	// customizations.ahjo.forward_env.
	envKeys := append([]string(nil), cfg.ForwardEnv...)
	for _, c := range dcConfs {
		envKeys = append(envKeys, c.Customizations.Ahjo.ForwardEnv...)
	}
	hostEnv := resolveHostEnv(envKeys)
	// Merge containerEnv into the per-exec env too — covers both stale
	// containers and the case where Incus's environment.* propagation
	// races a freshly-issued exec. Later configs override earlier ones,
	// matching the ApplyContainerEnv pass above.
	for _, c := range dcConfs {
		for k, v := range c.ContainerEnv {
			if hostEnv == nil {
				hostEnv = map[string]string{}
			}
			hostEnv[k] = v
		}
	}

	// Apply user-declared devcontainer Features (Phase 2b). Runs
	// before warm-install so a Feature that installs a runtime (Node,
	// Bun, …) is available to the lockfile-detected installer that
	// follows. When multiple configs are stacked we synthesize a single
	// config with their Features unioned (last-wins on key collision)
	// so the existing single-config resolver runs one trust-prompt /
	// fetch / resolve / apply pass for the combined set.
	consent, err := applyRepoFeatures(
		ctx, containerName, mergeFeaturesForApply(dcConfs),
		featureConsentForNew, os.Stdin, cobraOut(),
	)
	if err != nil {
		return err
	}

	if len(dcConfs) == 0 {
		// `bare` (either explicit --container-config bare, or chosen
		// from the picker after declining every detection suggestion):
		// no stack means no installer. Matches the existing "no
		// lockfile detected" line for parity in the output.
		fmt.Println("→ bare config; skipping warm install")
	} else if err := runWarmInstall(ctx, containerName, hostEnv); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: warm install: %v\n", err)
	}

	// onCreate runs before postCreate per spec, per config, in pick
	// order. Sequential within and across configs; a failure aborts so
	// the half-set-up container surfaces a clear error rather than
	// getting registered in a broken state. Lifecycle hooks aren't
	// merged because their three legal JSON shapes (string / array /
	// object) don't compose cleanly — running them in series is the
	// honest equivalent.
	for _, c := range dcConfs {
		if err := ahjocontainer.RunLifecycle(
			ctx, containerName, ahjocontainer.StageOnCreate, c.OnCreateCommand,
			1000, hostEnv, paths.RepoMountPath, cobraOut(),
		); err != nil {
			return err
		}
	}
	for _, c := range dcConfs {
		if err := ahjocontainer.RunLifecycle(
			ctx, containerName, ahjocontainer.StagePostCreate, c.PostCreateCommand,
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

	return commitRegistry(slug, primary, aliases, url, defaultBase, containerName, port, consent)
}

// cloneRepo is the token-resolution + clone phase of repoAddSetup. It runs
// the per-repo GitHub-token prompt, forwards the token onto the container and
// configures gh's credential helper when one is in hand, picks the clone URL
// (HTTPS-with-PAT vs SSH-then-HTTPS), clones /repo as the ubuntu user, and
// checks out an explicit --default-base. Returns the recorded remote URL and
// the resolved default branch (detected from the clone's HEAD when defaultBase
// was empty).
//
// The git clone + checkout run via ExecAsContext so a canceled ctx (Ctrl-C on
// a slow clone — the longest part of repo add) tears them down.
func cloneRepo(ctx context.Context, containerName, slug, primary string, src repoSource, defaultBase string, yes bool) (url, resolvedBase string, err error) {
	// Per-repo GitHub token prompt. Non-fatal: skipped on --yes / non-TTY /
	// already-set / empty paste. See `ahjo repo set-token` to add later.
	// Resolved before the clone URL is chosen so an inferred GitHub alias can
	// clone over HTTPS (and use the PAT) instead of falling onto the SSH key.
	if err := promptRepoGHToken(slug, primary, yes); err != nil {
		return "", "", err
	}
	// repoToken sees this prompt's paste, the Mac shim's pre-relay Keychain
	// inject, a global `ahjo env set GH_TOKEN`, or a prior repo-set-token run.
	tok, hasToken, err := repoToken(slug)
	if err != nil {
		return "", "", err
	}
	hasToken = hasToken && tok != ""
	if hasToken {
		// Promote the token onto the container as environment.GH_TOKEN/
		// GITHUB_TOKEN and configure git's HTTPS credential helper. `incus
		// copy` carries environment.* and the in-$HOME .gitconfig into every
		// branch container, so this runs once.
		if err := installRepoToken(func(k, v string) error { return incus.ConfigSet(containerName, k, v) }, tok); err != nil {
			return "", "", fmt.Errorf("forward GH_TOKEN to container: %w", err)
		}
		if err := incus.ExecAs(
			containerName, 1000,
			map[string]string{"HOME": "/home/ubuntu", "GH_TOKEN": tok},
			"/home/ubuntu",
			"gh", "auth", "setup-git",
		); err != nil {
			return "", "", fmt.Errorf("gh auth setup-git: %w", err)
		}
	}

	// Pick the clone URL now that we know whether a PAT is in hand. An
	// explicit URL is used verbatim; an inferred GitHub alias clones over
	// HTTPS when a token exists (so the PAT authenticates the clone and every
	// later fetch/push) and otherwise keeps the SSH-then-HTTPS probe. The
	// chosen URL is recorded as the repo's remote (registry row below).
	url = src.cloneURL(hasToken)

	// An inferred GitHub alias with no PAT falls onto SSH (when reachable).
	// That's a deliberate option for SSH-key users, but it's silent — surface
	// it so the dead-weight-PAT / agent-only-push footgun isn't a surprise.
	// Explicit user-typed git@ URLs (explicitURL != "") are intentional and
	// don't warn; the HTTPS-public fallback (no git@ prefix) doesn't either.
	if src.explicitURL == "" && !hasToken && strings.HasPrefix(url, "git@") {
		fmt.Fprintf(cobraOutErr(), "warn: cloning %s over SSH using the host ssh-agent "+
			"(no PAT set). git push/fetch will rely on the forwarded agent and `gh` "+
			"won't work. Set a repo-scoped PAT with `ahjo repo set-token %s`.\n", url, primary)
	}

	// /repo is at the container root, where uid 1000 can't `mkdir`. Create
	// it as ubuntu:ubuntu first so `git clone` runs unprivileged.
	if err := incus.ExecAs(containerName, 0, nil, "/", "install", "-d", "-m", "0755", "-o", "ubuntu", "-g", "ubuntu", paths.RepoMountPath); err != nil {
		return "", "", fmt.Errorf("create %s: %w", paths.RepoMountPath, err)
	}
	fmt.Printf("→ git clone %s %s (in container as ubuntu)\n", url, paths.RepoMountPath)
	// GIT_TERMINAL_PROMPT=0 turns a missing-credentials misconfig into a
	// fast clone failure instead of a hung `Username:` prompt the user
	// can't always see (e.g., when ahjo is invoked from a TUI surface).
	if err := incus.ExecAsContext(ctx, containerName, 1000, map[string]string{"GIT_TERMINAL_PROMPT": "0"}, "/", "git", "clone", url, paths.RepoMountPath); err != nil {
		return "", "", wrapCloneErr(err)
	}

	// A plain `git clone` checks out the remote's HEAD branch. When the user
	// passes an explicit --default-base that differs, the working tree is on
	// the wrong branch: the clone fetched origin/<default-base> as a
	// remote-tracking ref, but never checked it out. Detection (empty
	// defaultBase) reads that same HEAD, so the tree already matches and the
	// checkout below is a no-op; an explicit override must be checked out so
	// container-config detection, Features, warm-install, the lifecycle
	// hooks, and `ahjo shell <repo>@<default-base>` all operate on the
	// recorded base branch. (Feature containers re-checkout origin/<base> on
	// their own — see cli/create.go — but the base container is itself the
	// default-branch container.)
	explicitBase := defaultBase != ""
	if defaultBase == "" {
		defaultBase, err = detectContainerDefaultBranch(containerName)
		if err != nil {
			return "", "", fmt.Errorf("detect default branch (pass --default-base to override): %w", err)
		}
	}
	if explicitBase {
		fmt.Printf("→ git checkout -B %s origin/%s (in container)\n", defaultBase, defaultBase)
		if err := incus.ExecAsContext(ctx, containerName, 1000, nil, paths.RepoMountPath, "git", "checkout", "-B", defaultBase, "origin/"+defaultBase); err != nil {
			return "", "", fmt.Errorf("checkout --default-base %q (does the branch exist on the remote?): %w", defaultBase, err)
		}
	}
	return url, defaultBase, nil
}

// repoAddResolveConfigs is the container-config-resolution phase of
// repoAddSetup: it rejects a legacy .ahjoconfig, then resolves the
// ahjocontainer config(s) to apply. Order, first match wins:
//
//  1. Explicit --container-config — host path, repo-local .ahjo/<name>.json,
//     bundled stack, or "bare". Overrides the in-repo canonical file by
//     design (a contributor can spin up a CI-flavored container against a
//     repo whose committed ahjocontainer.json targets full local dev).
//  2. In-repo .ahjo/ahjocontainer.json (the canonical file).
//  3. Interactive picker on a TTY.
//  4. Bare (non-TTY fallback).
//
// Docker-flavored fields are rejected by the parser itself, so a returned
// *Config is already valid for ahjo. The result is a slice so detection can
// apply multiple stacks to a monorepo (node + go + docker etc.); the
// single-config paths wrap into a 1-element slice so the caller's
// apply-in-series block treats both shapes uniformly. Empty result == bare.
func repoAddResolveConfigs(containerName, containerConfig string, yes bool) ([]*ahjocontainer.Config, error) {
	if has, err := ahjocontainer.HasLegacyAhjoconfig(containerName); err != nil {
		return nil, fmt.Errorf("probe legacy .ahjoconfig: %w", err)
	} else if has {
		return nil, fmt.Errorf("/repo/.ahjoconfig is no longer supported. " +
			"Migrate it to .ahjo/ahjocontainer.json: " +
			"`run` → `postCreateCommand`, `forward_env` / `auto_expose` → `customizations.ahjo.*`")
	}

	var dcConfs []*ahjocontainer.Config
	if containerConfig != "" {
		cfg, _, err := resolveContainerConfig(containerName, containerConfig)
		if err != nil {
			return nil, err
		}
		if cfg != nil {
			dcConfs = []*ahjocontainer.Config{cfg}
		}
	} else {
		cfg, found, err := ahjocontainer.LoadFromContainer(containerName)
		if err != nil {
			return nil, err
		}
		if found {
			dcConfs = []*ahjocontainer.Config{cfg}
		} else {
			// Detection-driven suggestion: a lockfile, Dockerfile, or
			// compose manifest in /repo is the clearest hint about
			// which bundled stack(s) and Features the user wants.
			// Prompt per match before falling through to the generic
			// picker so the obvious case is one keypress per row, and
			// so `bare` (picked after declining everything) genuinely
			// means bare — runWarmInstall is gated on dcConfs below.
			matches, err := detectStacks(containerName)
			if err != nil {
				return nil, err
			}
			autoYes := yes || !isTerminal(os.Stdin)
			accepted, err := promptStackDetections(matches, os.Stdin, cobraOut(), autoYes)
			if err != nil {
				return nil, err
			}
			for _, m := range accepted {
				picked, err := resolveDetectMatch(m)
				if err != nil {
					return nil, err
				}
				fmt.Printf("→ applying %s\n", picked.Source)
				dcConfs = append(dcConfs, picked)
			}
			if len(dcConfs) == 0 {
				chosen, err := promptContainerConfig(containerName, os.Stdin, cobraOut())
				if err != nil {
					return nil, err
				}
				if chosen != "" && chosen != bareConfigName {
					picked, _, err := resolveContainerConfig(containerName, chosen)
					if err != nil {
						return nil, err
					}
					if picked != nil {
						dcConfs = []*ahjocontainer.Config{picked}
					}
				}
			}
		}
	}
	for _, c := range dcConfs {
		if msg := remoteUserWarning(c); msg != "" {
			fmt.Fprintln(cobraOutErr(), msg)
		}
	}
	return dcConfs, nil
}

// commitRegistry is the final phase of repoAddSetup: under the ahjo lock it
// re-validates the slug/aliases (a parallel run may have grabbed them during
// the unlocked setup), writes the repo + default-branch rows atomically,
// regenerates the ssh-config, and prints the ready line. Split out so
// repoAddSetup reads as a sequence of named phases rather than one long body.
func commitRegistry(slug, primary string, aliases []string, url, defaultBase, containerName string, port int, consent map[string]bool) error {
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

// runWarmInstall reuses detectStacks' probe + dedupe-by-name pipeline
// so a workspace repo (go.work + go.sum) runs `go work sync` once
// instead of also running `go mod download` after, and a repo carrying
// both pnpm-lock and package-lock warms pnpm only. Rows without a
// warm-install command (Docker, prek — the Feature install IS
// the warm-up) are skipped silently. Branch containers (cloned via
// `incus copy` with btrfs/zfs reflinks) inherit the hot dependency
// cache. hostEnv is forwarded into each installer (NPM_TOKEN etc.).
func runWarmInstall(ctx context.Context, containerName string, hostEnv map[string]string) error {
	matches, err := detectStacks(containerName)
	if err != nil {
		return err
	}
	return runWarmInstallWith(matches,
		// `command -v` is a POSIX shell builtin, so it must run through
		// /bin/sh — `incus exec` resolves argv[0] via execve, which only
		// finds binaries on PATH. e.cmd[0] is drawn from the hardcoded
		// detectTable (alphanumeric/dash only), so single-quoting the
		// arg is defense-in-depth rather than load-bearing.
		func(bin string) bool {
			_, err := incus.Exec(containerName, "/bin/sh", "-c", "command -v '"+bin+"'")
			return err == nil
		},
		// ExecAsContext: the installer (`npm ci`, `go mod download`, …) is the
		// canonical long/hang-prone step, so a canceled ctx kills it directly.
		func(argv []string) error {
			return incus.ExecAsContext(ctx, containerName, 1000, hostEnv, paths.RepoMountPath, argv...)
		},
		os.Stdout,
	)
}

// runWarmInstallWith is runWarmInstall's testable core: detection is
// already resolved, and the two side-effecting steps (binary precheck +
// installer invocation) are passed in. Output is written to out so
// tests can capture the user-visible skip/applied lines verbatim.
func runWarmInstallWith(
	matches []detectMatch,
	probeBin func(bin string) bool,
	runCmd func(argv []string) error,
	out io.Writer,
) error {
	any := false
	for _, m := range matches {
		e := m.entry
		if len(e.cmd) == 0 {
			continue
		}
		any = true
		// The chosen stack(s) may not provide every runtime the detect
		// table covers (polyglot repo, custom in-repo config, user
		// declined the stack but kept the lockfile), so a missing
		// binary becomes a one-line skip rather than a loud
		// `exec: cargo: not found`-style failure.
		if !probeBin(e.cmd[0]) {
			fmt.Fprintf(out, "→ %s not found in container; skipping %s\n", e.cmd[0], strings.Join(e.cmd, " "))
			continue
		}
		fmt.Fprintf(out, "→ %s (%s detected)\n", strings.Join(e.cmd, " "), m.hit)
		if err := runCmd(e.cmd); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(e.cmd, " "), err)
		}
	}
	if !any {
		fmt.Fprintln(out, "→ no lockfile detected; skipping warm install")
	}
	return nil
}

// remoteUserWarning returns the spec-vs-ahjo user-mismatch message for cfg
// or empty string when there's no mismatch (or no devcontainer.json).
// `ubuntu` is the canonical in-image account; ahjo never honors a different
// remoteUser/containerUser declaration because git config / SSH keys are
// pre-staged at /home/ubuntu.
func remoteUserWarning(cfg *ahjocontainer.Config) string {
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
func pushClaudeConfig(ctx context.Context, containerName string) error {
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
	// the env-var OAuth token (rank 5, see CLAUDE-SETTINGS.md), and the
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
		if err := incus.FilePushRecursive(ctx, containerName, d.src, d.dst); err != nil {
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
	// Wire ahjo's statusline unless the user brought their own. Best-effort: a
	// statusline failure must not fail container creation. May overwrite the
	// settings.json just pushed above (with the user's settings + our statusLine).
	if err := installClaudeStatusline(containerName, home); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: claude statusline: %v\n", err)
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
