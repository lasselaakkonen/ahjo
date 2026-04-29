package cli

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func newSSHCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ssh <repo> <branch>",
		Short: "exec ssh into the worktree's container via the generated ssh-config",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return runSSH(args[0], args[1])
		},
	}
}

func runSSH(repoName, branch string) error {
	reg, err := registry.Load()
	if err != nil {
		return err
	}
	w := reg.FindWorktree(repoName, branch)
	if w == nil {
		return fmt.Errorf("no worktree for %s/%s", repoName, branch)
	}
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not on PATH: %w", err)
	}
	host := "ahjo-" + w.Slug
	return syscall.Exec(bin, []string{"ssh", "-F", paths.SSHConfigPath(), host}, os.Environ())
}
