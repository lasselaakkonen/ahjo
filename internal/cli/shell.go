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

	// Best-effort: if the COI-managed container exists by alias, ensure ssh proxy
	// + sshd are wired before attach. coi-managed names are typically
	// "<alias>-<slot>" — slot 1 is the default. Look up by alias prefix.
	containerName := w.Slug + "-1"
	if exists, err := incus.ContainerExists(containerName); err == nil && exists {
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
	}
	// Whether the container existed or not, hand off to coi shell.
	return coi.ExecShell(w.WorktreePath)
}
