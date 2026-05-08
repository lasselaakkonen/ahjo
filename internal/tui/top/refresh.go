package top

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/mirror"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

const tickInterval = 2 * time.Second

type tickMsg time.Time

type snapshotMsg struct {
	snap snapshot
	err  error
}

type snapshot struct {
	repos       []registry.Repo
	worktrees   []registry.Worktree
	containers  map[string]bool // worktree slug -> container exists
	ports       *ports.Ports
	host        HostStatus
	mirrorSlug  string // slug of the worktree currently mirroring, "" if none
	mirrorAlive bool   // mirror daemon is reachable (kill -0)
}

func tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func refreshCmd(deps Deps) tea.Cmd {
	return func() tea.Msg {
		snap, err := loadSnapshot(deps)
		return snapshotMsg{snap: snap, err: err}
	}
}

func loadSnapshot(deps Deps) (snapshot, error) {
	var snap snapshot
	reg, err := registry.Load()
	if err != nil {
		return snap, err
	}
	snap.repos = reg.Repos
	snap.worktrees = reg.Worktrees

	pp, err := ports.Load()
	if err != nil {
		return snap, err
	}
	snap.ports = pp

	snap.containers = make(map[string]bool, len(reg.Worktrees))
	for i := range reg.Worktrees {
		w := &reg.Worktrees[i]
		name, err := deps.ResolveContainerName(w)
		if err != nil {
			continue
		}
		exists, err := incus.ContainerExists(name)
		if err != nil {
			continue
		}
		snap.containers[w.Slug] = exists
	}

	if deps.HostStatus != nil {
		snap.host = deps.HostStatus()
	}
	if mst, _ := mirror.Load(); mst != nil {
		snap.mirrorSlug = mst.Slug
		snap.mirrorAlive = mirror.PIDAlive(mst.PID)
	}
	return snap, nil
}
