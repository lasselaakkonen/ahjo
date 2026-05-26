package cli

import (
	"fmt"

	"github.com/lasselaakkonen/ahjo/internal/registry"
)

// resolveBranch loads the registry and returns the branch registered under
// alias, or a "no branch" error. Collapses the Load → FindBranchByAlias →
// nil-check trio that every branch-scoped subcommand (ssh/forward/expose/ide/…)
// repeated verbatim. Callers that also need the *Registry (e.g. to look up the
// parent repo) keep their own Load.
func resolveBranch(alias string) (*registry.Branch, error) {
	reg, err := registry.Load()
	if err != nil {
		return nil, err
	}
	br := reg.FindBranchByAlias(alias)
	if br == nil {
		return nil, fmt.Errorf("no branch with alias %q", alias)
	}
	return br, nil
}

// resolveBranchContainer extends resolveBranch with the container-name
// resolution that the incus-touching commands (forward/expose/…) need before
// they can issue a device or exec call.
func resolveBranchContainer(alias string) (*registry.Branch, string, error) {
	br, err := resolveBranch(alias)
	if err != nil {
		return nil, "", err
	}
	name, err := resolveContainerName(br)
	if err != nil {
		return nil, "", err
	}
	return br, name, nil
}

// resolveContainerName returns the Incus container name for br.
//
// Phase 1 onwards: every branch container is launched via `incus launch`,
// so IncusName is always set. The function exists for callers that want a
// loud error when a registry row is somehow missing the field instead of
// silently using an empty string.
func resolveContainerName(br *registry.Branch) (string, error) {
	if br.IncusName != "" {
		return br.IncusName, nil
	}
	alias := br.Slug
	if len(br.Aliases) > 0 {
		alias = br.Aliases[0]
	}
	return "", fmt.Errorf("registry row for %q (slug %q) has no incus_name; recreate with `ahjo rm %s && ahjo create`", alias, br.Slug, alias)
}
