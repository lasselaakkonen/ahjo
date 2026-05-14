package cli

import (
	"github.com/lasselaakkonen/ahjo/internal/ide"
	"github.com/lasselaakkonen/ahjo/internal/tui/top"
)

// idesForTop is the Deps hook that powers the `i` picker in `ahjo top`.
// Bare-Linux only: probes PATH via internal/ide and returns picker entries
// whose Open invokes the local CLI shim. On the Mac, the shim hands the
// TUI its own platform-specific Deps and this function is never called.
func idesForTop() []top.IDE {
	slugs := ide.DetectInstalled()
	out := make([]top.IDE, 0, len(slugs))
	for _, slug := range slugs {
		slug := slug
		out = append(out, top.IDE{
			Name: ide.DisplayName(slug),
			Open: func(host, path string) error {
				return ide.LaunchOnHost(slug, host, path)
			},
		})
	}
	return out
}
