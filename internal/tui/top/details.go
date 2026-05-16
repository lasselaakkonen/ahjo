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

	// PR state palette — Primer dark-theme fg tokens so the colors match
	// what users see on github.com. Only the dot + state label get tinted;
	// the rest of the row (number, url) stays detailValue grey.
	prStateOpen     = lipgloss.NewStyle().Foreground(lipgloss.Color("#3fb950"))
	prStateChecking = lipgloss.NewStyle().Foreground(lipgloss.Color("#d29922"))
	prStateFailed   = lipgloss.NewStyle().Foreground(lipgloss.Color("#f85149"))
	prStateMerged   = lipgloss.NewStyle().Foreground(lipgloss.Color("#a371f7"))
	prStateClosed   = lipgloss.NewStyle().Foreground(lipgloss.Color("#f85149"))
)

func renderHostDetail(snap Snapshot) string {
	var b strings.Builder
	title := snap.Host.Title
	if title == "" {
		title = "host"
	}
	b.WriteString(detailTitle.Render(title))
	b.WriteString("\n\n")
	if len(snap.Host.Lines) == 0 {
		b.WriteString(detailValue.Render("(no host details)"))
		return b.String()
	}
	for _, line := range snap.Host.Lines {
		b.WriteString(detailValue.Render(line))
		b.WriteString("\n")
	}
	return b.String()
}

func renderRepoDetail(repo registry.Repo, snap Snapshot) string {
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

func renderBranchDetail(deps Deps, br registry.Branch, snap Snapshot, status *BranchStatus) string {
	var b strings.Builder
	alias := br.Slug
	if len(br.Aliases) > 0 {
		alias = br.Aliases[0]
	}
	b.WriteString(detailTitle.Render(alias))
	b.WriteString("\n\n")
	// row writes a `key: value` line. The value is taken verbatim so callers
	// that need state-colored segments (git, pr, mirror) can pass pre-styled
	// strings; plain rows wrap their value in detailValue at the call site.
	row := func(k, v string) {
		b.WriteString(detailLabel.Render(fmt.Sprintf("%-12s", k+":")))
		b.WriteString(" ")
		b.WriteString(v)
		b.WriteString("\n")
	}

	if snap.Containers[br.Slug] {
		if snap.ContainersRunning[br.Slug] {
			row("git", FormatGitStatus(status))
			row("pr", FormatPRStatus(status))
		} else {
			notRunning := iconPending.Render("Instance not running")
			row("git", notRunning)
			row("pr", notRunning)
		}
	}
	row("repo", detailValue.Render(br.Repo))
	row("branch", detailValue.Render(br.Branch))
	row("slug", detailValue.Render(br.Slug))
	row("ssh", detailValue.Render(fmt.Sprintf("127.0.0.1:%d", br.SSHPort)))

	containerState := "missing"
	if snap.Containers[br.Slug] {
		containerState = "present"
	}
	if name, err := deps.ResolveContainerName(&br); err == nil {
		row("container", detailValue.Render(fmt.Sprintf("%s (%s)", name, containerState)))
	} else {
		row("container", detailValue.Render(containerState))
	}

	row("exposed", detailValue.Render(deps.FormatExposed(snap.PortsByBranch[br.Slug])))
	row("path", detailValue.Render("/repo"))
	row("created", detailValue.Render(br.CreatedAt.Format("2006-01-02 15:04")))
	if snap.MirrorSlug == br.Slug {
		row("mirror", formatMirror(snap, br.Repo))
	}
	if br.IsDefault {
		row("default", detailValue.Render("yes"))
	}
	return b.String()
}

// formatMirror colors the leading "active →" (or "inactive →") in the same
// blue/red palette the containers column uses for the ← arrow, then renders
// the target path in the normal value color. When no target is configured
// (legacy snapshot data), the state stands alone with no arrow.
func formatMirror(snap Snapshot, repoName string) string {
	state := "active"
	style := replicationAlive
	if !snap.MirrorAlive {
		state = "inactive"
		style = replicationDown
	}
	target := mirrorTargetFor(snap, repoName)
	if target == "" {
		return style.Render(state)
	}
	return style.Render(state+" →") + " " + detailValue.Render(target)
}

// FormatGitStatus turns a cached BranchStatus into the styled one-line value
// shown next to the "git" label. Output mirrors the icon + color the
// containers-column glyph uses for the same state (●/○/!/·) so the eye can
// link the two views. Returns a dim "· …" while the first fetch is
// outstanding so the user gets immediate feedback that work is happening.
func FormatGitStatus(s *BranchStatus) string {
	if s == nil {
		return iconPending.Render("· …")
	}
	if s.GitErr != "" {
		return gitError.Render("! " + errLine(s.GitErr))
	}
	if !s.GitChecked {
		return iconPending.Render("· …")
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
	text := strings.Join(parts, " · ")
	if s.Dirty || s.Stashed > 0 || s.Ahead > 0 {
		return gitDirty.Render("○ " + text)
	}
	return gitClean.Render("● " + text)
}

// FormatPRStatus is the PR-side counterpart to FormatGitStatus: glyph and
// label share a color (open=green, checking=amber, failed/closed=red,
// merged=purple, draft=grey-blue) so the row visually echoes the
// containers-column icon. "no PR" renders as a dim dot rather than blank
// so the row reads as "checked, nothing here" instead of "still loading".
func FormatPRStatus(s *BranchStatus) string {
	if s == nil {
		return iconPending.Render("· …")
	}
	if s.PRErr != "" {
		return gitError.Render("! " + errLine(s.PRErr))
	}
	if !s.PRChecked {
		return iconPending.Render("· …")
	}
	if s.PR == nil {
		return iconMissing.Render("· none")
	}
	glyph, label, style := prGlyphLabelStyle(s.PR)
	head := style.Render(glyph + " " + label)
	tail := detailValue.Render(fmt.Sprintf(" · #%d %s", s.PR.Number, s.PR.URL))
	return head + tail
}

// errLine renders an error message for the one-row detail field. Truncates
// to keep the panel layout stable when the underlying tool prints a long
// stderr.
func errLine(msg string) string {
	const max = 80
	if len(msg) > max {
		msg = msg[:max-1] + "…"
	}
	return msg
}

func mirrorTargetFor(snap Snapshot, repoName string) string {
	for i := range snap.Repos {
		if snap.Repos[i].Name == repoName {
			return snap.Repos[i].MacMirrorTarget
		}
	}
	return ""
}

func branchesFor(snap Snapshot, repoName string) []registry.Branch {
	var out []registry.Branch
	for _, br := range snap.Branches {
		if br.Repo == repoName {
			out = append(out, br)
		}
	}
	return out
}
