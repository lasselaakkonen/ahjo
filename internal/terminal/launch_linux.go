//go:build !darwin

package terminal

import (
	"fmt"
	"os/exec"

	"github.com/lasselaakkonen/ahjo/internal/spawn"
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
		return spawn.Detached(full[0], full[1:]...)
	case Konsole:
		base := []string{"konsole"}
		if asTab {
			base = append(base, "--new-tab")
		}
		full := append(base, "-e")
		full = append(full, argv...)
		return spawn.Detached(full[0], full[1:]...)
	case Kitty:
		if asTab {
			if _, err := exec.LookPath("kitty"); err == nil {
				args := append([]string{"@", "launch", "--type=tab"}, argv...)
				if err := exec.Command("kitty", args...).Start(); err == nil {
					return nil
				}
			}
		}
		return spawn.Detached("kitty", argv...)
	case WezTerm:
		if asTab {
			args := append([]string{"cli", "spawn", "--"}, argv...)
			if err := exec.Command("wezterm", args...).Start(); err == nil {
				return nil
			}
		}
		args := append([]string{"start", "--"}, argv...)
		return spawn.Detached("wezterm", args...)
	case Ghostty:
		args := []string{"-e", spawn.ShellJoin(argv)}
		return spawn.Detached("ghostty", args...)
	case Alacritty:
		args := append([]string{"-e"}, argv...)
		return spawn.Detached("alacritty", args...)
	case Xterm:
		args := append([]string{"-e"}, argv...)
		return spawn.Detached("xterm", args...)
	case Tilix:
		args := []string{"-e", spawn.ShellJoin(argv)}
		return spawn.Detached("tilix", args...)
	case Terminator:
		args := []string{"-x"}
		args = append(args, argv...)
		return spawn.Detached("terminator", args...)
	}
	return fmt.Errorf("terminal: unknown slug %q", slug)
}
