package cli

import (
	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/coi"
)

func newClaudeCmd() *cobra.Command {
	var update bool
	cmd := &cobra.Command{
		Use:   "claude <alias>",
		Short: "Start (if needed) and launch `claude` inside the worktree's container",
		Long: `Start the container if needed, wire SSH proxy + sshd, attach via ` + "`coi shell`" + `
so COI's AI-tool launcher starts claude inside the container. Use ` + "`ahjo shell`" + `
for an interactive shell instead.

Pass --update to discard the existing container before attaching: ahjo shuts it
down, deletes it, and the regular first-shell path then re-creates it from the
current ahjo-base image (re-running ahjo-claude-prepare). The worktree, host
keys, registry entry, and ssh port are preserved.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runClaude(args[0], update)
		},
	}
	cmd.Flags().BoolVar(&update, "update", false, "destroy the existing container before attaching so it picks up the current ahjo-base image (keeps the worktree)")
	return cmd
}

func runClaude(alias string, update bool) error {
	w, containerName, err := prepareWorktreeContainer(alias, update)
	if err != nil {
		return err
	}
	return coi.ExecClaude(w.WorktreePath, containerName)
}
