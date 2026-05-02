package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/coi"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/initflow"
	"github.com/lasselaakkonen/ahjo/internal/lima"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/profile"
)

// newUpdateCmd is the in-VM half of `ahjo update`. The macOS shim handles its
// own work (resolve the matching ahjo-linux-<arch>, push it into the VM at
// /usr/local/bin/ahjo) and then relays here so this side refreshes everything
// downstream of the host binary: claude on the VM, COI itself, the coi-default
// image (which is what bakes claude into containers), and ahjo-base.
func newUpdateCmd() *cobra.Command {
	var yes bool
	var buildCOI bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Refresh the in-VM stack: claude, COI, coi-default image, ahjo-base image",
		Long: `update brings every layer downstream of the host ahjo binary up to date:

  - re-runs Anthropic's claude installer on the VM (idempotent — pulls latest)
  - re-runs COI's install.sh (refreshes the coi binary itself)
  - rebuilds the coi-default image (this is where claude is baked into
    containers; without rebuilding it, container-side claude stays frozen at
    whatever version was current the last time coi-default was built)
  - re-materializes ~/.ahjo/profiles/ahjo-base/ from the embedded assets and
    rebuilds the ahjo-base image on top of the fresh coi-default

It does not touch worktrees, containers, registry entries, host claude
credentials, or COI's network config. To pick up the new images in an existing
container, recreate it with 'ahjo shell <alias> --update'.

On macOS, the host-side 'ahjo update' first pushes the new ahjo-linux-<arch>
binary into the VM and then relays this command, so a single 'ahjo update'
on the Mac drives the whole chain.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !insideLinuxVM() {
				return fmt.Errorf("the in-VM phase of `ahjo update` only runs on Linux; on macOS the same `ahjo update` first pushes the new binary into the VM and then relays this command")
			}
			r := initflow.Runner{Yes: yes, In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
			return r.Execute(vmUpdateSteps(buildCOI))
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the per-step confirmation prompt")
	cmd.Flags().BoolVar(&buildCOI, "build-coi", false, "build COI from source instead of downloading the pre-built binary")
	return cmd
}

func vmUpdateSteps(buildCOI bool) []initflow.Step {
	onLima := lima.IsGuest()
	return []initflow.Step{
		{
			Title: "Refresh Claude Code on the VM (idempotent)",
			Note:  "re-runs Anthropic's installer; it's a no-op if already current. Note: Anthropic's installer also auto-updates in the background, so this is mainly belt-and-suspenders.",
			Show:  "bash -lc 'curl -fsSL https://claude.ai/install.sh | bash'",
			Action: func(out io.Writer) error {
				if err := initflow.RunShell(out, "", "bash", "-lc", "curl -fsSL https://claude.ai/install.sh | bash"); err != nil {
					return fmt.Errorf("install.sh: %w", err)
				}
				return nil
			},
		},
		coiReinstallStep(onLima, buildCOI),
		{
			Title: "Rebuild coi-default image (~5 min — this is what refreshes claude inside containers)",
			Note:  "claude is installed into the coi-default image at build time (COI's profiles/default/build.sh runs `curl … claude.ai/install.sh | bash`). Without rebuilding this image, every new container inherits whatever claude version was current the last time coi-default was built.",
			Show: "delete incus image alias 'coi-default' (if present)\n" +
				"coi build  (default profile, from an empty temp dir so COI uses its embedded build.sh)",
			Action: func(out io.Writer) error {
				if err := incus.DeleteImageAlias("coi-default"); err != nil {
					return err
				}
				fmt.Fprintln(out, "  → coi-default alias cleared (or already absent)")
				return runCoiBuildDefault(out)
			},
		},
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
			Post: "\nDone. To pick up the new claude (and other refreshed deps) in an existing container, recreate it:\n" +
				"  ahjo shell <alias> --update",
		},
	}
}
