package top

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Style vars for the per-row status icons in the containers column.
// Greens/ambers/reds match the Primer dark-theme tokens already used by
// prState* in details.go so the column and detail pane stay in sync.
var (
	gitClean         = lipgloss.NewStyle().Foreground(lipgloss.Color("#3fb950"))
	gitDirty         = lipgloss.NewStyle().Foreground(lipgloss.Color("#d29922"))
	gitError         = lipgloss.NewStyle().Foreground(lipgloss.Color("#f85149"))
	iconPending      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	iconMissing      = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	replicationAlive = lipgloss.NewStyle().Foreground(lipgloss.Color("#238FF9"))
	replicationDown  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f85149"))
	prDraft          = lipgloss.NewStyle().Foreground(lipgloss.Color("#768390"))
)

// blank is the placeholder for "no icon here" so every slot is the same
// width and the alias column always lines up across rows.
const blank = "  "

// renderReplicationIcon shows whether this row's container holds the
// active mirror device, and if so whether ahjo-mirror.service is alive.
// Mirroring is one-way (VM → host); the left arrow reads as "pushing
// out". Off rows render blank so the column doesn't shout at the user
// for every non-mirrored container.
func renderReplicationIcon(snap Snapshot, slug string) string {
	if snap.MirrorSlug != slug {
		return blank
	}
	if snap.MirrorAlive {
		return replicationAlive.Render("←") + " "
	}
	return replicationDown.Render("✗") + " "
}

// renderGitIcon collapses the working-tree state into one glyph. "Dirty"
// here is the union of anything that hasn't reached the remote: modified
// files, staged changes, untracked, stashed, and unpushed commits. The
// user explicitly didn't want behind-only surfaced.
//
// state is "running" / "stopped" / "missing". Stopped and missing both
// render the dim/very-dim placeholder rather than an error glyph — `git`
// inside a stopped container would just produce a noisy "Instance is not
// running" error every tick.
func renderGitIcon(s *BranchStatus, state string) string {
	switch state {
	case "missing":
		return iconMissing.Render("·") + " "
	case "stopped":
		return iconPending.Render("·") + " "
	}
	if s == nil {
		return iconPending.Render("·") + " "
	}
	if s.GitErr != "" {
		return gitError.Render("!") + " "
	}
	if !s.GitChecked {
		return iconPending.Render("·") + " "
	}
	if s.Dirty || s.Stashed > 0 || s.Ahead > 0 {
		return gitDirty.Render("○") + " "
	}
	return gitClean.Render("●") + " "
}

// renderPRIcon shows the GitHub-side state for this branch. Open + checks
// running uses ○ so the user can tell it apart from closed-unmerged ⊘,
// which shares the red color but means something completely different.
//
// When the container isn't running we can't query gh, so the slot stays
// dim rather than red — matches renderGitIcon's handling.
func renderPRIcon(s *BranchStatus, state string) string {
	switch state {
	case "missing":
		return blank
	case "stopped":
		return iconPending.Render("·") + " "
	}
	if s == nil {
		return blank
	}
	if s.PRErr != "" {
		return gitError.Render("!") + " "
	}
	if !s.PRChecked {
		return iconPending.Render("·") + " "
	}
	if s.PR == nil {
		return iconMissing.Render("·") + " "
	}
	glyph, _, style := prGlyphLabelStyle(s.PR)
	return style.Render(glyph) + " "
}

// prGlyphLabelStyle returns the glyph, status label, and color for a PR row.
// Shared between the containers-column icon (glyph only) and the details
// pane (glyph + label) so the two stay in lockstep when a state is added or
// the palette is tweaked. Draft takes precedence over merged/closed so a
// draft PR that was later closed still reads as a draft visually.
func prGlyphLabelStyle(pr *PRStatus) (string, string, lipgloss.Style) {
	if pr.Draft {
		return "◌", "draft", prDraft
	}
	switch strings.ToUpper(pr.State) {
	case "MERGED":
		return "●", "merged", prStateMerged
	case "CLOSED":
		return "⊘", "closed", prStateClosed
	}
	switch pr.Checks {
	case "failed":
		return "!", "open, failed", prStateFailed
	case "checking":
		return "○", "open, checking", prStateChecking
	case "passed":
		return "◉", "open, passed", prStateOpen
	}
	return "◉", "open", prStateOpen
}
