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
		Use:   "ssh <alias>",
		Short: "exec ssh into the branch's container via the generated ssh-config",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runSSH(args[0])
		},
	}
}

func runSSH(alias string) error {
	br, err := resolveBranch(alias)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not on PATH: %w", err)
	}
	host := registry.ContainerName(br.Slug)
	return syscall.Exec(bin, []string{"ssh", "-F", paths.SSHConfigPath(), host}, os.Environ())
}
