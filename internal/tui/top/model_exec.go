package top

import (
	"bytes"
	"os"
	"os/exec"

	tea "charm.land/bubbletea/v2"
)

// execAhjo runs a subprocess of this binary with the given args, suspending
// the TUI until it exits. Used for repo/worktree create/remove so the user
// sees the underlying CLI's progress output unfiltered.
func execAhjo(action, label string, args ...string) tea.Cmd {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = os.Args[0]
	}
	cmd := exec.Command(self, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return actionDoneMsg{action: action, label: label, err: err}
	})
}

// execAhjoCaptured runs a subprocess of this binary in-process (no
// alt-screen exit), capturing combined stdout+stderr so the result text can
// be shown in the TUI's flash line. Use this for short-lived, non-interactive
// commands where the alt-screen flash is more annoying than the loss of live
// progress (e.g. `ahjo mirror off`, whose teardown/revert needs no prompt).
func execAhjoCaptured(action, label string, args ...string) tea.Cmd {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = os.Args[0]
	}
	return func() tea.Msg {
		cmd := exec.Command(self, args...)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		runErr := cmd.Run()
		return actionDoneMsg{action: action, label: label, err: runErr, output: buf.String()}
	}
}

func (m *model) execRepoAdd(url string) tea.Cmd {
	return execAhjo("add", url, "repo", "add", url)
}

func (m *model) execRepoRm() tea.Cmd {
	repo := selectedRepo(m.repos)
	if repo == nil {
		return nil
	}
	alias := repo.Aliases[0]
	if hasBranches(m.snap, repo.Name) {
		m.flash = "repo " + alias + " has branches; remove them first"
		return nil
	}
	m.flash = "removing " + alias + "…"
	return execAhjo("removed", alias, "repo", "rm", alias)
}

func (m *model) execNewContainer(repoAlias, branch string) tea.Cmd {
	label := repoAlias + "@" + branch
	return execAhjo("created", label, "create", repoAlias, branch)
}

func (m *model) execWorktreeRm() tea.Cmd {
	w := selectedBranch(m.containers)
	if w == nil {
		return nil
	}
	alias := w.Aliases[0]
	// Removing a default-branch container also drops the repo row + PAT and
	// strands sibling branches (the COW source is gone). There's no way back
	// short of a full `ahjo repo add`, so refuse here and point the user at
	// the repo-column `r` binding instead of bothering with a force prompt.
	if w.IsDefault {
		m.flash = "can't remove default-branch container in isolation; focus the repo column and press 'r' to remove the repo"
		return nil
	}
	m.flash = "removing " + alias + "…"
	return execAhjo("removed", alias, "rm", alias)
}

// execStartStop dispatches StartStop for the selected container. The Deps
// implementation decides whether to start or stop based on the current
// lifecycle state; this layer just routes the result onto the flash line.
func (m *model) execStartStop() tea.Cmd {
	w := selectedBranch(m.containers)
	if w == nil {
		return nil
	}
	if m.deps.StartStop == nil {
		m.flash = "start/stop: not wired"
		return nil
	}
	wt := *w
	return func() tea.Msg {
		out, err := m.deps.StartStop(&wt)
		return toggleResultMsg{action: "start/stop", msg: out, err: err}
	}
}

func (m *model) execToggleExpose() tea.Cmd {
	w := selectedBranch(m.containers)
	if w == nil {
		return nil
	}
	if m.deps.ToggleExpose == nil {
		m.flash = "toggle expose: not wired"
		return nil
	}
	wt := *w
	return func() tea.Msg {
		out, err := m.deps.ToggleExpose(&wt)
		return toggleResultMsg{action: "toggle expose", msg: out, err: err}
	}
}
