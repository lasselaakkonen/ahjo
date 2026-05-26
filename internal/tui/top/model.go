package top

import (
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// branchStatusStaleness bounds how often we'll re-run `git status` + `gh pr
// list` for the same branch. Cheap enough that the user perceives the panel
// as live, but slow enough that holding the arrow keys doesn't fan out into
// a stampede of `gh` subprocesses.
const branchStatusStaleness = 10 * time.Second

// ahjoStateStaleness bounds how often a selection re-pushes the same
// container's ~/.ahjo snapshot. Bridge state only changes via top's own
// toggles (which refresh inline) or out-of-band edits, so a modest window is
// plenty — and it stops fast back-and-forth navigation from fanning out into
// a stampede of `incus file push` calls.
const ahjoStateStaleness = 10 * time.Second

type focus int

const (
	focusRepos focus = iota
	focusContainers
	focusDetails
)

func (f focus) String() string {
	switch f {
	case focusContainers:
		return "containers"
	case focusDetails:
		return "details"
	default:
		return "repos"
	}
}

func focusFromString(s string) focus {
	switch s {
	case "containers":
		return focusContainers
	case "details":
		return focusDetails
	default:
		return focusRepos
	}
}

type inputMode int

const (
	inputNone inputMode = iota
	inputAddRepo
	inputNewContainer
	inputMirrorTarget
	inputForward
	inputIDE
	inputRunTarget
)

const (
	colMinWidth = 20

	// collapsedRepoWidth is the exterior width of the repo column when focus
	// has moved past it — rounded border (2) plus a 1-char content area for
	// the vertical-text breadcrumb.
	collapsedRepoWidth = 3

	repoColMax        = 40
	repoColBreakpoint = 200

	containerColMax        = 60
	containerColBreakpoint = 250

	footerH = 1

	// containerHeaderH is the height reserved at the top of the containers
	// column for the blue "<owner>/<repo>" header (title line + blank
	// spacer). The embedded list is sized to leave room for it, and View
	// always renders the spacer so the list lines up whether or not a repo
	// is selected.
	containerHeaderH = 2
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
	// Once focus leaves the repo column it collapses to a vertical-text strip;
	// the width it gives up flows into the containers column (see colWidths).
	if m.focus != focusRepos {
		return collapsedRepoWidth
	}
	return m.repoColNaturalWidth()
}

// repoColNaturalWidth is the repo column's exterior width when expanded, i.e.
// the width it occupies while focused. colWidths anchors the details pane to
// this value so the pane stays put when the repo column collapses.
func (m *model) repoColNaturalWidth() int {
	return scaledWidth(m.width, colMinWidth, repoColMax, repoColBreakpoint)
}

func (m *model) containerColWidth() int {
	return scaledWidth(m.width, colMinWidth, containerColMax, containerColBreakpoint)
}

// colWidths returns the exterior widths of the three columns: repo, containers,
// details. The details pane is anchored to the repo column's *natural*
// (expanded) width rather than its current width, so collapsing the repo
// column does not reflow details — the freed width flows into the containers
// column, which grows to take up the slack.
func (m *model) colWidths() (repo, container, details int) {
	repo = m.repoColWidth()
	details = m.width - m.repoColNaturalWidth() - m.containerColWidth()
	if details < colMinWidth {
		details = colMinWidth
	}
	container = m.width - repo - details
	if container < colMinWidth {
		container = colMinWidth
	}
	return repo, container, details
}

// stripVimKeys rebinds the bubbles/list movement bindings so single-letter
// hjkl/g/G keys no longer move the cursor or page the list. Arrow keys and
// pgup/pgdn/home/end still work. We do this so `h` (shell shortcut) and `j`
// /`k`/`l` are free for future contextual bindings without leaking into
// list navigation as a side effect.
func stripVimKeys(l *list.Model) {
	l.KeyMap.CursorUp = key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "up"))
	l.KeyMap.CursorDown = key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "down"))
	l.KeyMap.PrevPage = key.NewBinding(key.WithKeys("pgup"), key.WithHelp("pgup", "prev page"))
	l.KeyMap.NextPage = key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("pgdn", "next page"))
	l.KeyMap.GoToStart = key.NewBinding(key.WithKeys("home"), key.WithHelp("home", "go to start"))
	l.KeyMap.GoToEnd = key.NewBinding(key.WithKeys("end"), key.WithHelp("end", "go to end"))
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
	stripVimKeys(&repos)

	containers := list.New(nil, compactDelegate{focused: &contFocused}, colMinWidth-2, 10)
	containers.SetShowTitle(false)
	containers.SetShowStatusBar(false)
	containers.SetShowHelp(false)
	containers.SetFilteringEnabled(false)
	stripVimKeys(&containers)

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

	// restored guards the one-shot restore of the persisted selection: it
	// runs on the first snapshot (the lists are empty before then) and never
	// again, so later refresh ticks don't clobber the user's live navigation.
	restored bool

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

	// inFlightAhjoState / ahjoStateRefreshedAt back the on-selection
	// ~/.ahjo push (Deps.RefreshAhjoState), with the same single-flight +
	// staleness guards as branchStatus so fast navigation can't stampede.
	inFlightAhjoState    map[string]bool
	ahjoStateRefreshedAt map[string]time.Time

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

// restoreSelection applies the persisted selection on top of the freshly
// loaded snapshot: it highlights the saved repo and branch and restores the
// focused panel. Best-effort — a missing file or a saved repo/branch that no
// longer exists is silently skipped, leaving the defaults (repos focused,
// first row). Runs once, from the first snapshot, after the repo list is
// populated.
func (m *model) restoreSelection() {
	sel, err := loadSelection()
	if err != nil || sel == nil {
		return
	}
	if sel.Repo != "" {
		for i, it := range m.repos.Items() {
			if r, ok := it.(repoItem); ok && r.kind == "repo" && r.repo.Name == sel.Repo {
				m.repos.Select(i)
				break
			}
		}
	}
	// Populate the containers column for the just-restored repo so the branch
	// lookup below has rows to match against.
	m.refreshContainers()
	if sel.Branch != "" {
		for i, it := range m.containers.Items() {
			if c, ok := it.(containerItem); ok && c.kind == "container" && c.br.Slug == sel.Branch {
				m.containers.Select(i)
				break
			}
		}
	}
	m.setFocus(focusFromString(sel.Focus))
}

// persistSelection writes the current focus + highlighted repo/branch to disk.
// Called once, on quit. Skipped until the first snapshot has been restored:
// quitting before any snapshot loads would otherwise persist an empty
// selection over a previously-saved good one. Best-effort — a write failure
// only costs us the convenience of resuming where we left off, so the error
// is dropped.
func (m *model) persistSelection() {
	if !m.restored {
		return
	}
	sel := persistedSelection{
		Focus:  m.focus.String(),
		Repo:   selectedRepoName(m.repos),
		Branch: selectedBranchSlug(m.containers),
	}
	_ = sel.save()
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
	m.details.SetContent(fitWidth(content, m.details.Width()))
	m.details.GotoTop()
}

func (m *model) applySizes() {
	rowH := m.height - footerH - m.logHeight()
	if rowH < 5 {
		rowH = 5
	}
	rcw, ccw, rightWidth := m.colWidths()
	m.repos.SetSize(rcw-2, rowH-2)
	m.containers.SetSize(ccw-2, rowH-2-containerHeaderH)
	m.details.SetWidth(rightWidth - 2)
	m.details.SetHeight(rowH - 2)
	m.input.SetWidth(rightWidth - 4)
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

type ahjoStateRefreshedMsg struct{ slug string }

// maybeRefreshAhjoState returns a tea.Cmd that re-pushes the in-container
// ~/.ahjo snapshot for the currently selected, running container via
// Deps.RefreshAhjoState. Like maybeRefreshBranchStatus it's single-flighted +
// staleness-guarded per slug, so holding the arrow keys can't fan out into a
// file-push stampede. Returns nil when there's nothing to do: no hook wired,
// no branch selected, container not running, already in flight, or pushed
// recently.
func (m *model) maybeRefreshAhjoState() tea.Cmd {
	if m.deps.RefreshAhjoState == nil {
		return nil
	}
	w := selectedBranch(m.containers)
	if w == nil || !m.snap.ContainersRunning[w.Slug] {
		return nil
	}
	if m.inFlightAhjoState == nil {
		m.inFlightAhjoState = make(map[string]bool)
	}
	if m.inFlightAhjoState[w.Slug] {
		return nil
	}
	if at, ok := m.ahjoStateRefreshedAt[w.Slug]; ok && time.Since(at) < ahjoStateStaleness {
		return nil
	}
	slug := w.Slug
	m.inFlightAhjoState[slug] = true
	return func() tea.Msg {
		_ = m.deps.RefreshAhjoState(slug) // best-effort side-effect; nothing to render
		return ahjoStateRefreshedMsg{slug: slug}
	}
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
