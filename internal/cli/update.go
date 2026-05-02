package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/coi"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/initflow"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/profile"
)

// newUpdateCmd is the in-VM half of `ahjo update`. The macOS shim handles its
// own work (resolve the matching ahjo-linux-<arch>, push it into the VM at
// /usr/local/bin/ahjo) and then relays here so this side only has to refresh
// the parts that live in-VM.
func newUpdateCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Re-materialize the embedded ahjo-base profile and rebuild the ahjo-base image",
		Long: `update brings VM-side state in line with the current host binary:

  - re-materializes ~/.ahjo/profiles/ahjo-base/ from the embedded assets
  - deletes the existing ahjo-base image alias (if any)
  - rebuilds it via 'coi build --profile ahjo-base'

It does not touch worktrees, containers, registry entries, or COI itself.
Run this whenever you've edited the embedded ahjo-base profile (build.sh,
config.toml, or the ahjo-claude-prepare script). To pick up the new image
in an existing container, recreate it with 'ahjo shell <alias> --update'.

On macOS, the host-side 'ahjo update' first pushes the new ahjo-linux-<arch>
binary into the VM and then relays this command, so a single 'ahjo update'
on the Mac suffices.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !insideLinuxVM() {
				return fmt.Errorf("the in-VM phase of `ahjo update` only runs on Linux; on macOS the same `ahjo update` first pushes the new binary into the VM and then relays this command")
			}
			r := initflow.Runner{Yes: yes, In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
			return r.Execute(vmUpdateSteps())
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the per-step confirmation prompt")
	return cmd
}

func vmUpdateSteps() []initflow.Step {
	return []initflow.Step{
		{
			Title: "Rebuild ahjo-base from the embedded profile",
			Show: "delete incus image alias 'ahjo-base' (if present)\n" +
				"re-materialize ~/.ahjo/profiles/ahjo-base/{config.toml,build.sh} from embedded assets\n" +
				"coi build --profile ahjo-base",
			Action: func(out io.Writer) error {
				if err := paths.EnsureSkeleton(); err != nil {
					return err
				}
				if err := profile.Materialize(); err != nil {
					return err
				}
				if err := incus.DeleteImageAlias("ahjo-base"); err != nil {
					return err
				}
				fmt.Fprintln(out, "  → ahjo-base alias cleared (or already absent)")
				return coi.Build(paths.AhjoBaseProfile, false)
			},
			Post: "\nDone. To pick the new image up in an existing container, recreate it:\n" +
				"  ahjo shell <alias> --update",
		},
	}
}
