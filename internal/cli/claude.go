package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/paths"
)

func newClaudeCmd() *cobra.Command {
	var update bool
	var force bool
	var containerConfig string
	cmd := &cobra.Command{
		Use:   "claude <alias>",
		Short: "Start (if needed) and launch `claude` inside the branch's container",
		Long: `Start the container if needed, wire SSH proxy + sshd, then ` +
			"`incus exec --force-interactive`" + ` directly into ` + "`claude`" + ` as the
in-container ` + "`ubuntu`" + ` user in /repo. Use ` + "`ahjo shell`" + ` for an interactive
shell instead.

Pass --update to discard the existing container before attaching: ahjo stops
it, deletes it, and recreates it from the repo's default-branch container.

Before recreating with --update, ahjo inspects /repo for uncommitted/unpushed
work (starting a stopped container for the check, after prompting). If /repo
is dirty — or the user declines the start prompt — the command refuses to
proceed; pass --force to skip the check and recreate anyway.

` + containerConfigHelpBlock,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClaude(cmd.Context(), args[0], update, force, containerConfig)
		},
	}
	cmd.Flags().BoolVar(&update, "update", false, "destroy the existing container before attaching so it picks up the current ahjo-base image")
	cmd.Flags().BoolVar(&force, "force", false, "with --update, skip the /repo cleanliness check and recreate even when uncommitted/unpushed work is present")
	cmd.Flags().StringVar(&containerConfig, "container-config", "", containerConfigFlagShort)
	return cmd
}

func runClaude(ctx context.Context, alias string, update, force bool, containerConfig string) error {
	br, containerName, err := prepareBranchContainer(ctx, alias, update, force, containerConfig)
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
	if err := runPostAttach(ctx, containerName, dcConf, env); err != nil {
		return err
	}
	// Refresh the ahjo-state snapshots so this session's first on-demand read
	// (per AHJO.md) and the statusline's first tick reflect current bridge state.
	refreshAhjoState(alias)
	// Launch through `bash -lc 'exec claude …'` so ~/.profile fires and
	// ~/.local/bin lands on PATH — otherwise claude's self-check ("native
	// install exists but ~/.local/bin not in PATH") prints on every start.
	// `exec` replaces bash with claude so signals + exit codes still pass
	// through unchanged.
	code, err := incus.ExecAttachWait(containerName, 1000, env, paths.RepoMountPath,
		"bash", "-lc", `exec claude --dangerously-skip-permissions`)
	showPostAttachStatus(br, containerName)
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
