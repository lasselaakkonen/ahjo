package top

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/lasselaakkonen/ahjo/internal/registry"
)

var (
	detailLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	detailValue = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	detailTitle = lipgloss.NewStyle().Foreground(lipgloss.Color("#238FF9")).Bold(true)
)

func renderHostDetail(snap snapshot) string {
	var b strings.Builder
	title := snap.host.Title
	if title == "" {
		title = "host"
	}
	b.WriteString(detailTitle.Render(title))
	b.WriteString("\n\n")
	if len(snap.host.Lines) == 0 {
		b.WriteString(detailValue.Render("(no host details)"))
		return b.String()
	}
	for _, line := range snap.host.Lines {
		b.WriteString(detailValue.Render(line))
		b.WriteString("\n")
	}
	return b.String()
}

func renderRepoDetail(repo registry.Repo, snap snapshot) string {
	var b strings.Builder
	b.WriteString(detailTitle.Render(repo.Name))
	b.WriteString("\n\n")
	row := func(k, v string) {
		b.WriteString(detailLabel.Render(fmt.Sprintf("%-12s", k+":")))
		b.WriteString(" ")
		b.WriteString(detailValue.Render(v))
		b.WriteString("\n")
	}
	row("aliases", strings.Join(repo.Aliases, ", "))
	row("remote", repo.Remote)
	row("default", repo.DefaultBase)
	row("bare", repo.BarePath)
	if repo.MacMirrorTarget != "" {
		row("mirror", repo.MacMirrorTarget)
	}
	if repo.BaseContainerName != "" {
		row("base ctr", repo.BaseContainerName)
	}

	wts := worktreesFor(snap, repo.Name)
	row("worktrees", fmt.Sprintf("%d", len(wts)))
	return b.String()
}

func renderWorktreeDetail(deps Deps, w registry.Worktree, snap snapshot) string {
	var b strings.Builder
	alias := w.Slug
	if len(w.Aliases) > 0 {
		alias = w.Aliases[0]
	}
	b.WriteString(detailTitle.Render(alias))
	b.WriteString("\n\n")
	row := func(k, v string) {
		b.WriteString(detailLabel.Render(fmt.Sprintf("%-12s", k+":")))
		b.WriteString(" ")
		b.WriteString(detailValue.Render(v))
		b.WriteString("\n")
	}
	row("repo", w.Repo)
	row("branch", w.Branch)
	row("slug", w.Slug)
	row("ssh", fmt.Sprintf("127.0.0.1:%d", w.SSHPort))

	state := "missing"
	if snap.containers[w.Slug] {
		state = "present"
	}
	if name, err := deps.ResolveContainerName(&w); err == nil {
		row("container", fmt.Sprintf("%s (%s)", name, state))
	} else {
		row("container", state)
	}

	exposed := "-"
	if snap.ports != nil {
		exposed = deps.FormatExposed(snap.ports.AllocationsForSlug(w.Slug))
	}
	row("exposed", exposed)
	row("path", w.WorktreePath)
	row("created", w.CreatedAt.Format("2006-01-02 15:04"))
	return b.String()
}

func worktreesFor(snap snapshot, repoName string) []registry.Worktree {
	var out []registry.Worktree
	for _, w := range snap.worktrees {
		if w.Repo == repoName {
			out = append(out, w)
		}
	}
	return out
}
