package top

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

var (
	detailLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	detailValue = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	detailTitle = lipgloss.NewStyle().Foreground(lipgloss.Color("#238FF9")).Bold(true)

	// Footer shortcut hints — keys render brighter than their labels so the
	// buttons stand out from the descriptions.
	footerKey   = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	footerLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

	// PR state palette — Primer dark-theme fg tokens so the colors match
	// what users see on github.com. Only the dot + state label get tinted;
	// the rest of the row (number, url) stays detailValue grey.
	prStateOpen     = lipgloss.NewStyle().Foreground(lipgloss.Color("#3fb950"))
	prStateChecking = lipgloss.NewStyle().Foreground(lipgloss.Color("#d29922"))
	prStateFailed   = lipgloss.NewStyle().Foreground(lipgloss.Color("#f85149"))
	prStateMerged   = lipgloss.NewStyle().Foreground(lipgloss.Color("#a371f7"))
	prStateClosed   = lipgloss.NewStyle().Foreground(lipgloss.Color("#f85149"))
)

// linkValue renders text in the detail value color as an OSC 8 hyperlink to url
// (e.g. cmd+click in Ghostty). Terminals without OSC 8 support drop the escapes
// and show text verbatim, so it degrades cleanly. The link target is the full
// url even if text is later truncated to fit the pane, since the url lives in
// the opening escape rather than the visible text.
func linkValue(text, url string) string {
	return detailValue.Hyperlink(url).Render(text)
}

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

	row("state", formatContainerState(snap, br.Slug))
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
	sshURL := fmt.Sprintf("ssh://127.0.0.1:%d", br.SSHPort)
	row("ssh", linkValue(sshURL, sshURL))

	containerState := "missing"
	if snap.Containers[br.Slug] {
		containerState = "present"
	}
	if name, err := deps.ResolveContainerName(&br); err == nil {
		row("container", detailValue.Render(fmt.Sprintf("%s (%s)", name, containerState)))
	} else {
		row("container", detailValue.Render(containerState))
	}

	row("exposed", portLines(exposedEntries(snap.PortsByBranch[br.Slug])))
	row("forwarded", portLines(forwardedEntries(snap.ForwardsByBranch[br.Slug])))
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

// fitWidth truncates each line of a rendered detail block to width display
// cells, marking any clipped line with a trailing "…". The viewport already
// hard-cuts overflowing lines, but does so silently — a long URL or path just
// stops mid-token with no sign there's more, so it reads as if that's the whole
// value. Pre-truncating with an ellipsis keeps those values unambiguous.
// ansi.Truncate keeps escape sequences past the cut, so the SGR reset and the
// OSC 8 hyperlink terminator still close (styling stays balanced and a clipped
// link still targets its full URL). width <= 0 leaves content untouched, for
// the brief window before the pane has been sized.
func fitWidth(content string, width int) string {
	if width <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, width, "…")
	}
	return strings.Join(lines, "\n")
}

// valueIndent is the column where detail values begin: the 12-char label
// field (`%-12s`) plus the single separating space row() writes after it.
// Continuation lines of a multi-line value are padded to this so they sit
// directly under the first entry.
const valueIndent = 13

// portLines turns a slice of pre-aligned entry strings into one detail value:
// the first entry rides the label line, the rest are indented under the value
// column so the block reads as a grid. Empty input renders as "-".
func portLines(entries []string) string {
	if len(entries) == 0 {
		return detailValue.Render("-")
	}
	indent := strings.Repeat(" ", valueIndent)
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n")
			b.WriteString(indent)
		}
		// Entries are pre-styled by arrowAligned (grey value text + an OSC 8
		// link on the URL token), so they're written verbatim here.
		b.WriteString(e)
	}
	return b.String()
}

// exposedEntries renders each expose/auto-expose allocation as
// ":<cport> -> <scheme>://127.0.0.1:<hport>" with the arrows aligned. The
// scheme is guessed from the container port — that's where the service
// actually listens — while the URL points at the loopback host port you'd
// open to reach it.
func exposedEntries(allocs []ports.Allocation) []string {
	pairs := ports.ExposedPairs(allocs)
	lefts := make([]string, len(pairs))
	rights := make([]string, len(pairs))
	for i, p := range pairs {
		lefts[i] = fmt.Sprintf(":%d", p.Container)
		rights[i] = fmt.Sprintf("%s://127.0.0.1:%d", portScheme(p.Container), p.Host)
	}
	return arrowAligned(lefts, rights, urlRight)
}

// forwardedEntries renders each forward as
// "<scheme>://127.0.0.1:<hport> -> :<cport>" with the arrows aligned. The
// scheme is guessed from the host port — the service the container reaches
// out to runs on the host — and the container side shows the in-container
// listen port.
func forwardedEntries(fwds []Forward) []string {
	lefts := make([]string, len(fwds))
	rights := make([]string, len(fwds))
	for i, f := range fwds {
		lefts[i] = fmt.Sprintf("%s://127.0.0.1:%d", portScheme(f.Host), f.Host)
		rights[i] = fmt.Sprintf(":%d", f.Container)
	}
	return arrowAligned(lefts, rights, urlLeft)
}

// urlSide marks which token of an arrow-aligned port entry is the clickable
// URL: the right side for exposed (":cport -> URL"), the left for forwarded
// ("URL -> :cport").
type urlSide int

const (
	urlRight urlSide = iota
	urlLeft
)

// arrowAligned pads every left token to the widest one so the " -> " arrows
// line up, then renders each pair into a fully-styled entry: the plain side in
// detailValue grey and the URL side as a clickable OSC 8 hyperlink to itself.
// Padding is measured on the plain token text (ASCII here, so byte length
// equals display width) before any link escapes are added, so the escape bytes
// don't skew the alignment.
func arrowAligned(lefts, rights []string, side urlSide) []string {
	w := 0
	for _, l := range lefts {
		if len(l) > w {
			w = len(l)
		}
	}
	out := make([]string, len(lefts))
	for i := range lefts {
		left, right := lefts[i], rights[i]
		if side == urlLeft {
			pad := strings.Repeat(" ", w-len(left))
			out[i] = linkValue(left, left) + pad + detailValue.Render(" -> "+right)
			continue
		}
		out[i] = detailValue.Render(fmt.Sprintf("%-*s -> ", w, left)) + linkValue(right, right)
	}
	return out
}

// portScheme maps a well-known port to the URL scheme shown beside it in the
// details pane. Anything unrecognized falls back to http, which covers the
// common case of dev servers on arbitrary high ports.
func portScheme(port int) string {
	switch port {
	case 443, 8443:
		return "https"
	case 5432:
		return "pgsql"
	case 3306:
		return "mysql"
	case 6379:
		return "redis"
	case 27017:
		return "mongodb"
	case 5672:
		return "amqp"
	case 22:
		return "ssh"
	case 21:
		return "ftp"
	case 25, 465, 587:
		return "smtp"
	default:
		return "http"
	}
}

// formatContainerState renders the leading "● state" of the details pane.
// Green for running, dim grey for stopped (the normal "off" state — not an
// error), and red for anything else, including a missing/unreadable status
// (snapshot didn't capture one). The raw incus string is shown lowercased
// so the value reads as a single field, not a sentence.
func formatContainerState(snap Snapshot, slug string) string {
	raw, ok := snap.ContainerStates[slug]
	if !ok || raw == "" {
		if snap.Containers[slug] {
			// Container is registered but the snapshot couldn't read its
			// status — surface that as red so the user knows something
			// went wrong with the probe.
			return gitError.Render("● unknown")
		}
		return iconMissing.Render("● missing")
	}
	label := strings.ToLower(raw)
	switch label {
	case "running":
		return gitClean.Render("● " + label)
	case "stopped":
		return iconPending.Render("● " + label)
	}
	return gitError.Render("● " + label)
}

// formatMirror colors the leading "active →" (or "inactive →") in the same
// blue/red palette the containers column uses for the ← arrow, then renders
// the target path in the normal value color. The target is a path on the host
// the TUI runs on (unlike the container-side "path" row), so it's a clickable
// file:// link — the full path stays the link target even when the visible text
// is truncated to fit the pane. When no target is configured (legacy snapshot
// data), the state stands alone with no arrow.
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
	return style.Render(state+" →") + " " + linkValue(target, "file://"+target)
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
	ref := fmt.Sprintf("#%d %s", s.PR.Number, s.PR.URL)
	tail := detailValue.Render(" · ") + linkValue(ref, s.PR.URL)
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
