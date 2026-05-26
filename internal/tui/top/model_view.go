package top

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func (m *model) View() tea.View {
	if m.width == 0 || m.height == 0 {
		v := tea.NewView("loading…")
		v.AltScreen = true
		return v
	}

	rightContent := m.details.View()
	switch m.inputMode {
	case inputNone:
		// no overlay
	case inputIDE:
		rightContent = m.idePickerBlock()
	case inputRunTarget:
		rightContent = m.runTargetPickerBlock()
	default:
		rightContent = m.inputBlock()
	}

	logBlock := m.renderLog()
	logH := 0
	if logBlock != "" {
		logH = lipgloss.Height(logBlock)
	}
	rowH := m.height - footerH - logH
	if rowH < 5 {
		rowH = 5
	}
	// Keep embedded list/viewport sizes in sync with the row height we're
	// about to render into — flash messages can appear/disappear between
	// resizes and would otherwise leave the panes one render stale.
	m.applySizes()
	rcw, ccw, rightWidth := m.colWidths()
	var left string
	if m.focus == focusRepos {
		left = paneStyle(true, rcw, rowH).Render(m.repos.View())
	} else {
		// Collapsed: a vertical-text breadcrumb of the selected repo, so the
		// freed width flows into the containers column while still showing
		// which repo the visible containers belong to.
		strip := verticalText(repoDisplayName(selectedRepo(m.repos)), rowH-2)
		left = paneStyle(false, rcw, rowH).Render(detailTitle.Render(strip))
	}
	// The containers column leads with a blue "<owner>/<repo>" header (same
	// style as the details title) so the repo is named even while its own
	// column is collapsed; the "\n\n" spacer is always present so the list
	// rows line up with the height reserved in applySizes.
	midContent := m.containerHeader(ccw-2) + "\n\n" + m.containers.View()
	mid := paneStyle(m.focus == focusContainers, ccw, rowH).Render(midContent)
	right := paneStyle(m.focus == focusDetails, rightWidth, rowH).Render(rightContent)

	row := lipgloss.JoinHorizontal(lipgloss.Top, left, mid, right)
	// Belt-and-braces: even if a pane somehow renders past rowH (e.g.
	// border math edge cases), clamp the row to rowH so the footer's
	// position is guaranteed.
	row = lipgloss.NewStyle().MaxHeight(rowH).Render(row)
	parts := []string{row}
	if logBlock != "" {
		parts = append(parts, logBlock)
	}
	parts = append(parts, m.renderFooter())
	body := lipgloss.JoinVertical(lipgloss.Left, parts...)

	v := tea.NewView(body)
	v.AltScreen = true
	return v
}

// containerHeader returns the blue "<owner>/<repo>" title shown above the
// containers list — the selected repo's display name, truncated to the
// column's interior width so a long owner/repo can't wrap and break the
// header's reserved height. Empty when no repo is selected (the list is then
// empty too, so only the spacer shows).
func (m *model) containerHeader(width int) string {
	name := repoDisplayName(selectedRepo(m.repos))
	if name == "" {
		return ""
	}
	if width > 0 {
		name = ansi.Truncate(name, width, "…")
	}
	return detailTitle.Render(name)
}

func (m *model) inputBlock() string {
	var title, hint string
	switch m.inputMode {
	case inputAddRepo:
		title = detailTitle.Render("new repo")
		hint = detailLabel.Render("enter to submit · esc to cancel")
	case inputNewContainer:
		repo := selectedRepo(m.repos)
		repoAlias := ""
		if repo != nil {
			repoAlias = repo.Aliases[0]
		}
		title = detailTitle.Render("new container · " + repoAlias)
		hint = detailLabel.Render("enter to submit · esc to cancel")
	case inputMirrorTarget:
		w := selectedBranch(m.containers)
		alias := ""
		if w != nil {
			alias = w.Aliases[0]
		}
		title = detailTitle.Render("mirror target · " + alias)
		hint = detailLabel.Render("enter to submit · esc to cancel")
	case inputForward:
		w := selectedBranch(m.containers)
		alias := ""
		if w != nil {
			alias = w.Aliases[0]
		}
		title = detailTitle.Render("forward host port · " + alias)
		hint = detailLabel.Render("host :port (or 'host-port container-port') · enter to submit · esc to cancel")
	}
	return strings.Join([]string{title, "", m.input.View(), "", hint}, "\n")
}

// runTargetPickerBlock renders the inputRunTarget overlay: a ▸-marked list
// of detected terminal emulators (the current one tagged "(current)" and
// noted as opening a new tab) followed by a sentinel "copy to clipboard"
// row. Mirrors idePickerBlock's title/hint chrome so the two modals look
// consistent.
func (m *model) runTargetPickerBlock() string {
	title := detailTitle.Render("run ahjo " + m.runTargetSub + " · " + m.runTargetAlias)
	rows := len(m.runTargetTerms) + 1
	lines := make([]string, 0, rows)
	for i, term := range m.runTargetTerms {
		caret := "  "
		style := detailValue
		if i == m.runTargetIdx {
			caret = "▸ "
			style = detailTitle
		}
		label := term.Name
		switch {
		case term.IsCurrent:
			label += "  (current, new tab)"
		default:
			label += "  (new window)"
		}
		lines = append(lines, style.Render(caret+label))
	}
	caret := "  "
	style := detailValue
	if m.runTargetIdx == len(m.runTargetTerms) {
		caret = "▸ "
		style = detailTitle
	}
	lines = append(lines, style.Render(caret+"copy to clipboard"))
	hint := detailLabel.Render(m.runTargetHint())
	parts := []string{title, ""}
	parts = append(parts, lines...)
	parts = append(parts, "", hint)
	return strings.Join(parts, "\n")
}

// runTargetHint returns the footer text for the picker, mirroring the
// actions available for the currently highlighted row so the hint never
// promises something the row can't deliver (e.g. "w for new window" on a
// non-current terminal whose Enter already opens a window, or on the
// clipboard sentinel where there's no window/tab choice at all).
func (m *model) runTargetHint() string {
	parts := []string{"↑/↓ pick"}
	switch {
	case m.runTargetIdx >= len(m.runTargetTerms):
		parts = append(parts, "enter to copy")
	case m.runTargetTerms[m.runTargetIdx].IsCurrent:
		parts = append(parts, "enter for new tab", "w for new window")
	default:
		parts = append(parts, "enter for new window")
	}
	parts = append(parts, "esc to cancel")
	return strings.Join(parts, " · ")
}

// idePickerBlock renders the inputIDE overlay: a ▸-marked list of the
// host's detected SSH-capable IDEs. Uses the same title/hint chrome as
// inputBlock so the modal feels consistent with text-input prompts.
func (m *model) idePickerBlock() string {
	title := detailTitle.Render("open in IDE · " + m.idePickerHost + ":" + m.idePickerPath)
	hint := detailLabel.Render("↑/↓ pick · enter to open · esc to cancel")
	lines := make([]string, 0, len(m.idePickerIDEs))
	for i, ide := range m.idePickerIDEs {
		caret := "  "
		style := detailValue
		if i == m.idePickerIdx {
			caret = "▸ "
			style = detailTitle
		}
		lines = append(lines, style.Render(caret+ide.Name))
	}
	parts := []string{title, ""}
	parts = append(parts, lines...)
	parts = append(parts, "", hint)
	return strings.Join(parts, "\n")
}

func (m *model) renderFooter() string {
	bindings := []key.Binding{
		m.keys.Left, m.keys.Right, m.keys.Up, m.keys.Down,
	}
	switch m.focus {
	case focusRepos:
		bindings = append(bindings, m.keys.AddRepo, m.keys.RemoveRepo)
	case focusContainers, focusDetails:
		bindings = append(bindings, m.keys.NewContainer, m.keys.RemoveContainer)
		if selectedBranch(m.containers) != nil {
			bindings = append(bindings, m.keys.StartStop, m.keys.ToggleExpose, m.keys.ToggleMirror, m.keys.Forward, m.keys.CopyClaudeCmd, m.keys.CopyShellCmd, m.keys.OpenIDE)
		}
	}
	bindings = append(bindings, m.keys.Submit, m.keys.Refresh, m.keys.Quit)

	hints := make([]string, 0, len(bindings))
	for _, b := range bindings {
		h := b.Help()
		if h.Key == "" {
			continue
		}
		if h.Desc == "" {
			hints = append(hints, footerKey.Render(h.Key))
		} else {
			hints = append(hints, footerKey.Render(h.Key)+" "+footerLabel.Render(h.Desc))
		}
	}
	footer := strings.Join(hints, footerLabel.Render(" · "))
	if m.width > 0 {
		footer = ansi.Truncate(footer, m.width, "…")
	}
	return footer
}

// logMessage returns the latest action / status text, preferring a fresh
// flash over a stale load error. Empty when there's nothing to surface.
func (m *model) logMessage() string {
	if m.flash != "" {
		return m.flash
	}
	if m.loadErr != nil {
		return "load error: " + m.loadErr.Error()
	}
	return ""
}

var logStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.Color("240")).
	Foreground(lipgloss.Color("252"))

func (m *model) renderLog() string {
	msg := m.logMessage()
	if msg == "" || m.width < 4 {
		return ""
	}
	return logStyle.Width(m.width - 2).Render(msg)
}

func (m *model) logHeight() int {
	s := m.renderLog()
	if s == "" {
		return 0
	}
	return lipgloss.Height(s)
}

// paneStyle pins the pane to a fixed (width × height) box. In lipgloss v2
// Width/Height set the *exterior* block size (borders included), so we pass
// width/height directly — the content area inside the rounded border is
// width-2 × height-2, which is what SetSize() on the embedded list/viewport
// is given in applySizes. MaxHeight clips long content (e.g. a viewport
// line that lipgloss wrapped) so the row never grows past `height` and
// pushes the footer off-screen.
func paneStyle(focused bool, width, height int) lipgloss.Style {
	color := lipgloss.Color("240")
	if focused {
		color = lipgloss.Color("#238FF9")
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(color).
		Width(width).
		Height(height).
		MaxHeight(height)
}
