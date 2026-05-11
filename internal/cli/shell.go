package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/devcontainer"
	"github.com/lasselaakkonen/ahjo/internal/idmap"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func newShellCmd() *cobra.Command {
	var update bool
	cmd := &cobra.Command{
		Use:   "shell <alias>",
		Short: "Start (if needed) and attach an interactive shell to the branch's container",
		Long: `Start the container if needed, wire SSH proxy + sshd, attach an interactive
bash via ` + "`incus exec --force-interactive`" + ` as the in-container ` + "`ubuntu`" + ` user
in /repo. Use ` + "`ahjo claude`" + ` to launch ` + "`claude`" + ` instead.

Pass --update to discard the existing container before attaching: ahjo stops
it, deletes it, and recreates it from the repo's default-branch container
(running ` + "`git checkout -b <branch>`" + ` again on the fresh COW copy). The
host-keys, registry entry, and ssh port are preserved.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runShell(args[0], update)
		},
	}
	cmd.Flags().BoolVar(&update, "update", false, "destroy the existing container before attaching so it picks up the current ahjo-base image")
	return cmd
}

func runShell(alias string, update bool) error {
	br, containerName, err := prepareBranchContainer(alias, update)
	if err != nil {
		return err
	}
	dcConf, err := loadDevcontainerSafe(containerName)
	if err != nil {
		return err
	}
	env, err := branchEnv(containerName, dcConf)
	if err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: collect forward env: %v\n", err)
	}
	if err := runPostAttach(containerName, dcConf, env); err != nil {
		return err
	}
	_ = br
	return incus.ExecAttach(containerName, 1000, env, paths.RepoMountPath, "bash", "-l")
}

// prepareBranchContainer resolves the branch by alias, ensures its container
// exists (recreating from the default-branch base when missing or --update),
// starts it, wires the ssh proxy + sshd, and reconciles auto-expose. Returns
// the branch row and the resolved container name ready for an attach call.
func prepareBranchContainer(alias string, update bool) (*registry.Branch, string, error) {
	if _, err := EnsureBranch(alias); err != nil {
		return nil, "", err
	}
	reg, err := registry.Load()
	if err != nil {
		return nil, "", err
	}
	br := reg.FindBranchByAlias(alias)
	if br == nil {
		return nil, "", fmt.Errorf("internal: just-ensured branch %q not in registry", alias)
	}

	containerName := br.IncusName
	if containerName == "" {
		return nil, "", fmt.Errorf("registry row for %q has no incus_name; recreate with `ahjo rm %s && ahjo create`", alias, alias)
	}

	exists, err := incus.ContainerExists(containerName)
	if err != nil {
		return nil, "", err
	}
	if update {
		if exists {
			if err := stopAndRemoveMirror(containerName); err != nil {
				fmt.Fprintf(cobraOutErr(), "warn: stop mirror on %s: %v\n", containerName, err)
			}
			fmt.Printf("→ incus stop %s\n", containerName)
			_ = incus.Stop(containerName)
			fmt.Printf("→ incus delete --force %s\n", containerName)
			if err := incus.ContainerDeleteForce(containerName); err != nil {
				return nil, "", fmt.Errorf("delete container: %w", err)
			}
			exists = false
		}
	}
	if !exists {
		repo := reg.FindRepo(br.Repo)
		if repo == nil {
			return nil, "", fmt.Errorf("internal: branch %q references missing repo %q", alias, br.Repo)
		}
		if repo.BaseContainerName == "" {
			return nil, "", fmt.Errorf("repo %q has no base container; recreate with `ahjo rm %s && ahjo repo add`", br.Repo, alias)
		}
		fmt.Printf("container %q not found; recreating from base %s...\n", containerName, repo.BaseContainerName)
		if err := cloneFromBase(repo, br); err != nil {
			return nil, "", err
		}
	}

	if err := incus.Start(containerName); err != nil {
		return nil, "", err
	}
	if err := incus.WaitReady(containerName, 30*time.Second); err != nil {
		return nil, "", err
	}
	// Refresh ssh-agent post-start: Lima's host SSH_AUTH_SOCK path changes
	// per session, and bind=container proxy devices must be attached while
	// the container is running.
	if err := attachSSHAgent(containerName); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: ssh-agent proxy: %v\n", err)
	}
	if err := incus.AddProxyDevice(
		containerName, "ahjo-ssh",
		fmt.Sprintf("tcp:127.0.0.1:%d", br.SSHPort),
		"tcp:127.0.0.1:22",
	); err != nil {
		return nil, "", err
	}
	if _, err := incus.Exec(containerName, "systemctl", "start", "ssh"); err != nil {
		// non-fatal: maybe systemd not up yet, or already running
		fmt.Fprintf(cobraOutErr(), "warn: could not start sshd: %v\n", err)
	}

	// devcontainer.json's postStartCommand fires every time the container
	// starts (not just on first creation). Re-parse on each start — one
	// extra `incus exec ... cat` and we never have a cache to invalidate.
	if dcConf, err := loadDevcontainerSafe(containerName); err != nil {
		return nil, "", err
	} else if dcConf != nil {
		env, _ := branchEnv(containerName, dcConf)
		if err := devcontainer.RunLifecycle(
			containerName, devcontainer.StagePostStart, dcConf.PostStartCommand,
			1000, env, paths.RepoMountPath, cobraOut(),
		); err != nil {
			return nil, "", err
		}
	}

	// Auto-expose: reconcile proxy devices for any pre-existing listeners.
	// Best-effort — failures here must not block the user from getting their
	// shell.
	release, err := lockfile.Acquire()
	if err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: auto-expose skipped: %v\n", err)
	} else {
		if err := reconcileAutoExpose(cobraOut(), br); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: auto-expose: %v\n", err)
		}
		release()
	}

	return br, containerName, nil
}

// loadDevcontainerSafe wraps devcontainer.LoadFromContainer with a warning
// path: a parse / read failure is logged but not fatal at attach time. The
// only attach-blocking case is a present-but-unsupported config; that's
// reported as an error so the user knows to fix it.
func loadDevcontainerSafe(container string) (*devcontainer.Config, error) {
	cfg, found, err := devcontainer.LoadFromContainer(container)
	if err != nil {
		if found {
			// parsed-but-rejected (e.g. `image:` declared) — surface so
			// the user fixes the file before next start.
			return nil, err
		}
		fmt.Fprintf(cobraOutErr(), "warn: read devcontainer.json: %v\n", err)
		return nil, nil
	}
	if cfg != nil {
		if msg := cfg.CheckRemoteUser("ubuntu"); msg != "" {
			fmt.Fprintln(cobraOutErr(), msg)
		}
	}
	return cfg, nil
}

// runPostAttach runs cfg.PostAttachCommand right before ahjo execs into
// the user's shell. Sequential with the rest of the prep flow; failure
// aborts the attach so the user sees the error rather than a silent
// half-launch.
func runPostAttach(container string, cfg *devcontainer.Config, env map[string]string) error {
	if cfg == nil {
		return nil
	}
	return devcontainer.RunLifecycle(
		container, devcontainer.StagePostAttach, cfg.PostAttachCommand,
		1000, env, paths.RepoMountPath, cobraOut(),
	)
}

// applyRawIdmap stops the container, sets the per-container raw.idmap that
// maps the in-VM host UID/GID onto the container's `ubuntu` user, and starts
// it back up. raw.idmap is honored at next start, so the stop/start cycle is
// what makes it take effect on a container that's currently running.
//
// See CONTAINER-ISOLATION.md "Workspace UID mapping" for the host-side
// mechanics ahjo wires (subuid/subgid + the per-container raw.idmap value).
func applyRawIdmap(containerName string) error {
	val := idmap.RawIdmapValue(os.Getuid(), os.Getgid())
	if err := incus.Stop(containerName); err != nil {
		return fmt.Errorf("stop %s before raw.idmap: %w", containerName, err)
	}
	if err := incus.ConfigSet(containerName, "raw.idmap", val); err != nil {
		return fmt.Errorf("set raw.idmap on %s: %w", containerName, err)
	}
	return nil
}

// branchEnv builds the env map propagated into the container at attach time:
// global cfg.ForwardEnv ∪ customizations.ahjo.forward_env from the parsed
// devcontainer.json, resolved against the host's current environment. Keys
// that aren't set on the host fall through silently. dcConf may be nil when
// the repo has no devcontainer.json; the global default still applies.
func branchEnv(containerName string, dcConf *devcontainer.Config) (map[string]string, error) {
	gcfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	_ = containerName // dcConf is read by the caller; signature retained for symmetry
	keys := append([]string(nil), gcfg.ForwardEnv...)
	if dcConf != nil {
		keys = append(keys, dcConf.Customizations.Ahjo.ForwardEnv...)
	}
	env := make(map[string]string, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	return env, nil
}
