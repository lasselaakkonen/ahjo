package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/coi"
	"github.com/lasselaakkonen/ahjo/internal/idmap"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func newShellCmd() *cobra.Command {
	var update bool
	cmd := &cobra.Command{
		Use:   "shell <alias>",
		Short: "Start (if needed) and attach an interactive shell to the worktree's container",
		Long: `Start the container if needed, wire SSH proxy + sshd, attach an interactive
shell via ` + "`coi shell --debug`" + ` (skips COI's AI-tool launcher). Use ` + "`ahjo claude`" + `
to launch claude instead.

Pass --update to discard the existing container before attaching: ahjo shuts it
down, deletes it, and the regular first-shell path then re-creates it from the
current ahjo-base image (re-running ahjo-claude-prepare). The worktree, host
keys, registry entry, and ssh port are preserved. Use this after 'ahjo update'
or after editing the per-worktree .coi/config.toml (' ahjo new <repo> <branch>'
re-renders that file in place).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runShell(args[0], update)
		},
	}
	cmd.Flags().BoolVar(&update, "update", false, "destroy the existing container before attaching so it picks up the current ahjo-base image (keeps the worktree)")
	return cmd
}

func runShell(alias string, update bool) error {
	w, containerName, err := prepareWorktreeContainer(alias, update)
	if err != nil {
		return err
	}
	return coi.ExecShell(w.WorktreePath, containerName)
}

// prepareWorktreeContainer resolves the worktree by alias, creates the
// container on first use (or recreates it when update=true), starts it, wires
// the ssh proxy/sshd, and reconciles auto-expose. Returns the registry entry
// and the resolved container name ready for an attach call.
func prepareWorktreeContainer(alias string, update bool) (*registry.Worktree, string, error) {
	// EnsureWorktree handles auto-add of the parent repo (when alias is a
	// "<owner>/<repo>@<branch>" worktree alias) and auto-create of the
	// worktree itself. After it returns, the worktree is registered.
	if _, err := EnsureWorktree(alias); err != nil {
		return nil, "", err
	}
	reg, err := registry.Load()
	if err != nil {
		return nil, "", err
	}
	w := reg.FindWorktreeByAlias(alias)
	if w == nil {
		return nil, "", fmt.Errorf("internal: just-ensured worktree %q not in registry", alias)
	}

	var containerName string

	if w.IncusName != "" {
		// Container was created via incus copy (COW from the default-branch base);
		// it is not registered with COI so ResolveContainer won't find it.
		if update {
			return nil, "", fmt.Errorf("--update is not supported for COW-copied containers; recreate with `ahjo rm %s && ahjo new`", alias)
		}
		exists, err := incus.ContainerExists(w.IncusName)
		if err != nil {
			return nil, "", err
		}
		if !exists {
			repoEnt := reg.FindRepo(w.Repo)
			if repoEnt == nil {
				return nil, "", fmt.Errorf("internal: worktree %q references missing repo %q", alias, w.Repo)
			}
			if repoEnt.BaseContainerName == "" {
				return nil, "", fmt.Errorf("incus container %q not found and repo has no base container; recreate with `ahjo rm %s && ahjo new`", w.IncusName, alias)
			}
			fmt.Printf("container %q not found; recreating from base %s...\n", w.IncusName, repoEnt.BaseContainerName)
			if err := setupCOWContainer(repoEnt, w, w.IncusName); err != nil {
				return nil, "", err
			}
		}
		containerName = w.IncusName
	} else {
		// COI-managed container flow.
		const slot = 1
		containerName, err = coi.ResolveContainer(w.Slug, slot)
		if err != nil {
			return nil, "", err
		}

		if update && containerName != "" {
			// `coi shutdown` is graceful-stop-and-delete (per `coi --help`), so a
			// successful Shutdown leaves no container behind. Only fall through to
			// `coi container delete -f` when Shutdown actually failed — otherwise
			// we'd hit "instance not found" and surface it as an error.
			fmt.Printf("→ coi shutdown %s\n", containerName)
			if err := coi.Shutdown(containerName); err != nil {
				fmt.Fprintf(cobraOutErr(), "warn: coi shutdown: %v; falling back to force-delete\n", err)
				fmt.Printf("→ coi container delete -f %s\n", containerName)
				if err := coi.ContainerDelete(containerName); err != nil {
					return nil, "", fmt.Errorf("container delete: %w", err)
				}
			}
			containerName = ""
		}

		if containerName == "" {
			// First-shell: run COI's session-setup pipeline (mounts, claude
			// config push, sandbox injection) without launching claude, then
			// merge ahjo's claude prompt-suppressors into the just-populated
			// /home/code/.claude/{settings,.}.json so the user's first claude
			// invocation skips the trust + bypass dialogs.
			if err := coi.Setup(w.WorktreePath, slot); err != nil {
				return nil, "", fmt.Errorf("coi setup: %w", err)
			}
			containerName, err = coi.ResolveContainer(w.Slug, slot)
			if err != nil {
				return nil, "", err
			}
			if containerName == "" {
				return nil, "", fmt.Errorf("coi setup completed but no container registered for alias %q at slot %d", w.Slug, slot)
			}
			// COI's Lima auto-detect skips raw.idmap (it assumes the workspace
			// is virtiofs-backed; ahjo's worktrees aren't). Apply it ourselves
			// so the workspace surfaces inside the container as code:code
			// instead of nobody:nogroup. Requires the container be stopped:
			// raw.idmap is honored at next start.
			if err := applyRawIdmap(containerName); err != nil {
				return nil, "", err
			}
			repo := reg.FindRepo(w.Repo)
			if repo == nil {
				return nil, "", fmt.Errorf("internal: worktree %q references missing repo %q", alias, w.Repo)
			}
			// Bind-mount the bare repo at its absolute VM path so
			// /workspace/.git's gitdir: pointer resolves inside the container.
			if err := incus.AddDiskDevice(containerName, "ahjo-bare", repo.BarePath, repo.BarePath, false); err != nil {
				return nil, "", fmt.Errorf("add ahjo-bare disk device: %w", err)
			}
			if err := coi.ContainerExecAs(containerName, 1000, "/usr/local/bin/ahjo-claude-prepare"); err != nil {
				return nil, "", fmt.Errorf("ahjo-claude-prepare: %w", err)
			}
		}
	}

	// Ensure ssh proxy + sshd are wired before attach.
	if err := coi.ContainerStart(containerName); err != nil {
		return nil, "", err
	}
	if err := incus.AddProxyDevice(
		containerName, "ahjo-ssh",
		fmt.Sprintf("tcp:127.0.0.1:%d", w.SSHPort),
		"tcp:127.0.0.1:22",
	); err != nil {
		return nil, "", err
	}
	if err := coi.ContainerExec(containerName, true, "systemctl", "start", "ssh"); err != nil {
		// non-fatal: maybe systemd not up yet, or already running
		fmt.Fprintf(cobraOutErr(), "warn: could not start sshd: %v\n", err)
	}

	// Auto-expose: reconcile proxy devices for any pre-existing listeners
	// (e.g. from a previously-started docker-compose). Best-effort — failures
	// here must not block the user from getting their shell.
	release, err := lockfile.Acquire()
	if err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: auto-expose skipped: %v\n", err)
	} else {
		if err := reconcileAutoExpose(cobraOut(), w); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: auto-expose: %v\n", err)
		}
		release()
	}

	return w, containerName, nil
}

// applyRawIdmap stops the container, sets the per-container raw.idmap that
// maps the in-VM host UID/GID onto the container's `code` user, and starts
// it back up. raw.idmap is honored at next start, so the stop/start cycle is
// what makes it take effect on a container coi.Setup just left running.
//
// See CONTAINER-ISOLATION.md "Workspace UID mapping" for why ahjo applies
// this itself rather than relying on COI.
func applyRawIdmap(containerName string) error {
	val := idmap.RawIdmapValue(os.Getuid(), os.Getgid())
	if err := incus.Stop(containerName); err != nil {
		return fmt.Errorf("stop %s before raw.idmap: %w", containerName, err)
	}
	if err := incus.ConfigSet(containerName, "raw.idmap", val); err != nil {
		return fmt.Errorf("set raw.idmap on %s: %w", containerName, err)
	}
	if err := coi.ContainerStart(containerName); err != nil {
		return fmt.Errorf("start %s after raw.idmap: %w", containerName, err)
	}
	return nil
}
