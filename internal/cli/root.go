// Package cli wires every ahjo subcommand into a single cobra root.
package cli

import "github.com/spf13/cobra"

// NewRoot returns the cobra root with every subcommand attached.
func NewRoot(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "ahjo",
		Short:         "Manage sandboxed Claude Code branches across repos via coi/Incus",
		SilenceUsage:  true,
		SilenceErrors: false,
		Version:       version,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(
		newInitCmd(),
		newUpdateCmd(),
		newRepoCmd(),
		newNewCmd(),
		newShellCmd(),
		newSSHCmd(),
		newExposeCmd(),
		newLsCmd(),
		newRmCmd(),
		newGCCmd(),
		newDoctorCmd(),
		newNukeCmd(),
	)
	return root
}
