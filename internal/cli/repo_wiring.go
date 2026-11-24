package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	sshpkg "github.com/lasselaakkonen/ahjo/internal/ssh"
)

// wireBranchContainer applies the per-container config + devices ahjo
// needs on every container: runtime security flags, host-keys disk
// devices, and ssh-agent proxy. Runs while the container is still in
// `incus init`-stopped state so first start honors raw.idmap and the
// security keys.
//
// `incus copy` propagates instance config + device definitions to branch
// containers, so flags applied here on the default container are inherited
// by every COW branch. raw.idmap and the ssh-agent socket path are the
// exceptions — both must be reapplied after copy (see create.go cloneFromBase).
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
	// Forward this layer's cumulative pubkey set into the new container at
	// paths.AncestorPubkeysMount. The child's pubKeyHomes() reads from
	// there as a third source when authoring ITS authorized_keys, so the
	// recursion (ahjo-in-ahjo-in-ahjo-…) propagates pubkeys at every hop
	// without relying on a Mac virtiofs window. The staged dir was
	// populated by WriteAuthorizedKeys (it lives at hostKeysDir/ancestor-pubkeys).
	if err := incus.AddDiskDevice(
		containerName, "ahjo-ancestor-pubkeys",
		hostKeysDir+"/ancestor-pubkeys", paths.AncestorPubkeysMount,
		true,
	); err != nil {
		return err
	}
	// SSH_AUTH_SOCK env can be set on a stopped container; the listen
	// socket itself can only be created post-start (see attachSSHAgent).
	// This env value COW-propagates to branch containers, but it is harmless
	// without the proxy device: if shouldForwardAgent suppresses the agent,
	// /tmp/ssh-agent.sock simply never appears, so SSH_AUTH_SOCK points at a
	// non-existent socket and ssh/git behave as if no agent is configured —
	// it does not leak the host agent. The proxy device is the actual control
	// point: every site that starts a container strips it first
	// (stripSSHAgent), and applySSHAgentPolicy re-creates it post-start only
	// when policy allows.
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
func attachSSHAgent(ctx context.Context, containerName string) error {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	return incus.EnsureSSHAgentProxy(ctx, containerName, sock)
}

// sshAgentDevice is the Incus proxy device that exposes the host ssh-agent
// inside a container as /tmp/ssh-agent.sock (see incus.EnsureSSHAgentProxy).
const sshAgentDevice = "ssh-agent"

// errAgentStrip marks a failure to remove the ssh-agent device. Callers that
// treat a failed attach as a warning check it with errors.Is so a failed
// strip still aborts: leaving the device in place would forward the agent
// into a container policy says must not have it (fail closed).
var errAgentStrip = errors.New("remove ssh-agent device")

// stripSSHAgent removes the ssh-agent proxy device from containerName. Works
// on running and stopped containers and is a no-op when the device is absent.
// Every site that starts or restarts a container calls this first: a device
// left in a stopped container's config is live from the moment the container
// boots, before any post-start policy check could run. The stored device also
// points at the SSH_AUTH_SOCK of whichever session added it, so it is always
// re-created post-start anyway (applySSHAgentPolicy). Errors wrap errAgentStrip.
func stripSSHAgent(containerName string) error {
	if err := incus.RemoveDevice(containerName, sshAgentDevice); err != nil {
		return fmt.Errorf("%w from %s: %w", errAgentStrip, containerName, err)
	}
	return nil
}

// applySSHAgentPolicy reconciles the ssh-agent device on a RUNNING container
// with shouldForwardAgent: attach when the repo should get the agent, strip
// otherwise. A strip failure wraps errAgentStrip and must be treated as fatal
// by every caller; an attach failure keeps each site's own severity.
func applySSHAgentPolicy(ctx context.Context, containerName string, repo *registry.Repo, cfg *config.Config) error {
	if shouldForwardAgent(repo, repoHasToken(repo), cfg) {
		if err := attachSSHAgent(ctx, containerName); err != nil {
			return fmt.Errorf("attach ssh-agent: %w", err)
		}
		return nil
	}
	return stripSSHAgent(containerName)
}

// configGet is incus.ConfigGet, swappable in tests.
var configGet = incus.ConfigGet

// repoHasToken reports whether a GitHub PAT covers repo, for the ssh-agent
// policy. True when any of:
//
//   - the registry records it (HasToken: set at repo add / set-token, or
//     backfilled by syncRepoHasToken);
//   - a token is visible to this invocation (repoToken: the Mac shim's
//     injected GH_TOKEN, a global GH_TOKEN, or the Linux per-repo env file);
//   - the repo's base container carries environment.GH_TOKEN, which
//     installRepoToken sets at add / set-token time. This identifies PAT repos
//     registered before HasToken existed without needing the Keychain. A repo
//     can set that key itself through containerEnv, but only towards
//     suppressing the agent on its own containers, which is the safe
//     direction. Probe errors count as "no token".
//
// A nil repo has no token.
func repoHasToken(repo *registry.Repo) bool {
	if repo == nil {
		return false
	}
	if repo.HasToken {
		return true
	}
	if tok, found, err := repoToken(repo.Name); err == nil && found && tok != "" {
		return true
	}
	if repo.BaseContainerName != "" {
		if v, err := configGet(repo.BaseContainerName, "environment.GH_TOKEN"); err == nil && v != "" {
			return true
		}
	}
	return false
}

// isHTTPSRemote reports whether remote is an HTTPS GitHub origin — the only
// case where a per-repo PAT (via gh's credential helper) authenticates git on
// its own, so the ssh-agent isn't needed for git. GitHub-strict on purpose: a
// self-hosted HTTPS remote with different PAT semantics shouldn't be silently
// treated as covered.
//
// Parsed rather than prefix-matched so equivalent spellings of the same origin
// (`https://GitHub.com/…`, `https://github.com:443/…`, `https://user@github.com/…`)
// are recognised: git and gh treat them all as github.com, so a prefix miss
// would silently keep the agent forwarded on a PAT-covered repo. Anything that
// is not exactly host github.com on the default port (e.g.
// `github.com.evil.example`, `:8443`) stays uncovered.
func isHTTPSRemote(remote string) bool {
	u, err := url.Parse(remote)
	if err != nil {
		return false
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	if !strings.EqualFold(u.Hostname(), "github.com") {
		return false
	}
	if p := u.Port(); p != "" && p != "443" {
		return false
	}
	return u.Path != "" && u.Path != "/"
}

// shouldForwardAgent decides whether to proxy the host ssh-agent into a repo's
// containers. An explicit config value wins; otherwise (the nil/auto default)
// the agent is suppressed only when an HTTPS GitHub origin is already covered
// by a PAT — git there uses the token, so forwarding the agent would be pure
// attack surface (the "SSH-agent hole" in docs/CONTAINER-ISOLATION.md). SSH
// origins, no-PAT repos, and non-GitHub remotes keep the agent.
func shouldForwardAgent(repo *registry.Repo, hasToken bool, cfg *config.Config) bool {
	if cfg != nil && cfg.ForwardSSHAgent != nil {
		return *cfg.ForwardSSHAgent
	}
	covered := repo != nil && hasToken && isHTTPSRemote(repo.Remote)
	return !covered
}

// forwardAgentForSSHConfig is the per-repo ForwardAgent decision for the
// generated ssh-config. It consults only the registry's HasToken flag, never
// per-invocation token sources: the config renders every repo at once, and on
// macOS an injected GH_TOKEN belongs to the one repo the shim resolved.
func forwardAgentForSSHConfig(cfg *config.Config) func(*registry.Repo) bool {
	return func(r *registry.Repo) bool {
		return shouldForwardAgent(r, r != nil && r.HasToken, cfg)
	}
}

// regenerateSSHConfig re-renders ~/.ahjo-shared/ssh-config (plus the alias
// and known_hosts files) with ForwardAgent following shouldForwardAgent. A
// config.toml that fails to load degrades to the auto policy with a warning
// rather than blocking the registry write that triggered the regeneration.
func regenerateSSHConfig(reg *registry.Registry) error {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: load config for ssh-config (using auto ssh-agent policy): %v\n", err)
		cfg = nil
	}
	return sshpkg.RegenerateConfig(reg, forwardAgentForSSHConfig(cfg))
}

// attachPasteShim installs the in-container half of the macOS host paste
// bridge: the xclip/wl-paste shims at /usr/local/bin/* and an Incus proxy
// device that forwards container:127.0.0.1:18340 to host.lima.internal:18340.
// Must run post-start — the proxy uses bind=container, which needs a live
// container namespace, and `incus file push` requires the container to be
// running too.
//
// Every component is best-effort: a missing host paste-daemon, a failed
// proxy add, or a refused `incus file push` must not block `ahjo shell` /
// `ahjo claude` from launching. Caller wraps each error in a warn and moves
// on. The shims gracefully exit 1 when the daemon is unreachable, which
// Claude treats as "no image on clipboard" — same as a stock Linux box
// with nothing in xclip.
func attachPasteShim(containerName string) error {
	if err := incus.EnsurePasteDaemonProxy(containerName); err != nil {
		return err
	}
	return incus.WritePasteShims(containerName)
}

// securityConfigFlags are the per-container Incus config keys ahjo applies
// unconditionally to every container:
//
//   - setxattr syscall intercept: required by dockerd >=26 (containerd
//     snapshotter uses xattr whiteouts) and by pnpm/npm postinstall scripts
//     that touch xattrs. Always on so non-Docker Node repos work without
//     declaring the Docker Feature.
//   - ip_unprivileged_port_start=0: lets a dev server bind :80 without sudo.
//   - security.guestapi=false: suppresses the host incus socket mount inside
//     the container.
//
// security.nesting is intentionally NOT here. Nesting widens the
// user-namespace / overlayfs kernel attack surface; on bare Linux it backs
// directly onto the real host kernel. It is enabled on a per-repo basis only
// when the repo declares `ahjo/docker` (via its Feature metadata) or sets
// `customizations.ahjo.nested_incus`, and only after the host-side user
// approves it (TTY prompt or --allow-nesting / --allow-nested-incus) — see
// elevationGate in elevation.go and its use in repoAddSetup.
//
// `incus copy` carries these keys to branch containers, so the default
// container's wireBranchContainer call covers the whole repo.
func securityConfigFlags() [][2]string {
	return [][2]string{
		{"security.syscalls.intercept.setxattr", "true"},
		{"linux.sysctl.net.ipv4.ip_unprivileged_port_start", "0"},
		{"security.guestapi", "false"},
	}
}
