package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
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
	// it does not leak the host agent. The proxy device (attachSSHAgent /
	// RemoveDevice at the runtime sites) is the actual control point.
	if os.Getenv("SSH_AUTH_SOCK") != "" {
		if err := incus.ConfigSet(containerName, "environment.SSH_AUTH_SOCK", "/tmp/ssh-agent.sock"); err != nil {
			return err
		}
	}
	return applyRawIdmap(containerName)
}

// wireLoopDevices attaches /dev/loop-control + /dev/loop0..7 to the
// container as unix-char/unix-block devices. Each /dev/loopN source is
// probed; missing nodes (some kernels expose fewer at boot) are skipped.
// Idempotent — incus.AddUnixDevice tolerates "already exists".
//
// Gated by customizations.ahjo.nested_incus. Enables nested Incus (or any
// tool needing loop-mounted block images) to operate inside the container.
// Capability bump — widens kernel filesystem-driver attack surface; see
// CONTAINER-ISOLATION.md for the trade-off.
//
// `incus copy` carries the device list, so wire-up on the default
// container propagates to every COW branch container automatically.
func wireLoopDevices(container string) error {
	if err := incus.AddUnixDevice(container, "ahjo-loop-control", "unix-char", "/dev/loop-control"); err != nil {
		return err
	}
	for i := 0; i < 8; i++ {
		src := fmt.Sprintf("/dev/loop%d", i)
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		name := fmt.Sprintf("ahjo-loop-%d", i)
		if err := incus.AddUnixDevice(container, name, "unix-block", src); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
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

// isHTTPSRemote reports whether remote is an HTTPS GitHub origin — the only
// case where a per-repo PAT (via gh's credential helper) authenticates git on
// its own, so the ssh-agent isn't needed for git. GitHub-strict on purpose: a
// self-hosted HTTPS remote with different PAT semantics shouldn't be silently
// treated as covered.
func isHTTPSRemote(remote string) bool {
	return strings.HasPrefix(remote, "https://github.com/")
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

// securityConfigFlags are the per-container Incus config keys ahjo applies:
// nesting (for docker-in-container), setxattr syscall intercept,
// unprivileged-port binding (so a dev server on :80 works without sudo),
// and disabling the guest-API mount (which exposes the host's incus
// socket inside).
//
// The setxattr intercept is load-bearing for docker-in-container: dockerd
// >=26 defaults to the containerd snapshotter, whose layer whiteouts are
// xattrs (trusted.overlay.opaque / trusted.overlay.whiteout). pnpm/npm
// postinstall scripts that touch xattrs lean on the same intercept.
//
// The mknod intercept was removed after tracing docker pull + a
// whiteout-producing docker build (FROM alpine; RUN touch /f; RUN rm /f):
// zero mknod/mknodat calls in either workload. The containerd snapshotter
// uses xattr whiteouts exclusively; the legacy graph driver's mknod-c-0-0
// whiteouts were never reliably covered by the intercept's mode/dev-bit
// matching anyway, and that driver is deliberately excluded (see
// ahjofeature_docker/feature/install.sh).
//
// `incus copy` carries these keys to branch containers, so the default
// container's wireBranchContainer call covers the whole repo.
func securityConfigFlags() [][2]string {
	return [][2]string{
		{"security.nesting", "true"},
		{"security.syscalls.intercept.setxattr", "true"},
		{"linux.sysctl.net.ipv4.ip_unprivileged_port_start", "0"},
		{"security.guestapi", "false"},
	}
}
