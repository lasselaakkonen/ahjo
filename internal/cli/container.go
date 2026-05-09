package cli

import (
	"fmt"

	"github.com/lasselaakkonen/ahjo/internal/registry"
)

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
	return "", fmt.Errorf("registry row for %q (slug %q) has no incus_name; recreate with `ahjo rm %s && ahjo new`", alias, br.Slug, alias)
}
