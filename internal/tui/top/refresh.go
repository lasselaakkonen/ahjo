package top

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

const tickInterval = 2 * time.Second

type tickMsg time.Time

type snapshotMsg struct {
	snap Snapshot
	err  error
}

// Snapshot is the per-tick view of registry + container + ports state that
// the TUI renders against. It crosses the Mac→VM boundary as JSON: the Mac
// shim's Deps fetches a Snapshot via `limactl shell ahjo ahjo top-state`,
// while the in-VM Deps builds one in-process. The `Host` field is filled in
// platform-locally on the Mac side (the VM doesn't know the host's Lima
// state), so it's never serialized.
type Snapshot struct {
	Repos         []registry.Repo               `json:"repos"`
	Branches      []registry.Branch             `json:"branches"`
	Containers    map[string]bool               `json:"containers"`
	PortsByBranch map[string][]ports.Allocation `json:"ports_by_branch"`
	Host          HostStatus                    `json:"-"`
	MirrorSlug    string                        `json:"mirror_slug,omitempty"`
	MirrorAlive   bool                          `json:"mirror_alive"`
}

func tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// refreshCmd defers to deps.LoadSnapshot for the bulk of state (registry,
// containers, ports, mirror), then fills in Host platform-locally so the
// right-pane host blurb doesn't have to round-trip.
func refreshCmd(deps Deps) tea.Cmd {
	return func() tea.Msg {
		snap, err := deps.LoadSnapshot()
		if deps.HostStatus != nil {
			snap.Host = deps.HostStatus()
		}
		return snapshotMsg{snap: snap, err: err}
	}
}
