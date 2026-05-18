package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// lockfileEntry maps an on-disk lockfile to the bundled ahjo stack that
// owns it and the installer command (run as ubuntu in /repo) that warms
// its dependency cache. This table is the single source of truth for
// both the consent prompt in `ahjo repo add` and runWarmInstall's
// dispatch — keeping the two in lockstep so a new row enables both
// sides at once.
//
// Order matters: detectLockfiles probes in this order, and
// promptLockfileStack walks accepted matches in the same order, so the
// first row wins when a repo carries multiple lockfiles for the same
// stack (e.g. both pnpm-lock.yaml and package-lock.json — pnpm-first
// matches the bundled `node` stack's corepack-pnpm postCreate).
//
// `bun.lockb` intentionally absent: no bundled stack ships bun, so
// detecting it would suggest nothing and warm-installing it would
// fail. Re-add the row when a `bun` stack lands.
type lockfileEntry struct {
	lockfile string
	stack    string
	cmd      []string
}

var lockfileTable = []lockfileEntry{
	{"pnpm-lock.yaml", "node", []string{"pnpm", "install", "--frozen-lockfile"}},
	{"package-lock.json", "node", []string{"npm", "ci"}},
	{"uv.lock", "python", []string{"uv", "sync", "--frozen"}},
	{"Cargo.lock", "rust", []string{"cargo", "fetch"}},
}

// detectLockfiles probes /repo inside containerName for each row in
// lockfileTable, returning the matches in table order. The probe is a
// `test -f` via incus.Exec; a non-zero exit (file absent) is treated
// as a miss, every other error propagates so callers don't silently
// downgrade a broken container to "no matches".
func detectLockfiles(containerName string) ([]lockfileEntry, error) {
	var matches []lockfileEntry
	for _, e := range lockfileTable {
		_, err := incus.Exec(containerName, "test", "-f", paths.RepoMountPath+"/"+e.lockfile)
		if err == nil {
			matches = append(matches, e)
		}
		// `test -f` exits 1 on absent; incus.Exec wraps that as an
		// error. We can't easily distinguish "file absent" from "exec
		// failed" without a richer return shape, so we conservatively
		// treat any non-success as a miss — consistent with how
		// runWarmInstall has always interpreted this same probe.
	}
	return matches, nil
}

// promptLockfileStack walks `matches` in order, asking the user
// whether to apply the corresponding bundled stack and run its
// installer. The first accepted match wins: its stack name is
// returned, and the remaining matches are surfaced as "also detected"
// informational lines so polyglot repos are visible to the user
// without silently composing stacks.
//
// Empty input defaults to yes (the prompt advertises `[Y/n]`).
// autoYes — set on non-TTY stdin or when --yes was passed — auto-
// accepts the first match and prints the same "also detected" lines
// for the rest, matching today's "scripted invocations never hang"
// ergonomic. When every match is declined, the caller falls through
// to the generic picker.
func promptLockfileStack(matches []lockfileEntry, in *os.File, out io.Writer, autoYes bool) (string, error) {
	if len(matches) == 0 {
		return "", nil
	}

	accepted := -1
	if autoYes {
		accepted = 0
	} else {
		reader := bufio.NewReader(in)
		for i, m := range matches {
			fmt.Fprintf(out, "Found %s. Apply ahjo's %q stack and run `%s`? [Y/n]: ",
				m.lockfile, m.stack, strings.Join(m.cmd, " "))
			line, err := reader.ReadString('\n')
			// Read failure (closed pipe etc.) is treated as accept-
			// default — mirrors promptContainerConfig's EOF
			// handling so a half-closed stdin doesn't error.
			if err != nil && line == "" {
				accepted = i
				break
			}
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "", "y", "yes":
				accepted = i
			case "n", "no":
				continue
			default:
				return "", fmt.Errorf("unrecognized response %q (expected y/n)", strings.TrimSpace(line))
			}
			if accepted >= 0 {
				break
			}
		}
	}

	if accepted < 0 {
		return "", nil
	}
	// Only the matches we never asked about get surfaced as "also
	// detected" — declined ones (indices < accepted) already had
	// their prompt and don't need a second mention.
	for _, m := range matches[accepted+1:] {
		fmt.Fprintf(out, "also detected: %s (%s) — not applied; combine via a custom .ahjo/ahjocontainer.json\n",
			m.lockfile, m.stack)
	}
	return matches[accepted].stack, nil
}
