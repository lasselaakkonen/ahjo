package git

import (
	"fmt"
	"strconv"
	"strings"
)

// StatusCounts summarizes the output of
// `git status --porcelain=v2 --branch`: how many entries are staged,
// unstaged, untracked, or unmerged, plus how many local commits are ahead of
// the upstream branch.
type StatusCounts struct {
	Staged    int
	Unstaged  int
	Untracked int
	Unmerged  int
	Ahead     int
}

// ParseStatusV2 parses `git status --porcelain=v2 --branch` output. It is a
// pure function over the captured text so both the in-container check (via
// incus exec) and the host-target check (via direct git) can share it.
func ParseStatusV2(out string) StatusCounts {
	var c StatusCounts
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# branch.ab "):
			// "# branch.ab +<ahead> -<behind>"
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				if v, err := strconv.Atoi(strings.TrimPrefix(fields[2], "+")); err == nil {
					c.Ahead = v
				}
			}
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "):
			// "1 XY ..." or "2 XY ..."; XY is the second token. X = staged,
			// Y = unstaged; "." means unchanged for that half.
			fields := strings.Fields(line)
			if len(fields) < 2 || len(fields[1]) < 2 {
				continue
			}
			if fields[1][0] != '.' {
				c.Staged++
			}
			if fields[1][1] != '.' {
				c.Unstaged++
			}
		case strings.HasPrefix(line, "u "):
			c.Unmerged++
		case strings.HasPrefix(line, "? "):
			c.Untracked++
		}
	}
	return c
}

// WorkTreeDirty reports whether the work tree carries uncommitted changes that
// a file-level overwrite would clobber: staged, unstaged, untracked, or
// unmerged entries. Unpushed commits (Ahead) are excluded — they live in
// .git, which is never overwritten by a file mirror.
func (c StatusCounts) WorkTreeDirty() bool {
	return c.Staged > 0 || c.Unstaged > 0 || c.Untracked > 0 || c.Unmerged > 0
}

// Summary renders every set count, including unpushed commits, as a short
// human-readable phrase ("3 staged, 1 untracked, 2 unpushed commit(s)"), or
// "" when nothing is set.
func (c StatusCounts) Summary() string { return c.summary(true) }

// WorkTreeSummary renders only the work-tree counts (omits unpushed commits),
// or "" when the work tree is clean.
func (c StatusCounts) WorkTreeSummary() string { return c.summary(false) }

func (c StatusCounts) summary(includeAhead bool) string {
	var parts []string
	if c.Staged > 0 {
		parts = append(parts, fmt.Sprintf("%d staged", c.Staged))
	}
	if c.Unstaged > 0 {
		parts = append(parts, fmt.Sprintf("%d unstaged", c.Unstaged))
	}
	if c.Untracked > 0 {
		parts = append(parts, fmt.Sprintf("%d untracked", c.Untracked))
	}
	if c.Unmerged > 0 {
		parts = append(parts, fmt.Sprintf("%d unmerged", c.Unmerged))
	}
	if includeAhead && c.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("%d unpushed commit(s)", c.Ahead))
	}
	return strings.Join(parts, ", ")
}
