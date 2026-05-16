package top

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lasselaakkonen/ahjo/internal/git"
)

// branchStatusStaleness bounds how often we'll re-run `git status` + `gh pr
// list` for the same branch. Cheap enough that the user perceives the panel
// as live, but slow enough that holding the arrow keys doesn't fan out into
// a stampede of `gh` subprocesses.
const branchStatusStaleness = 10 * time.Second

type focus int

const (
	focusRepos focus = iota
	focusContainers
	focusDetails
)

type inputMode int

const (
	inputNone inputMode = iota
	inputAddRepo
	inputNewContainer
	inputMirrorTarget
	inputIDE
	inputRunTarget
)

const (
	colMinWidth = 20

	repoColMax        = 40
	repoColBreakpoint = 200

	containerColMax        = 60
	containerColBreakpoint = 250

	footerH = 1
)

// scaledWidth interpolates linearly from min at termWidth=0 to max at
// termWidth=breakpoint, then clamps to [min, max].
func scaledWidth(termWidth, min, max, breakpoint int) int {
	w := min + termWidth*(max-min)/breakpoint
	if w < min {
		return min
	}
	if w > max {
		return max
	}
	return w
}

func (m *model) repoColWidth() int {
	return scaledWidth(m.width, colMinWidth, repoColMax, repoColBreakpoint)
}

func (m *model) containerColWidth() int {
	return scaledWidth(m.width, colMinWidth, containerColMax, containerColBreakpoint)
}

// New constructs the top model. Caller runs it via tea.NewProgram(m).
func New(deps Deps) tea.Model {
	reposFocused := true
	contFocused := false

	repos := list.New(nil, compactDelegate{focused: &reposFocused}, colMinWidth-2, 10)
	repos.SetShowTitle(false)
	repos.SetShowStatusBar(false)
	repos.SetShowHelp(false)
	repos.SetFilteringEnabled(false)

	containers := list.New(nil, compactDelegate{focused: &contFocused}, colMinWidth-2, 10)
	containers.SetShowTitle(false)
	containers.SetShowStatusBar(false)
	containers.SetShowHelp(false)
	containers.SetFilteringEnabled(false)

	details := viewport.New(viewport.WithWidth(40), viewport.WithHeight(10))

	ti := textinput.New()
	ti.CharLimit = 200
	ti.SetWidth(50)

	cwd, _ := os.Getwd()

	return &model{
		deps:         deps,
		repos:        repos,
		containers:   containers,
		details:      details,
		input:        ti,
		startCwd:     cwd,
		reposFocused: &reposFocused,
		contFocused:  &contFocused,
		focus:        focusRepos,
		keys:         newKeymap(),
		help:         help.New(),
	}
}

type model struct {
	deps Deps

	repos      list.Model
	containers list.Model
	details    viewport.Model

	input     textinput.Model
	inputMode inputMode
	flash     string
	// startCwd is the working directory at TUI launch — used as the default
	// prefill when prompting for a mirror target on first activation.
	startCwd string

	reposFocused *bool
	contFocused  *bool
	focus        focus

	keys keymap
	help help.Model

	width, height int

	snap    Snapshot
	loadErr error

	// branchStatus caches the most recent git/PR snapshot per branch slug.
	// inFlightStatus tracks slugs with an outstanding Deps.LoadBranchStatus
	// call so we don't pile on duplicate requests.
	branchStatus   map[string]BranchStatus
	inFlightStatus map[string]bool

	// idePickerIDEs / idePickerIdx back the inputIDE picker. Populated on
	// entry from deps.IDEs() against the then-selected branch, so the
	// launcher inside each IDE entry already knows which host/path to
	// open. Reset on cancel/submit.
	idePickerIDEs []IDE
	idePickerIdx  int
	idePickerHost string
	idePickerPath string

	// runTargetTerms / runTargetIdx back the inputRunTarget picker
	// (presented when `s`/`a` is pressed on a selected container).
	// runTargetSub is "claude" or "shell"; runTargetAlias is the
	// branch alias the picker was opened against, captured at entry so
	// the launcher doesn't rely on selection state holding still. Reset
	// on cancel/submit.
	runTargetTerms []Terminal
	runTargetIdx   int
	runTargetSub   string
	runTargetAlias string
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(refreshCmd(m.deps), tickCmd(), textinput.Blink)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.applySizes()
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
		m.refreshContainers()
		m.refreshDetails()
		return m, m.maybeRefreshBranchStatus()

	case branchStatusMsg:
		if m.branchStatus == nil {
			m.branchStatus = make(map[string]BranchStatus)
		}
		m.branchStatus[msg.slug] = msg.status
		delete(m.inFlightStatus, msg.slug)
		m.refreshContainers()
		m.refreshDetails()
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
		if c := m.maybeRefreshBranchStatus(); c != nil {
			cmd = tea.Batch(cmd, c)
		}
	} else if selectedBranchSlug(m.containers) != prevWt {
		m.refreshDetails()
		if c := m.maybeRefreshBranchStatus(); c != nil {
			cmd = tea.Batch(cmd, c)
		}
	}
	return m, cmd
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
	m.idePickerHost = "ahjo-" + br.Slug
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
		cmd := execAhjoCaptured("mirroring", alias, "mirror", alias, "--target", val)
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
	m.focus = focus(target)
	*m.reposFocused = m.focus == focusRepos
	*m.contFocused = m.focus == focusContainers
	m.refreshDetails()
}

func (m *model) refreshContainers() {
	repo := selectedRepo(m.repos)
	if repo == nil {
		m.containers.SetItems(nil)
		return
	}
	m.containers.SetItems(containerItemsFor(m.snap, repo.Name, m.branchStatus))
}

func (m *model) refreshDetails() {
	var content string
	switch m.focus {
	case focusRepos:
		repo := selectedRepo(m.repos)
		if repo == nil {
			content = renderHostDetail(m.snap)
		} else {
			content = renderRepoDetail(*repo, m.snap)
		}
	case focusContainers:
		if w := selectedBranch(m.containers); w != nil {
			var status *BranchStatus
			if s, ok := m.branchStatus[w.Slug]; ok {
				status = &s
			}
			content = renderBranchDetail(m.deps, *w, m.snap, status)
		} else if repo := selectedRepo(m.repos); repo != nil {
			content = renderRepoDetail(*repo, m.snap)
		} else {
			content = renderHostDetail(m.snap)
		}
	case focusDetails:
		return
	}
	m.details.SetContent(content)
	m.details.GotoTop()
}

func (m *model) applySizes() {
	rowH := m.height - footerH - m.logHeight()
	if rowH < 5 {
		rowH = 5
	}
	rcw := m.repoColWidth()
	ccw := m.containerColWidth()
	rightWidth := m.width - rcw - ccw
	if rightWidth < colMinWidth {
		rightWidth = colMinWidth
	}
	m.repos.SetSize(rcw-2, rowH-2)
	m.containers.SetSize(ccw-2, rowH-2)
	m.details.SetWidth(rightWidth - 2)
	m.details.SetHeight(rowH - 2)
	m.input.SetWidth(rightWidth - 4)
}

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
// be shown in the TUI's flash line. Use this for short-lived commands where
// the alt-screen flash is more annoying than the loss of live progress
// (e.g. `ahjo mirror`, where rsync output is also written to mirror.log).
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
	m.flash = "removing " + alias + "…"
	return execAhjo("removed", alias, "rm", alias)
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

// handleToggleMirror runs `ahjo mirror off` if this worktree is the active
// mirror, otherwise opens the inputMirrorTarget prompt prefilled with the
// remembered per-repo target (or the TUI's startup cwd on first activation).
// The activate subprocess uses tea.ExecProcess because rsync prints progress;
// runMirrorActivate stops any other active mirror itself, so this is purely
// the dispatch.
func (m *model) handleToggleMirror() (tea.Model, tea.Cmd) {
	w := selectedBranch(m.containers)
	if w == nil {
		return m, nil
	}
	if m.snap.MirrorSlug == w.Slug && m.snap.MirrorAlive {
		m.flash = "stopping mirror…"
		return m, execAhjoCaptured("mirror", "off", "mirror", "off")
	}
	m.startInput(inputMirrorTarget)
	return m, nil
}

type branchStatusMsg struct {
	slug   string
	status BranchStatus
}

// maybeRefreshBranchStatus returns a tea.Cmd that fetches BranchStatus
// for every present container in the focused repo via
// Deps.LoadBranchStatus. The single-flight + staleness guards are
// applied per slug, so this is safe to call on every tick: each branch
// gets at most one in-flight fetch and is re-fetched only after the
// cache goes stale. Returns nil when there's nothing to do (no repo
// focused, no eligible containers, or no fetcher wired).
func (m *model) maybeRefreshBranchStatus() tea.Cmd {
	if m.deps.LoadBranchStatus == nil {
		return nil
	}
	repo := selectedRepo(m.repos)
	if repo == nil {
		return nil
	}
	if m.inFlightStatus == nil {
		m.inFlightStatus = make(map[string]bool)
	}
	var cmds []tea.Cmd
	for _, br := range m.snap.Branches {
		if br.Repo != repo.Name {
			continue
		}
		if !m.snap.ContainersRunning[br.Slug] {
			continue
		}
		if m.inFlightStatus[br.Slug] {
			continue
		}
		if cur, ok := m.branchStatus[br.Slug]; ok && time.Since(cur.FetchedAt) < branchStatusStaleness {
			continue
		}
		slug := br.Slug
		m.inFlightStatus[slug] = true
		cmds = append(cmds, func() tea.Msg {
			bs, err := m.deps.LoadBranchStatus(slug)
			if err != nil {
				bs.FetchedAt = time.Now()
				bs.GitErr = err.Error()
			}
			return branchStatusMsg{slug: slug, status: bs}
		})
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

type actionDoneMsg struct {
	action string
	label  string
	err    error
	// output, when non-empty, holds the subprocess's combined stdout+stderr
	// (set by execAhjoCaptured paths). The handler surfaces a tail of this
	// in the flash so the user sees what actually went wrong.
	output string
}

type toggleResultMsg struct {
	action string
	msg    string
	err    error
}

// lastNonEmptyLine returns the last non-empty line of s, with cobra's
// "Error: " prefix stripped so the flash reads cleanly. Empty if s has no
// non-blank content.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "Error: ")
		line = strings.TrimPrefix(line, "error: ")
		return line
	}
	return ""
}

func hasBranches(snap Snapshot, repoName string) bool {
	for _, br := range snap.Branches {
		if br.Repo == repoName {
			return true
		}
	}
	return false
}

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
	rcw := m.repoColWidth()
	ccw := m.containerColWidth()
	rightWidth := m.width - rcw - ccw
	if rightWidth < colMinWidth {
		rightWidth = colMinWidth
	}
	left := paneStyle(m.focus == focusRepos, rcw, rowH).Render(m.repos.View())
	mid := paneStyle(m.focus == focusContainers, ccw, rowH).Render(m.containers.View())
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
			bindings = append(bindings, m.keys.ToggleExpose, m.keys.ToggleMirror, m.keys.CopyClaudeCmd, m.keys.CopyShellCmd, m.keys.OpenIDE)
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
			hints = append(hints, h.Key)
		} else {
			hints = append(hints, fmt.Sprintf("%s %s", h.Key, h.Desc))
		}
	}
	footer := strings.Join(hints, " · ")
	if m.width > 0 {
		footer = ansi.Truncate(footer, m.width, "…")
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render(footer)
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

func selectedRepoName(l list.Model) string {
	if r := selectedRepo(l); r != nil {
		return r.Name
	}
	return ""
}

func selectedBranchSlug(l list.Model) string {
	if w := selectedBranch(l); w != nil {
		return w.Slug
	}
	return ""
}
