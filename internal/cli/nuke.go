package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	sshpkg "github.com/lasselaakkonen/ahjo/internal/ssh"
)

func newNukeCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "nuke",
		Short: "Tear down all containers, images, host-keys, and caches; keep configs",
		Long: `nuke removes everything ahjo built so a fresh 'ahjo init' can rebuild:

  - stops and deletes every branch container (default + per-branch)
  - deletes the ahjo-base and ahjo-osbase Incus images (and any leftover
    coi-default from a pre-Phase-1 install)
  - removes ~/.ahjo/host-keys
  - clears branch + repo entries from registry.toml (default containers
    are not recoverable without re-cloning) and port allocations
  - regenerates ~/.ahjo-shared/ssh-config

It KEEPS:
  - ~/.ahjo/{config.toml,profiles}

On macOS, 'ahjo nuke' is handled host-side: it tears down the Lima VM
(making in-VM state moot) and removes ~/.ahjo/cache.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runNuke(yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation; required to actually destroy state")
	return cmd
}

func runNuke(yes bool) error {
	reg, err := registry.Load()
	if err != nil {
		return err
	}

	if !yes {
		printNukePreview(reg)
		return nil
	}

	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	for _, br := range reg.Branches {
		name, err := resolveContainerName(&br)
		if err != nil {
			fmt.Fprintf(cobraOutErr(), "note: skipping container ops for %s: %v\n", br.Slug, err)
			continue
		}
		fmt.Printf("→ incus stop %s\n", name)
		if err := incus.Stop(name); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: incus stop %s: %v\n", name, err)
		}
		fmt.Printf("→ incus delete --force %s\n", name)
		if err := incus.ContainerDeleteForce(name); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: incus delete %s: %v\n", name, err)
		}
	}

	for _, alias := range []string{"ahjo-base", "ahjo-osbase", "coi-default"} {
		fmt.Printf("→ incus image delete %s\n", alias)
		out, err := exec.Command("incus", "image", "delete", alias).CombinedOutput()
		if err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: incus image delete %s: %v: %s\n", alias, err, out)
		}
	}

	for _, d := range []string{paths.HostKeysDir()} {
		fmt.Printf("→ rm -rf %s\n", d)
		if err := os.RemoveAll(d); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: rm %s: %v\n", d, err)
		}
	}

	reg.Branches = nil
	reg.Repos = nil
	if err := reg.Save(); err != nil {
		return fmt.Errorf("save registry: %w", err)
	}

	if pp, err := ports.Load(); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: load ports: %v\n", err)
	} else {
		pp.Allocations = nil
		if err := pp.Save(); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: save ports: %v\n", err)
		}
	}

	if err := sshpkg.RegenerateConfig(reg); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: regenerate ssh-config: %v\n", err)
	}

	fmt.Println("\nDone. Run `ahjo init` to rebuild.")
	return nil
}

func printNukePreview(reg *registry.Registry) {
	fmt.Println("ahjo nuke will:")
	if len(reg.Branches) == 0 {
		fmt.Println("  - no branch containers tracked in the registry")
	} else {
		for _, br := range reg.Branches {
			name, err := resolveContainerName(&br)
			if err != nil {
				fmt.Printf("  - (no container) drop branch %s\n", br.Aliases[0])
				continue
			}
			fmt.Printf("  - delete container %s\n", name)
		}
	}
	fmt.Println("  - delete incus images: ahjo-base, ahjo-osbase (and leftover coi-default if present)")
	fmt.Printf("  - remove %s\n", paths.HostKeysDir())
	fmt.Printf("  - clear branches + repos from %s and allocations from %s\n",
		paths.RegistryPath(), paths.PortsPath())
	fmt.Println("It KEEPS: config.toml, profiles")
	fmt.Println("Re-run with -y to proceed.")
}
