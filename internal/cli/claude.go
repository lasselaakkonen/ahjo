package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/paths"
)

func newClaudeCmd() *cobra.Command {
	var update bool
	cmd := &cobra.Command{
		Use:   "claude <alias>",
		Short: "Start (if needed) and launch `claude` inside the branch's container",
		Long: `Start the container if needed, wire SSH proxy + sshd, then ` +
			"`incus exec --force-interactive`" + ` directly into ` + "`claude`" + ` as the
in-container ` + "`ubuntu`" + ` user in /repo. Use ` + "`ahjo shell`" + ` for an interactive
shell instead.

Pass --update to discard the existing container before attaching: ahjo stops
it, deletes it, and recreates it from the repo's default-branch container.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runClaude(args[0], update)
		},
	}
	cmd.Flags().BoolVar(&update, "update", false, "destroy the existing container before attaching so it picks up the current ahjo-base image")
	return cmd
}

func runClaude(alias string, update bool) error {
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
	return incus.ExecAttach(containerName, 1000, env, paths.RepoMountPath, "claude")
}
