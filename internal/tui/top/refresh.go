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
	Repos             []registry.Repo   `json:"repos"`
	Branches          []registry.Branch `json:"branches"`
	Containers        map[string]bool   `json:"containers"`
	ContainersRunning map[string]bool   `json:"containers_running"`
	// ContainerStates carries the raw incus status string per branch slug
	// ("Running", "Stopped", "Frozen", ...). Empty / missing means the
	// container isn't registered with incus or the probe failed; the
	// details pane renders that as a red "unknown".
	ContainerStates map[string]string             `json:"container_states,omitempty"`
	PortsByBranch   map[string][]ports.Allocation `json:"ports_by_branch"`
	// ForwardsByBranch carries each branch's host→container forwards, read
	// live from the container's proxy devices. Forwards aren't tracked in
	// ports.json, so they can't be derived from PortsByBranch; the cli loader
	// reads them from incus and ships the structured pairs so the details pane
	// can guess a scheme and align the arrows itself. Absent entry means no
	// forwards (rendered as "-").
	ForwardsByBranch map[string][]Forward `json:"forwards_by_branch,omitempty"`
	Host             HostStatus           `json:"-"`
	MirrorSlug       string               `json:"mirror_slug,omitempty"`
	MirrorAlive      bool                 `json:"mirror_alive"`
}

// Forward is one host→container port forward: the container listens on
// Container and proxies to the host's loopback Host port. Shipped structured
// (rather than pre-formatted) so the details pane owns scheme-guessing and
// arrow alignment, matching how exposes are rendered from PortsByBranch.
type Forward struct {
	Container int `json:"container"`
	Host      int `json:"host"`
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
