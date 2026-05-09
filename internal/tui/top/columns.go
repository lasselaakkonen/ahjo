package top

import (
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/bubbles/v2/list"
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
type containerItem struct {
	br    registry.Branch
	state string // "present" | "missing"
}

func (c containerItem) FilterValue() string { return strings.Join(c.br.Aliases, ",") }

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

	label := itemLabel(item)
	caret := "  "
	if selected {
		caret = "▸ "
	}
	line := caret + label

	// Hard-truncate to the list's width so a long alias can't wrap into a
	// second line — that wrap pushes the body past m.height and the
	// alt-screen frame clips the footer off the bottom.
	if w := m.Width(); w > 0 {
		line = ansi.Truncate(line, w, "…")
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
	fmt.Fprint(w, style.Render(line))
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
		alias := v.br.Slug
		if len(v.br.Aliases) > 0 {
			alias = v.br.Aliases[0]
		}
		marker := "·"
		if v.state == "present" {
			marker = "●"
		}
		return fmt.Sprintf("%s %s", marker, alias)
	}
	return ""
}

// repoItemsFrom builds the leftmost column's items from a snapshot, always
// appending the "+ new repo" sentinel last.
func repoItemsFrom(snap snapshot) []list.Item {
	out := make([]list.Item, 0, len(snap.repos)+1)
	for _, r := range snap.repos {
		out = append(out, repoItem{kind: "repo", repo: r})
	}
	out = append(out, repoItem{kind: "new"})
	return out
}

// containerItemsFor returns the branches of a single repo, with their
// container-existence state pre-resolved.
func containerItemsFor(snap snapshot, repoName string) []list.Item {
	var out []list.Item
	for _, br := range snap.branches {
		if br.Repo != repoName {
			continue
		}
		state := "missing"
		if snap.containers[br.Slug] {
			state = "present"
		}
		out = append(out, containerItem{br: br, state: state})
	}
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
// column, or nil if no item is highlighted.
func selectedBranch(l list.Model) *registry.Branch {
	it, ok := l.SelectedItem().(containerItem)
	if !ok {
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
