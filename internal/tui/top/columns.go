package top

import (
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lasselaakkonen/ahjo/internal/registry"
)

// repoItem represents one row in the leftmost column.
// kind == "new" is the sentinel "+ new repo" affordance.
type repoItem struct {
	kind string // "repo" | "new"
	repo registry.Repo
}

func (r repoItem) FilterValue() string {
	if r.kind == "new" {
		return "new repo"
	}
	return r.repo.Name
}

// containerItem represents one branch row in the middle column.
// kind == "new" is the sentinel "+ create container" affordance.
//
// status and snap are stamped at build time by containerItemsFor so the
// row's status icons can be rendered without the delegate needing back-
// references to the model. status is nil when no fetch has completed
// yet for this slug; icon renderers treat nil as "pending".
type containerItem struct {
	kind   string // "container" | "new"
	br     registry.Branch
	state  string // "running" | "stopped" | "missing"
	status *BranchStatus
	snap   Snapshot
}

func (c containerItem) FilterValue() string {
	if c.kind == "new" {
		return "create container"
	}
	return strings.Join(c.br.Aliases, ",")
}

// compactDelegate renders a single-line item with a `▸` caret on the focused row.
type compactDelegate struct {
	focused *bool // pointer so we can flip styling without rebuilding the delegate
}

func (d compactDelegate) Height() int                             { return 1 }
func (d compactDelegate) Spacing() int                            { return 0 }
func (d compactDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }

func (d compactDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	selected := index == m.Index()
	focused := d.focused != nil && *d.focused

	caret := "  "
	if selected {
		caret = "▸ "
	}

	style := lipgloss.NewStyle()
	switch {
	case selected && focused:
		style = style.Foreground(lipgloss.Color("#238FF9")).Bold(true)
	case selected:
		style = style.Foreground(lipgloss.Color("245")).Bold(true)
	default:
		style = style.Foreground(lipgloss.Color("252"))
	}

	// For real container rows we render the status icons pre-styled so
	// their per-state colors survive — they sit between the caret and the
	// alias, both of which take the row style.
	var line string
	if v, ok := item.(containerItem); ok && v.kind == "container" {
		icons := renderReplicationIcon(v.snap, v.br.Slug) +
			renderGitIcon(v.status, v.state) +
			renderPRIcon(v.status, v.state)
		line = style.Render(caret) + icons + style.Render(branchLabelOf(v))
	} else {
		line = style.Render(caret + itemLabel(item))
	}

	// Hard-truncate to the list's width so a long alias can't wrap into a
	// second line — that wrap pushes the body past m.height and the
	// alt-screen frame clips the footer off the bottom.
	if lw := m.Width(); lw > 0 {
		line = ansi.Truncate(line, lw, "…")
	}

	fmt.Fprint(w, line)
}

func itemLabel(item list.Item) string {
	switch v := item.(type) {
	case repoItem:
		if v.kind == "new" {
			return "＋ new repo"
		}
		if len(v.repo.Aliases) > 0 {
			return v.repo.Aliases[0]
		}
		return v.repo.Name
	case containerItem:
		if v.kind == "new" {
			return "＋ create container"
		}
		return aliasOf(v)
	}
	return ""
}

// aliasOf returns the display alias for a container row — first listed
// alias when set, otherwise the slug.
func aliasOf(v containerItem) string {
	if len(v.br.Aliases) > 0 {
		return v.br.Aliases[0]
	}
	return v.br.Slug
}

// branchLabelOf is the alias as shown in the containers column. Container
// aliases are "<owner>/<repo>@<branch>" (see registry.MakeBranchAlias); the
// "<owner>/<repo>" part is already shown once as the column header, so we
// strip that "<owner>/<repo>@" prefix here to leave room for longer branch
// names. The "/" guard keeps the strip scoped to that owner/repo shape — a
// custom alias without a slash, or a bare slug, is left untouched.
func branchLabelOf(v containerItem) string {
	alias := aliasOf(v)
	if at := strings.IndexByte(alias, '@'); at > 0 && strings.IndexByte(alias[:at], '/') >= 0 {
		return alias[at+1:]
	}
	return alias
}

// repoDisplayName returns the same label the expanded repo list shows — first
// alias when set, otherwise the repo name — and "" for a nil repo (e.g. when
// the "+ new repo" sentinel is highlighted).
func repoDisplayName(r *registry.Repo) string {
	if r == nil {
		return ""
	}
	if len(r.Aliases) > 0 {
		return r.Aliases[0]
	}
	return r.Name
}

// verticalText stacks s one rune per line (upright, top-down), clipped to
// maxLines with a trailing ellipsis when it would overflow.
func verticalText(s string, maxLines int) string {
	rs := []rune(s)
	if maxLines > 0 && len(rs) > maxLines {
		rs = append(rs[:maxLines-1:maxLines-1], '…')
	}
	lines := make([]string, len(rs))
	for i, r := range rs {
		lines[i] = string(r)
	}
	return strings.Join(lines, "\n")
}

// repoItemsFrom builds the leftmost column's items from a snapshot, always
// appending the "+ new repo" sentinel last.
func repoItemsFrom(snap Snapshot) []list.Item {
	out := make([]list.Item, 0, len(snap.Repos)+1)
	for _, r := range snap.Repos {
		out = append(out, repoItem{kind: "repo", repo: r})
	}
	out = append(out, repoItem{kind: "new"})
	return out
}

// containerItemsFor returns the branches of a single repo, with their
// container-existence state pre-resolved. status holds the most recent
// BranchStatus per slug (nil entries / missing keys render as pending).
// Always appends the "+ create container" sentinel last.
func containerItemsFor(snap Snapshot, repoName string, status map[string]BranchStatus) []list.Item {
	var out []list.Item
	for _, br := range snap.Branches {
		if br.Repo != repoName {
			continue
		}
		state := "missing"
		switch {
		case snap.ContainersRunning[br.Slug]:
			state = "running"
		case snap.Containers[br.Slug]:
			state = "stopped"
		}
		item := containerItem{kind: "container", br: br, state: state, snap: snap}
		if s, ok := status[br.Slug]; ok {
			s := s
			item.status = &s
		}
		out = append(out, item)
	}
	out = append(out, containerItem{kind: "new"})
	return out
}

// selectedRepo returns the repo currently highlighted in the leftmost column,
// or nil if the highlight is on the "+ new repo" sentinel (or no repos exist).
func selectedRepo(l list.Model) *registry.Repo {
	it, ok := l.SelectedItem().(repoItem)
	if !ok || it.kind != "repo" {
		return nil
	}
	r := it.repo
	return &r
}

// selectedBranch returns the branch currently highlighted in the middle
// column, or nil if no item is highlighted (or the "+ create container"
// sentinel is selected).
func selectedBranch(l list.Model) *registry.Branch {
	it, ok := l.SelectedItem().(containerItem)
	if !ok || it.kind != "container" {
		return nil
	}
	b := it.br
	return &b
}

// findRepoByName returns the snapshot's copy of the repo with the given Name,
// or nil if it isn't in the snapshot. Used when only a Branch.Repo string
// is in hand (e.g. from the containers list).
func findRepoByName(repos []registry.Repo, name string) *registry.Repo {
	for i := range repos {
		if repos[i].Name == name {
			return &repos[i]
		}
	}
	return nil
}
