package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	sshpkg "github.com/lasselaakkonen/ahjo/internal/ssh"
)

func newRepoRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <alias>",
		Short: "Stop+delete every branch container in the repo (including the default), free ports, drop registry entries",
		Long: `Removes a repo end-to-end: every branch container in the repo (including the
default-branch container that 'repo add' created as the COW source) is stopped
and deleted, its SSH port is freed, host-keys are removed, the registry rows
are dropped, and ssh-config is regenerated.

If any non-default branch containers exist, the command refuses unless --force
is passed — those branches typically hold in-flight work.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRepoRm(args[0], force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "also delete non-default branch containers in this repo (loses any in-flight work in those branches)")
	return cmd
}

func runRepoRm(alias string, force bool) error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return err
	}
	repo := reg.FindRepoByAlias(alias)
	if repo == nil {
		return sweepUnmanagedContainers(reg, alias, force)
	}

	var defaultBranchKey string
	var nonDefaultKeys []string
	for _, b := range reg.Branches {
		if b.Repo != repo.Name {
			continue
		}
		if b.IsDefault {
			defaultBranchKey = b.Branch
		} else {
			nonDefaultKeys = append(nonDefaultKeys, b.Branch)
		}
	}
	if len(nonDefaultKeys) > 0 && !force {
		return fmt.Errorf("repo %q has %d branch container(s) besides default; pass --force to delete them too", repo.Aliases[0], len(nonDefaultKeys))
	}

	// Remove non-default branches first so the default-branch row is the
	// last write that also drops the repo row (see removeBranchLocked).
	for _, branchKey := range nonDefaultKeys {
		br := reg.FindBranch(repo.Name, branchKey)
		if br == nil {
			continue
		}
		if err := removeBranchLocked(reg, br, false); err != nil {
			return err
		}
	}

	slug := repo.Name
	if defaultBranchKey != "" {
		br := reg.FindBranch(slug, defaultBranchKey)
		if br != nil {
			if err := removeBranchLocked(reg, br, true); err != nil {
				return err
			}
			// removeBranchLocked already dropped the PAT via its wasDefault
			// path, but call again defensively — `repo rm` is the repo-level
			// owner of PAT lifecycle and shouldn't depend on the branch-level
			// helper getting it right. dropRepoToken is idempotent.
			dropRepoToken(slug)
			return sweepRepoOrphans(reg, slug, force)
		}
	}

	// Legacy state: repo row exists with no default-branch row (e.g. left
	// behind by the old registry-only repo rm). Best-effort: delete the
	// base container if its name is recorded, then drop the repo row.
	if name := repo.BaseContainerName; name != "" {
		fmt.Printf("→ incus delete --force %s\n", name)
		if err := incus.ContainerDeleteForce(name); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: incus delete %s: %v\n", name, err)
		}
	}
	// Drop the per-repo PAT (Linux: the .env on disk; Mac: a marker file the
	// shim sweeps post-relay against Keychain). Best-effort: a missing file
	// is fine; permission failures log but don't block the rest of cleanup.
	dropRepoToken(slug)
	reg.RemoveRepo(slug)
	if err := reg.Save(); err != nil {
		return err
	}
	if err := sshpkg.RegenerateConfig(reg); err != nil {
		return err
	}
	fmt.Printf("removed repo %s\n", repo.Aliases[0])
	return sweepRepoOrphans(reg, slug, force)
}

// sweepRepoOrphans deletes any `ahjo-<slug>*` container not registered to a
// branch in reg. Runs after a successful `repo rm` to mop up suffix-past-orphan
// leftovers from prior crashed `repo add` attempts (e.g. `ahjo-foo-2`,
// `ahjo-foo-3` when only `ahjo-foo` was registered — see
// internal/registry's slug allocator).
//
// With --force, deletes silently. Without --force, lists and prompts.
// Best-effort: a per-container delete failure logs a warn but doesn't abort
// the rest of the sweep.
func sweepRepoOrphans(reg *registry.Registry, slug string, force bool) error {
	orphans, err := findOrphanContainers(reg, slug)
	if err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: scan for orphan containers: %v\n", err)
		return nil
	}
	if len(orphans) == 0 {
		return nil
	}
	if !force {
		fmt.Printf("found unmanaged container(s) matching %s prefix (likely left by a past crashed `repo add`):\n", registry.ContainerName(slug))
		for _, name := range orphans {
			fmt.Printf("  %s\n", name)
		}
		if !promptYesNo("Delete them?") {
			return nil
		}
	}
	for _, name := range orphans {
		fmt.Printf("→ incus delete --force %s\n", name)
		if err := incus.ContainerDeleteForce(name); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: incus delete %s: %v\n", name, err)
		}
	}
	return nil
}

// findOrphanContainers returns container names matching `ahjo-<slug>` or
// `ahjo-<slug>-…` that aren't registered to any branch in reg. Used by both
// the `repo rm` tail sweep (after the registered repo is gone) and
// `sweepUnmanagedContainers` (no repo row exists at all).
func findOrphanContainers(reg *registry.Registry, slug string) ([]string, error) {
	candidates, err := incus.ContainersWithPrefix(registry.ContainerName(slug))
	if err != nil {
		return nil, err
	}
	registered := map[string]bool{}
	for _, b := range reg.Branches {
		if b.IncusName != "" {
			registered[b.IncusName] = true
		}
	}
	var orphans []string
	for _, name := range candidates {
		if !registered[name] {
			orphans = append(orphans, name)
		}
	}
	return orphans, nil
}

// sweepUnmanagedContainers handles the case where `repo rm <alias>` finds no
// registry entry but Incus still has a container that matches the slug — the
// signature of a `repo add` that crashed mid-flow after creating the container
// but before writing the registry row. Without --force we list the orphans and
// prompt; with --force we delete unconditionally. Caller holds the lockfile.
func sweepUnmanagedContainers(reg *registry.Registry, alias string, force bool) error {
	slug := registry.AliasToSlug(alias)
	if slug == "" {
		return fmt.Errorf("no repo with alias %q", alias)
	}
	orphans, err := findOrphanContainers(reg, slug)
	if err != nil {
		return err
	}
	if len(orphans) == 0 {
		return fmt.Errorf("no repo with alias %q", alias)
	}
	if !force {
		fmt.Printf("no managed repo with alias %q, but found unmanaged container(s):\n", alias)
		for _, name := range orphans {
			fmt.Printf("  %s\n", name)
		}
		if !promptYesNo("Delete them?") {
			return nil
		}
	}
	for _, name := range orphans {
		fmt.Printf("→ incus delete --force %s\n", name)
		if err := incus.ContainerDeleteForce(name); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: incus delete %s: %v\n", name, err)
		}
	}
	// Host keys for the base slug are deterministic from the alias. Branch
	// host-keys live under their own (unknown-to-us) slugs, so they stay.
	_ = os.RemoveAll(paths.SlugHostKeysDir(slug))
	// A crashed `repo add` may have written the PAT before the registry row;
	// drop it here so the orphan sweep leaves no plaintext token behind.
	dropRepoToken(slug)
	return nil
}

// repoContainerNames returns every container name owned by repoSlug — the
// default-branch container plus each branch's incus_name. Filters out
// empty entries (legacy registry rows that pre-date BaseContainerName).
func repoContainerNames(reg *registry.Registry, repoSlug string) []string {
	var out []string
	if r := reg.FindRepo(repoSlug); r != nil && r.BaseContainerName != "" {
		out = append(out, r.BaseContainerName)
	}
	for i := range reg.Branches {
		if reg.Branches[i].Repo != repoSlug {
			continue
		}
		name := reg.Branches[i].IncusName
		if name == "" {
			continue
		}
		// Skip the default-branch container we already added above (its
		// IncusName matches BaseContainerName).
		dup := false
		for _, existing := range out {
			if existing == name {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, name)
		}
	}
	return out
}
