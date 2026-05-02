package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/coi"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func newShellCmd() *cobra.Command {
	var update bool
	cmd := &cobra.Command{
		Use:   "shell <alias>",
		Short: "Start (if needed) and attach to the worktree's container via `coi shell`",
		Long: `Start the container if needed, wire SSH proxy + sshd, attach via ` + "`coi shell`" + `.

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
	reg, err := registry.Load()
	if err != nil {
		return err
	}
	w := reg.FindWorktreeByAlias(alias)
	if w == nil {
		return fmt.Errorf("no worktree with alias %q; create with `ahjo new`", alias)
	}

	// COI names containers "coi-<hash>-<slot>" where the hash is derived from
	// the workspace path; the .coi/config.toml `alias` is just a label. Resolve
	// alias+slot -> the real incus name via `coi list --format json`. An empty
	// result means COI hasn't created the container yet, so we trigger setup.
	const slot = 1
	containerName, err := coi.ResolveContainer(w.Slug, slot)
	if err != nil {
		return err
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
				return fmt.Errorf("container delete: %w", err)
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
			return fmt.Errorf("coi setup: %w", err)
		}
		containerName, err = coi.ResolveContainer(w.Slug, slot)
		if err != nil {
			return err
		}
		if containerName == "" {
			return fmt.Errorf("coi setup completed but no container registered for alias %q at slot %d", w.Slug, slot)
		}
		if err := coi.ContainerExecAs(containerName, 1000, "/usr/local/bin/ahjo-claude-prepare"); err != nil {
			return fmt.Errorf("ahjo-claude-prepare: %w", err)
		}
	}

	// Ensure ssh proxy + sshd are wired before attach.
	if err := coi.ContainerStart(containerName); err != nil {
		return err
	}
	if err := incus.AddProxyDevice(
		containerName, "ahjo-ssh",
		fmt.Sprintf("tcp:127.0.0.1:%d", w.SSHPort),
		"tcp:127.0.0.1:22",
	); err != nil {
		return err
	}
	if err := coi.ContainerExec(containerName, true, "systemctl", "start", "ssh"); err != nil {
		// non-fatal: maybe systemd not up yet, or already running
		fmt.Fprintf(cobraOutErr(), "warn: could not start sshd: %v\n", err)
	}
	return coi.ExecShell(w.WorktreePath, containerName)
}
