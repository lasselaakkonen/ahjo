//go:build !darwin

package terminal

import (
	"fmt"
	"os/exec"
	"strings"
)

// LaunchCommand spawns the terminal identified by slug pointed at argv as
// its initial command. asTab requests opening in a new tab of the running
// emulator (only meaningful when the caller is itself running inside that
// terminal); when the slug doesn't support tabs, the launcher silently
// falls back to a new window. Fire-and-forget — caller doesn't reap.
func LaunchCommand(slug string, argv []string, asTab bool) error {
	if len(argv) == 0 {
		return fmt.Errorf("terminal: empty argv")
	}
	switch slug {
	case GnomeTerminal:
		base := []string{"gnome-terminal"}
		if asTab {
			base = append(base, "--tab")
		}
		full := append(base, "--")
		full = append(full, argv...)
		return spawnDetached(full[0], full[1:]...)
	case Konsole:
		base := []string{"konsole"}
		if asTab {
			base = append(base, "--new-tab")
		}
		full := append(base, "-e")
		full = append(full, argv...)
		return spawnDetached(full[0], full[1:]...)
	case Kitty:
		if asTab {
			if _, err := exec.LookPath("kitty"); err == nil {
				args := append([]string{"@", "launch", "--type=tab"}, argv...)
				if err := exec.Command("kitty", args...).Start(); err == nil {
					return nil
				}
			}
		}
		return spawnDetached("kitty", argv...)
	case WezTerm:
		if asTab {
			args := append([]string{"cli", "spawn", "--"}, argv...)
			if err := exec.Command("wezterm", args...).Start(); err == nil {
				return nil
			}
		}
		args := append([]string{"start", "--"}, argv...)
		return spawnDetached("wezterm", args...)
	case Ghostty:
		args := []string{"-e", shellJoin(argv)}
		return spawnDetached("ghostty", args...)
	case Alacritty:
		args := append([]string{"-e"}, argv...)
		return spawnDetached("alacritty", args...)
	case Xterm:
		args := append([]string{"-e"}, argv...)
		return spawnDetached("xterm", args...)
	case Tilix:
		args := []string{"-e", shellJoin(argv)}
		return spawnDetached("tilix", args...)
	case Terminator:
		args := []string{"-x"}
		args = append(args, argv...)
		return spawnDetached("terminator", args...)
	}
	return fmt.Errorf("terminal: unknown slug %q", slug)
}

func spawnDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Start()
}

// shellJoin renders argv as a POSIX shell command string. Used for the
// emulators whose "run this command" flag is a shell string rather than an
// argv list (ghostty -e, tilix -e).
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}
