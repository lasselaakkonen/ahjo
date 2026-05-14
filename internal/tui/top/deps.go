// Package top implements the `ahjo top` Miller-columns TUI.
package top

import (
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

// Deps wires platform-specific helpers into the TUI without an import cycle.
// The TUI package stays platform-agnostic; each platform supplies its own
// Deps. The in-VM cli builds one with in-process registry/incus/ports calls;
// the Mac shim builds one that JSON-RPCs `limactl shell ahjo ahjo top-state`
// (and the equivalent for branch status) so the Mac can drive the TUI while
// the state of the world still lives in the VM.
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

	// IDEs enumerates SSH-capable IDEs the host has installed, in
	// preferred display order. Each entry knows how to launch itself
	// pointed at a given ssh host + remote path. Returns nil/empty when
	// nothing was detected — the picker surfaces a flash in that case.
	IDEs func() []IDE

	// LoadSnapshot fetches the per-tick registry/container/ports/mirror
	// state used to render the three columns. Linux-native: builds it
	// in-process. Mac: shells `ahjo top-state --json` into the VM and
	// unmarshals the result. Errors land on the snapshot load error
	// line; the previous snapshot is left in place.
	LoadSnapshot func() (Snapshot, error)

	// LoadBranchStatus fetches git+PR status for one branch slug. Same
	// platform split as LoadSnapshot: in-VM runs `git status` + `gh pr
	// list` inside the container; on the Mac the in-VM ahjo handles
	// both via the hidden `ahjo branch-status` subcommand.
	LoadBranchStatus func(slug string) (BranchStatus, error)
}

// IDE is one row in the `i` picker. Open runs the launcher non-blocking;
// the TUI surfaces any error via the flash line.
type IDE struct {
	Name string
	Open func(host, path string) error
}

// HostStatus is the right-pane content shown when no repo/branch is selected.
// On macOS this is the Lima VM state; on Linux it's a short host blurb.
type HostStatus struct {
	Title string
	Lines []string
}
