// Package top implements the `ahjo top` Miller-columns TUI.
package top

import (
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

// Deps wires cli-package helpers into the TUI without an import cycle.
// All fields except the toggles are required; the toggles default to a
// "not implemented" flash when nil.
type Deps struct {
	ResolveContainerName func(*registry.Branch) (string, error)
	FormatExposed        func([]ports.Allocation) string
	HostStatus           func() HostStatus

	// ToggleExpose flips the branch between "all listening ports
	// exposed" and "no ports exposed". Returns a status string for the
	// flash line. Quiet (no stdout writes), so it can run inline in the
	// TUI process. Mirror-toggle, by contrast, prints progress and is
	// run as an `ahjo mirror` subprocess via tea.ExecProcess instead.
	ToggleExpose func(*registry.Branch) (string, error)
}

// HostStatus is the right-pane content shown when no repo/branch is selected.
// On macOS this is the Lima VM state; on Linux it's a short host blurb.
type HostStatus struct {
	Title string
	Lines []string
}
