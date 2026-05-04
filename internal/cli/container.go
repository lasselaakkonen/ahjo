package cli

import (
	"fmt"

	"github.com/lasselaakkonen/ahjo/internal/coi"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

// resolveContainerName returns the live Incus container name for w.
//
// Mirrors the read-only branch of shell.go's prepareWorktreeContainer:
// trust w.IncusName for COW worktrees (set by `ahjo new` after `incus
// copy`); fall back to coi.ResolveContainer for COI-managed ones (whose
// real name is `coi-<hash>-<slot>`, only resolvable via `coi list`).
//
// Errors when neither resolves so callers fail loudly instead of operating
// on a synthesized `<slug>-1` name that never matches reality.
//
// Deliberately does not run `coi.Setup` — that's shell.go's job. Callers
// like `ahjo expose`, `ahjo ls`, `ahjo rm` must operate on existing
// containers only.
func resolveContainerName(w *registry.Worktree) (string, error) {
	if w.IncusName != "" {
		return w.IncusName, nil
	}
	name, err := coi.ResolveContainer(w.Slug, 1)
	if err != nil {
		return "", err
	}
	if name == "" {
		alias := w.Slug
		if len(w.Aliases) > 0 {
			alias = w.Aliases[0]
		}
		return "", fmt.Errorf("no container found for worktree %q (slug %q); start one with `ahjo shell %s`", alias, w.Slug, alias)
	}
	return name, nil
}
