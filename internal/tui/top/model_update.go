package top

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.applySizes()
		// Re-fit the detail rows to the new pane width so truncation ellipses
		// track the resize (no-op while the details pane is focused, matching
		// refreshDetails' own guard).
		m.refreshDetails()
		return m, nil

	case tickMsg:
		cmds := []tea.Cmd{refreshCmd(m.deps), tickCmd()}
		if c := m.maybeRefreshBranchStatus(); c != nil {
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)

	case snapshotMsg:
		m.loadErr = msg.err
		m.snap = msg.snap
		m.repos.SetItems(repoItemsFrom(m.snap))
		if !m.restored {
			m.restored = true
			m.restoreSelection()
		}
		m.refreshContainers()
		m.refreshDetails()
		return m, tea.Batch(m.maybeRefreshBranchStatus(), m.maybeRefreshAhjoState())

	case branchStatusMsg:
		if m.branchStatus == nil {
			m.branchStatus = make(map[string]BranchStatus)
		}
		m.branchStatus[msg.slug] = msg.status
		delete(m.inFlightStatus, msg.slug)
		m.refreshContainers()
		m.refreshDetails()
		return m, nil

	case ahjoStateRefreshedMsg:
		if m.ahjoStateRefreshedAt == nil {
			m.ahjoStateRefreshedAt = make(map[string]time.Time)
		}
		m.ahjoStateRefreshedAt[msg.slug] = time.Now()
		delete(m.inFlightAhjoState, msg.slug)
		return m, nil

	case actionDoneMsg:
		if msg.err != nil {
			detail := lastNonEmptyLine(msg.output)
			if detail == "" {
				detail = msg.err.Error()
			}
			m.flash = msg.action + " " + msg.label + " failed: " + detail
		} else if detail := lastNonEmptyLine(msg.output); detail != "" {
			m.flash = detail
		} else if msg.label != "" {
			m.flash = msg.action + " " + msg.label
		} else {
			m.flash = msg.action
		}
		return m, refreshCmd(m.deps)

	case toggleResultMsg:
		if msg.err != nil {
			m.flash = msg.action + " failed: " + msg.err.Error()
		} else {
			m.flash = msg.msg
		}
		return m, refreshCmd(m.deps)

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.PasteMsg:
		// Bracketed-paste content arrives as its own message, not a run of
		// KeyPressMsgs, so it bypasses handleKey entirely. Route it to the
		// active text prompt; outside a prompt there's nowhere for pasted
		// text to go (the list panes have filtering disabled), so drop it.
		if m.textInputActive() {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	var cmd tea.Cmd
	switch m.focus {
	case focusRepos:
		m.repos, cmd = m.repos.Update(msg)
	case focusContainers:
		m.containers, cmd = m.containers.Update(msg)
	case focusDetails:
		m.details, cmd = m.details.Update(msg)
	}
	return m, cmd
}

func (m *model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.inputMode == inputIDE {
		return m.handleIDEPickerKey(msg)
	}
	if m.inputMode == inputRunTarget {
		return m.handleRunTargetPickerKey(msg)
	}
	if m.inputMode != inputNone {
		switch {
		case key.Matches(msg, m.keys.Submit):
			return m.submitInput()
		case key.Matches(msg, m.keys.Cancel):
			m.cancelInput()
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	// Global keys.
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.persistSelection()
		return m, tea.Quit
	case key.Matches(msg, m.keys.Refresh):
		m.flash = ""
		return m, refreshCmd(m.deps)
	case key.Matches(msg, m.keys.Left):
		m.shiftFocus(-1)
		return m, nil
	case key.Matches(msg, m.keys.Right):
		m.shiftFocus(+1)
		return m, nil
	}

	// Per-focus keys. Note both RemoveRepo and RemoveContainer bind "r";
	// dispatching by focus first avoids the ambiguity.
	switch m.focus {
	case focusRepos:
		switch {
		case key.Matches(msg, m.keys.AddRepo):
			m.startInput(inputAddRepo)
			return m, nil
		case key.Matches(msg, m.keys.RemoveRepo):
			return m, m.execRepoRm()
		case key.Matches(msg, m.keys.Submit):
			if it, ok := m.repos.SelectedItem().(repoItem); ok && it.kind == "new" {
				m.startInput(inputAddRepo)
				return m, nil
			}
		}
	case focusContainers, focusDetails:
		// Container-context shortcuts work the same whether focus is on the
		// containers list or on the details pane for that container.
		switch {
		case key.Matches(msg, m.keys.NewContainer):
			m.startInput(inputNewContainer)
			return m, nil
		case key.Matches(msg, m.keys.RemoveContainer):
			return m, m.execWorktreeRm()
		case key.Matches(msg, m.keys.ToggleExpose):
			return m, m.execToggleExpose()
		case key.Matches(msg, m.keys.ToggleMirror):
			return m.handleToggleMirror()
		case key.Matches(msg, m.keys.Forward):
			m.startInput(inputForward)
			return m, nil
		case key.Matches(msg, m.keys.StartStop):
			return m, m.execStartStop()
		case key.Matches(msg, m.keys.CopyClaudeCmd):
			m.startRunTargetPicker("claude")
			return m, nil
		case key.Matches(msg, m.keys.CopyShellCmd):
			m.startRunTargetPicker("shell")
			return m, nil
		case key.Matches(msg, m.keys.OpenIDE):
			m.startIDEPicker()
			return m, nil
		case key.Matches(msg, m.keys.Submit):
			if m.focus == focusContainers {
				if it, ok := m.containers.SelectedItem().(containerItem); ok && it.kind == "new" {
					m.startInput(inputNewContainer)
					return m, nil
				}
			}
		}
	}

	prevRepo := selectedRepoName(m.repos)
	prevWt := selectedBranchSlug(m.containers)

	var cmd tea.Cmd
	switch m.focus {
	case focusRepos:
		m.repos, cmd = m.repos.Update(msg)
	case focusContainers:
		m.containers, cmd = m.containers.Update(msg)
	case focusDetails:
		m.details, cmd = m.details.Update(msg)
	}

	if selectedRepoName(m.repos) != prevRepo {
		m.refreshContainers()
		m.refreshDetails()
		cmd = tea.Batch(cmd, m.maybeRefreshBranchStatus(), m.maybeRefreshAhjoState())
	} else if selectedBranchSlug(m.containers) != prevWt {
		m.refreshDetails()
		cmd = tea.Batch(cmd, m.maybeRefreshBranchStatus(), m.maybeRefreshAhjoState())
	}
	return m, cmd
}

// textInputActive reports whether a prompt backed by m.input is currently
// capturing input. The inputIDE / inputRunTarget pickers are list-style
// selection (no m.input), so they're deliberately excluded — pasted text
// means nothing to them.
func (m *model) textInputActive() bool {
	switch m.inputMode {
	case inputAddRepo, inputNewContainer, inputMirrorTarget, inputForward:
		return true
	default:
		return false
	}
}

func (m *model) startInput(mode inputMode) {
	var prefill string
	switch mode {
	case inputAddRepo:
		m.input.Placeholder = "owner/repo or git URL"
	case inputNewContainer:
		repo := selectedRepo(m.repos)
		if repo == nil {
			m.flash = "select a repo first"
			return
		}
		m.input.Placeholder = "branch (under " + repo.Aliases[0] + ")"
	case inputMirrorTarget:
		w := selectedBranch(m.containers)
		if w == nil {
			m.flash = "select a branch first"
			return
		}
		repo := findRepoByName(m.snap.Repos, w.Repo)
		if repo != nil && repo.MacMirrorTarget != "" {
			prefill = repo.MacMirrorTarget
		} else {
			prefill = m.startCwd
		}
		m.input.Placeholder = "absolute Mac path (or ~/...)"
	case inputForward:
		if selectedBranch(m.containers) == nil {
			m.flash = "select a branch first"
			return
		}
		m.input.Placeholder = "host-port [container-port]"
	}
	m.inputMode = mode
	m.input.Reset()
	if prefill != "" {
		m.input.SetValue(prefill)
		m.input.CursorEnd()
	}
	m.input.Focus()
	m.flash = ""
}

func (m *model) cancelInput() {
	m.inputMode = inputNone
	m.input.Reset()
	m.idePickerIDEs = nil
	m.idePickerIdx = 0
	m.idePickerHost = ""
	m.idePickerPath = ""
	m.runTargetTerms = nil
	m.runTargetIdx = 0
	m.runTargetSub = ""
	m.runTargetAlias = ""
}

// startIDEPicker enters inputIDE mode for the currently selected branch.
// Resolves the SSH host alias + remote path up-front so the launchers in
// the picker don't need branch context. Flashes (without switching modes)
// when no branch is selected or no IDEs were detected on the host.
func (m *model) startIDEPicker() {
	br := selectedBranch(m.containers)
	if br == nil {
		m.flash = "select a container first"
		return
	}
	if m.deps.IDEs == nil {
		m.flash = "ide picker not wired"
		return
	}
	ides := m.deps.IDEs()
	if len(ides) == 0 {
		m.flash = "no SSH-capable IDEs found on host"
		return
	}
	m.idePickerIDEs = ides
	m.idePickerIdx = 0
	m.idePickerHost = registry.ContainerName(br.Slug)
	m.idePickerPath = "/repo"
	m.inputMode = inputIDE
	m.flash = ""
}

// handleIDEPickerKey owns the keypress loop while inputIDE is active.
// Up/down navigate; enter launches; esc cancels. All other keys are
// dropped so an in-flight picker can't leak keys into the underlying
// list/viewport.
func (m *model) handleIDEPickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Cancel):
		m.cancelInput()
		return m, nil
	case key.Matches(msg, m.keys.Up):
		if m.idePickerIdx > 0 {
			m.idePickerIdx--
		}
		return m, nil
	case key.Matches(msg, m.keys.Down):
		if m.idePickerIdx < len(m.idePickerIDEs)-1 {
			m.idePickerIdx++
		}
		return m, nil
	case key.Matches(msg, m.keys.Submit):
		if m.idePickerIdx < 0 || m.idePickerIdx >= len(m.idePickerIDEs) {
			m.cancelInput()
			return m, nil
		}
		ide := m.idePickerIDEs[m.idePickerIdx]
		host, path := m.idePickerHost, m.idePickerPath
		m.cancelInput()
		if ide.Open == nil {
			m.flash = "ide " + ide.Name + ": no launcher"
			return m, nil
		}
		if err := ide.Open(host, path); err != nil {
			m.flash = "open " + ide.Name + " failed: " + err.Error()
			return m, nil
		}
		m.flash = "opening " + ide.Name + " → " + host + ":" + path
		return m, nil
	}
	return m, nil
}

func (m *model) submitInput() (tea.Model, tea.Cmd) {
	val := strings.TrimSpace(m.input.Value())
	if val == "" {
		m.cancelInput()
		return m, nil
	}
	switch m.inputMode {
	case inputAddRepo:
		m.flash = "adding " + val + "…"
		cmd := m.execRepoAdd(val)
		m.cancelInput()
		return m, cmd
	case inputNewContainer:
		repo := selectedRepo(m.repos)
		if repo == nil {
			m.cancelInput()
			return m, nil
		}
		alias := repo.Aliases[0]
		branch := git.SanitizeBranchName(val)
		if branch == "" {
			m.flash = fmt.Sprintf("branch %q has no usable characters", val)
			m.cancelInput()
			return m, nil
		}
		if branch != val {
			m.flash = fmt.Sprintf("creating %s@%s (sanitized from %q)…", alias, branch, val)
		} else {
			m.flash = "creating " + alias + "@" + branch + "…"
		}
		cmd := m.execNewContainer(alias, branch)
		m.cancelInput()
		return m, cmd
	case inputMirrorTarget:
		w := selectedBranch(m.containers)
		if w == nil {
			m.cancelInput()
			return m, nil
		}
		alias := w.Aliases[0]
		m.flash = "mirroring " + alias + " → " + val + "…"
		// execAhjo (tea.ExecProcess), not execAhjoCaptured: `mirror on` confirms
		// before clobbering a dirty/non-git target ("continue?" / "mirror without
		// the ability to revert?"). Those prompts need a real TTY on stdin — a
		// captured subprocess would silently cancel the activation. Suspending the
		// TUI also surfaces the daemon bootstrap progress live on the terminal.
		cmd := execAhjo("mirroring", alias, "mirror", alias, "--target", val)
		m.cancelInput()
		return m, cmd
	case inputForward:
		w := selectedBranch(m.containers)
		if w == nil {
			m.cancelInput()
			return m, nil
		}
		// "host-port" or "host-port container-port". Port values are validated
		// by the `ahjo forward` subprocess; its error tail surfaces on the
		// flash line, so we only guard the field count here.
		fields := strings.Fields(val)
		if len(fields) > 2 {
			m.flash = "forward: expected 'host-port [container-port]'"
			m.cancelInput()
			return m, nil
		}
		alias := w.Aliases[0]
		dest := alias
		if len(fields) == 2 {
			dest = alias + " :" + fields[1]
		}
		m.flash = "forwarding host :" + fields[0] + " → " + dest + "…"
		cmd := execAhjoCaptured("forward", alias, append([]string{"forward", alias}, fields...)...)
		m.cancelInput()
		return m, cmd
	}
	m.cancelInput()
	return m, nil
}

func (m *model) shiftFocus(delta int) {
	target := int(m.focus) + delta
	if target < int(focusRepos) || target > int(focusDetails) {
		return
	}
	m.setFocus(focus(target))
	m.refreshDetails()
}

// setFocus moves keyboard focus to f and syncs the delegate styling pointers.
// It does not refresh the details pane or persist — callers that change focus
// in response to user input do that themselves; restore wants neither.
func (m *model) setFocus(f focus) {
	m.focus = f
	*m.reposFocused = f == focusRepos
	*m.contFocused = f == focusContainers
}

// startRunTargetPicker enters inputRunTarget mode for the currently selected
// branch and the given subcommand ("claude" or "shell"). Captures the
// branch alias at entry so the picker isn't affected by later list moves.
// When no branch is selected, flashes without switching modes — mirrors
// startIDEPicker's behaviour. The picker always renders a "copy to
// clipboard" entry, so an empty Terminals() result is fine.
func (m *model) startRunTargetPicker(sub string) {
	w := selectedBranch(m.containers)
	if w == nil {
		m.flash = "select a container first"
		return
	}
	var terms []Terminal
	if m.deps.Terminals != nil {
		terms = m.deps.Terminals()
	}
	m.runTargetTerms = terms
	m.runTargetIdx = 0
	m.runTargetSub = sub
	m.runTargetAlias = w.Aliases[0]
	m.inputMode = inputRunTarget
	m.flash = ""
}

// handleRunTargetPickerKey owns the keypress loop while inputRunTarget is
// active. Up/down navigate the combined list (detected terminals followed
// by the clipboard sentinel); enter dispatches; esc cancels. All other
// keys are dropped so an in-flight picker can't leak keys into the
// underlying list/viewport.
func (m *model) handleRunTargetPickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	rows := len(m.runTargetTerms) + 1 // +1 for the clipboard sentinel
	switch {
	case key.Matches(msg, m.keys.Cancel):
		m.cancelInput()
		return m, nil
	case key.Matches(msg, m.keys.Up):
		if m.runTargetIdx > 0 {
			m.runTargetIdx--
		}
		return m, nil
	case key.Matches(msg, m.keys.Down):
		if m.runTargetIdx < rows-1 {
			m.runTargetIdx++
		}
		return m, nil
	case key.Matches(msg, m.keys.Submit):
		return m.dispatchRunTarget(true)
	case key.Matches(msg, m.keys.SubmitWindow):
		return m.dispatchRunTarget(false)
	}
	return m, nil
}

// dispatchRunTarget acts on the selected row: clipboard sentinel copies,
// terminal rows spawn. honorTab is true for plain Enter (use the term's
// own preference — tab for the current terminal, window for the rest) and
// false for Shift+Enter (force a new window regardless). Shift+Enter on
// the clipboard sentinel behaves the same as Enter — there's no "shift to
// copy differently" semantic.
func (m *model) dispatchRunTarget(honorTab bool) (tea.Model, tea.Cmd) {
	sub := m.runTargetSub
	alias := m.runTargetAlias
	idx := m.runTargetIdx
	terms := m.runTargetTerms
	m.cancelInput()
	cmdStr := "ahjo " + sub + " " + alias
	if idx >= len(terms) {
		m.flash = "copied to clipboard: " + cmdStr
		return m, tea.SetClipboard(cmdStr)
	}
	term := terms[idx]
	if term.Run == nil {
		m.flash = "run target " + term.Name + ": no launcher"
		return m, nil
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "ahjo"
	}
	argv := []string{self, sub, alias}
	asTab := honorTab && term.IsCurrent
	if err := term.Run(argv, asTab); err != nil {
		m.flash = "open " + term.Name + " failed: " + err.Error()
		return m, nil
	}
	m.flash = "opening " + term.Name + " → " + cmdStr
	return m, nil
}

// handleToggleMirror runs `ahjo mirror off` if this worktree is the active
// mirror, otherwise opens the inputMirrorTarget prompt prefilled with the
// remembered per-repo target (or the TUI's startup cwd on first activation).
// Activating while another container already holds the mirror is fine:
// `mirror on` takes the device over (stop+revert the old mirror, then start
// here), so the TUI can switch the mirror between containers in one gesture.
func (m *model) handleToggleMirror() (tea.Model, tea.Cmd) {
	w := selectedBranch(m.containers)
	if w == nil {
		return m, nil
	}
	if m.snap.MirrorSlug == w.Slug && m.snap.MirrorAlive {
		m.flash = "stopping mirror…"
		// execAhjoCaptured, not execAhjo: `mirror off` no longer prompts (the
		// revert is automatic, no confirmation), so it needs no TTY. Running it
		// captured keeps the TUI up and lands the result line in the flash,
		// instead of flicking through the alt-screen for a non-interactive
		// teardown.
		return m, execAhjoCaptured("mirror", "off", "mirror", "off")
	}
	m.startInput(inputMirrorTarget)
	return m, nil
}
