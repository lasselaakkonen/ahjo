package cli

import (
	"github.com/lasselaakkonen/ahjo/internal/terminal"
	"github.com/lasselaakkonen/ahjo/internal/tui/top"
)

// terminalsForTop is the Deps hook that backs the `s`/`a` run-target picker
// in `ahjo top`. Bare-Linux only: probes PATH via internal/terminal and
// returns picker entries whose Run invokes LaunchCommand. On the Mac the
// shim wires its own macTerminals() instead.
func terminalsForTop() []top.Terminal {
	slugs := terminal.DetectInstalled()
	cur, hasCur := terminal.Current()
	out := make([]top.Terminal, 0, len(slugs))
	for _, slug := range slugs {
		slug := slug
		out = append(out, top.Terminal{
			Name:      terminal.DisplayName(slug),
			IsCurrent: hasCur && slug == cur,
			Run: func(argv []string, asTab bool) error {
				return terminal.LaunchCommand(slug, argv, asTab)
			},
		})
	}
	return out
}
