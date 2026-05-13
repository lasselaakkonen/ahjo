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
	"github.com/lasselaakkonen/ahjo/internal/tokenstore"
)

func newShellCmd() *cobra.Command {
	var update bool
	var force bool
	cmd := &cobra.Command{
		Use:   "shell <alias>",
		Short: "Start (if needed) and attach an interactive shell to the branch's container",
		Long: `Start the container if needed, wire SSH proxy + sshd, attach an interactive
bash via ` + "`incus exec --force-interactive`" + ` as the in-container ` + "`ubuntu`" + ` user
in /repo. Use ` + "`ahjo claude`" + ` to launch ` + "`claude`" + ` instead.

Pass --update to discard the existing container before attaching: ahjo stops
it, deletes it, and recreates it from the repo's default-branch container
(running ` + "`git checkout -b <branch>`" + ` again on the fresh COW copy). The
host-keys, registry entry, and ssh port are preserved.

Before recreating with --update, ahjo inspects /repo for uncommitted/unpushed
work (starting a stopped container for the check, after prompting). If /repo
is dirty — or the user declines the start prompt — the command refuses to
proceed; pass --force to skip the check and recreate anyway.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runShell(args[0], update, force)
		},
	}
	cmd.Flags().BoolVar(&update, "update", false, "destroy the existing container before attaching so it picks up the current ahjo-base image")
	cmd.Flags().BoolVar(&force, "force", false, "with --update, skip the /repo cleanliness check and recreate even when uncommitted/unpushed work is present")
	return cmd
}

func runShell(alias string, update, force bool) error {
	br, containerName, err := prepareBranchContainer(alias, update, force)
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
func prepareBranchContainer(alias string, update, force bool) (*registry.Branch, string, error) {
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
			if err := ensureRepoCleanOrForce(br, "recreate", force); err != nil {
				return nil, "", err
			}
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
// devcontainer.json, resolved against the per-repo .env (highest precedence)
// then the host's current environment. Keys that aren't set anywhere fall
// through silently. dcConf may be nil when the repo has no devcontainer.json;
// the global default still applies.
//
// Per-repo overrides come from ~/.ahjo/repo-env/<slug>.env, populated by
// `ahjo repo add` (PAT prompt) and `ahjo repo set-token`. The slug is
// resolved from containerName via the registry; if no row exists (e.g. a
// brand-new container during `repo add`) only the process env is consulted.
func branchEnv(containerName string, dcConf *devcontainer.Config) (map[string]string, error) {
	gcfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	keys := append([]string(nil), gcfg.ForwardEnv...)
	if dcConf != nil {
		keys = append(keys, dcConf.Customizations.Ahjo.ForwardEnv...)
	}

	repoEnv := map[string]string{}
	if slug := slugForContainer(containerName); slug != "" {
		if err := tokenstore.LoadInto(paths.SlugEnvPath(slug), repoEnv); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: load per-repo env for %s: %v\n", slug, err)
		}
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
		if v, ok := repoEnv[k]; ok {
			env[k] = v
			continue
		}
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	return env, nil
}

// slugForContainer maps an Incus container name back to the repo slug that
// owns it. The repo container is "ahjo-<slug>" and branch containers are
// "ahjo-<slug>-<branch-safe>"; both rows are queried via the registry rather
// than parsed out of the name (branch slugs themselves contain hyphens, so
// string-splitting is ambiguous).
func slugForContainer(containerName string) string {
	if containerName == "" {
		return ""
	}
	reg, err := registry.Load()
	if err != nil {
		return ""
	}
	for i := range reg.Repos {
		if reg.Repos[i].BaseContainerName == containerName {
			return reg.Repos[i].Name
		}
	}
	for i := range reg.Branches {
		if reg.Branches[i].IncusName == containerName {
			return reg.Branches[i].Repo
		}
	}
	return ""
}
