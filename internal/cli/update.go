package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/devcontainer"
	"github.com/lasselaakkonen/ahjo/internal/initflow"
)

// newUpdateCmd is the in-VM half of `ahjo update`. The macOS shim handles its
// own work (resolve the matching ahjo-linux-<arch>, push it into the VM at
// /usr/local/bin/ahjo) and then relays here so this side refreshes everything
// downstream of the host binary: claude on the VM, and the ahjo-base image
// (which bakes claude prep, sshd, and Node into containers).
func newUpdateCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Refresh the in-VM stack: claude, ahjo-base image (devcontainer Feature pipeline)",
		Long: `update brings every layer downstream of the host ahjo binary up to date:

  - re-runs Anthropic's claude installer on the VM (idempotent — pulls latest)
  - rebuilds the ahjo-base image by force-replaying the devcontainer
    Feature pipeline (curated upstream Features + ahjo's embedded
    Features) on top of images:ubuntu/24.04 (the local ahjo-osbase
    mirror of upstream is reused — re-pulling it would slow update for
    no gain)

It does not touch worktrees, containers, registry entries, or host claude
credentials. To pick up the new image in an existing container, recreate
it with 'ahjo shell <alias> --update'.

On macOS, the host-side 'ahjo update' first pushes the new ahjo-linux-<arch>
binary into the VM and then relays this command, so a single 'ahjo update'
on the Mac drives the whole chain.`,
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
		subuidGrantStep(),
		inotifySysctlStep(),
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
		{
			Title: "Rebuild ahjo-base via the devcontainer Feature pipeline",
			Note: "applies the curated upstream Features (common-utils, git, github-cli) " +
				"followed by ahjo's embedded ahjo-default-dev-tools (rg, fd, eza, yq, " +
				"ast-grep, httpie, make) and ahjo-runtime (sshd, ahjo-claude-prepare, " +
				"Node + corepack) to a fresh transient container off " + devcontainer.OSBaseAlias + ", " +
				"then publishes the result as ahjo-base. The local " + devcontainer.OSBaseAlias + " " +
				"mirror of " + devcontainer.UpstreamRemote + " is reused — re-pulling it would " +
				"slow update for no gain.",
			Show: "delete incus image alias '" + devcontainer.AhjoBaseAlias + "' (if present)\n" +
				"incus launch " + devcontainer.OSBaseAlias + " ahjo-build-<rand>\n" +
				"apply ghcr.io/devcontainers/features/{common-utils,git,github-cli}\n" +
				"apply embedded ahjo-default-dev-tools + ahjo-runtime\n" +
				"incus publish ahjo-build-<rand> --alias " + devcontainer.AhjoBaseAlias + "\n" +
				"incus delete ahjo-build-<rand>",
			Action: func(out io.Writer) error {
				return devcontainer.BuildAhjoBase(out, true)
			},
			Post: "\nDone. To pick up the new image in an existing container, recreate it:\n" +
				"  ahjo shell <alias> --update",
		},
	}
}
