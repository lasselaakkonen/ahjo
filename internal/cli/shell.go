package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/coi"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func newShellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell <alias>",
		Short: "Start (if needed) and attach to the worktree's container via `coi shell`",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runShell(args[0])
		},
	}
}

func runShell(alias string) error {
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
