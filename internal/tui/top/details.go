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
	if repo.MacMirrorTarget != "" {
		row("mirror", repo.MacMirrorTarget)
	}
	if repo.BaseContainerName != "" {
		row("base ctr", repo.BaseContainerName)
	}

	bs := branchesFor(snap, repo.Name)
	row("branches", fmt.Sprintf("%d", len(bs)))
	return b.String()
}

func renderBranchDetail(deps Deps, br registry.Branch, snap snapshot, status *branchStatus) string {
	var b strings.Builder
	alias := br.Slug
	if len(br.Aliases) > 0 {
		alias = br.Aliases[0]
	}
	b.WriteString(detailTitle.Render(alias))
	b.WriteString("\n\n")
	row := func(k, v string) {
		b.WriteString(detailLabel.Render(fmt.Sprintf("%-12s", k+":")))
		b.WriteString(" ")
		b.WriteString(detailValue.Render(v))
		b.WriteString("\n")
	}
	row("repo", br.Repo)
	row("branch", br.Branch)
	row("slug", br.Slug)
	row("ssh", fmt.Sprintf("127.0.0.1:%d", br.SSHPort))

	state := "missing"
	if snap.containers[br.Slug] {
		state = "present"
	}
	if name, err := deps.ResolveContainerName(&br); err == nil {
		row("container", fmt.Sprintf("%s (%s)", name, state))
	} else {
		row("container", state)
	}

	exposed := "-"
	if snap.ports != nil {
		exposed = deps.FormatExposed(snap.ports.AllocationsForSlug(br.Slug))
	}
	row("exposed", exposed)
	row("path", "/repo")
	if br.IsDefault {
		row("default", "yes")
	}
	row("created", br.CreatedAt.Format("2006-01-02 15:04"))

	if snap.containers[br.Slug] {
		row("git", formatGitStatus(status))
		row("pr", formatPRStatus(status))
	}
	return b.String()
}

// formatGitStatus turns a cached branchStatus into the one-line value shown
// next to the "git" label. Returns "…" while the first fetch is outstanding
// so the user gets immediate feedback that work is happening.
func formatGitStatus(s *branchStatus) string {
	if s == nil {
		return "…"
	}
	if s.GitErr != nil {
		return "error"
	}
	if !s.GitChecked {
		return "…"
	}
	parts := []string{}
	if s.Dirty {
		parts = append(parts, fmt.Sprintf("dirty (%d)", s.DirtyFiles))
	} else {
		parts = append(parts, "clean")
	}
	if s.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("ahead %d", s.Ahead))
	}
	if s.Behind > 0 {
		parts = append(parts, fmt.Sprintf("behind %d", s.Behind))
	}
	return strings.Join(parts, " · ")
}

func formatPRStatus(s *branchStatus) string {
	if s == nil {
		return "…"
	}
	if s.PRErr != nil {
		return "error"
	}
	if !s.PRChecked {
		return "…"
	}
	if s.PR == nil {
		return "none"
	}
	return fmt.Sprintf("#%d %s · %s", s.PR.Number, strings.ToLower(s.PR.State), s.PR.URL)
}

func branchesFor(snap snapshot, repoName string) []registry.Branch {
	var out []registry.Branch
	for _, br := range snap.branches {
		if br.Repo == repoName {
			out = append(out, br)
		}
	}
	return out
}
