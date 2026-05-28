// Package spawn holds the tiny, feature-agnostic process-launch helpers shared
// by the terminal and ide launchers: starting a fully detached child, and
// rendering an argv as a POSIX shell command string. Both packages launch GUI
// apps fire-and-forget, so the logic is identical and lives here once rather
// than copy-pasted into each platform file.
package spawn

import (
	"os/exec"
	"strings"
)

// Detached starts name+args as a fire-and-forget child with no stdio wired up,
// returning as soon as the child is spawned. The caller does not reap it — the
// GUI app it dispatches to is the real long-running process.
func Detached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Start()
}

// ShellJoin renders argv as a POSIX shell command string: each token wrapped
// in single quotes with embedded single quotes escaped via the `'\”` idiom.
// Used for the emulators whose "run this command" flag is a shell string
// rather than an argv list (ghostty -e, tilix -e) and for AppleScript
// `do script` payloads.
func ShellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}
