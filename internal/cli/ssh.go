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
	reg, err := registry.Load()
	if err != nil {
		return err
	}
	br := reg.FindBranchByAlias(alias)
	if br == nil {
		return fmt.Errorf("no branch with alias %q", alias)
	}
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not on PATH: %w", err)
	}
	host := "ahjo-" + br.Slug
	return syscall.Exec(bin, []string{"ssh", "-F", paths.SSHConfigPath(), host}, os.Environ())
}
