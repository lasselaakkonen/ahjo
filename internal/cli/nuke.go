package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/coi"
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
		Short: "Tear down all containers, images, worktrees, and caches; keep configs",
		Long: `nuke removes everything ahjo built so a fresh 'ahjo init' can rebuild:

  - stops and deletes every worktree container
  - deletes the ahjo-base and coi-default Incus images
  - removes ~/.ahjo/{worktrees,host-keys}
  - clears worktree entries from registry.toml and port allocations from ports.json
  - regenerates ~/.ahjo-shared/ssh-config

It KEEPS:
  - ~/.ahjo/{config.toml,profiles,repos}
  - registered repo entries in registry.toml
  - ~/.coi/

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

	for _, w := range reg.Worktrees {
		name, err := resolveContainerName(&w)
		if err != nil {
			fmt.Fprintf(cobraOutErr(), "note: skipping container ops for %s: %v\n", w.Slug, err)
			continue
		}
		fmt.Printf("→ coi shutdown %s\n", name)
		if err := coi.Shutdown(name); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: coi shutdown %s: %v\n", name, err)
		}
		fmt.Printf("→ coi container delete -f %s\n", name)
		if err := coi.ContainerDelete(name); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: container delete %s: %v\n", name, err)
		}
	}

	for _, alias := range []string{"ahjo-base", "coi-default"} {
		fmt.Printf("→ incus image delete %s\n", alias)
		out, err := exec.Command("incus", "image", "delete", alias).CombinedOutput()
		if err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: incus image delete %s: %v: %s\n", alias, err, out)
		}
	}

	for _, d := range []string{paths.WorktreesDir(), paths.HostKeysDir()} {
		fmt.Printf("→ rm -rf %s\n", d)
		if err := os.RemoveAll(d); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: rm %s: %v\n", d, err)
		}
	}

	reg.Worktrees = nil
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
	if len(reg.Worktrees) == 0 {
		fmt.Println("  - no worktree containers tracked in the registry")
	} else {
		for _, w := range reg.Worktrees {
			name, err := resolveContainerName(&w)
			if err != nil {
				fmt.Printf("  - (no container) remove worktree %s\n", w.WorktreePath)
				continue
			}
			fmt.Printf("  - delete container %s and worktree %s\n", name, w.WorktreePath)
		}
	}
	fmt.Println("  - delete incus images: ahjo-base, coi-default")
	fmt.Printf("  - remove %s\n", paths.WorktreesDir())
	fmt.Printf("  - remove %s\n", paths.HostKeysDir())
	fmt.Printf("  - clear worktrees from %s and allocations from %s\n",
		paths.RegistryPath(), paths.PortsPath())
	fmt.Println("It KEEPS: registered repos, config.toml, profiles, ~/.ahjo/repos, ~/.coi/")
	fmt.Println("Re-run with -y to proceed.")
}
