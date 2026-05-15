//go:build darwin

package terminal

import (
	"fmt"
	"os"
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
	case AppleTerminal:
		return launchAppleTerminal(argv, asTab)
	case ITerm:
		return launchITerm(argv, asTab)
	case WezTerm:
		return launchMacWezTerm(argv, asTab)
	case Ghostty:
		return launchMacGhostty(argv, asTab)
	case Alacritty:
		return launchMacOpenArgs("Alacritty", "-e", argv)
	case Kitty:
		return launchMacKitty(argv, asTab)
	}
	return fmt.Errorf("terminal: unknown slug %q", slug)
}

// launchAppleTerminal runs argv in Terminal.app. AppleScript's `do script`
// always opens a new tab (or window, per the user's Terminal prefs) — there
// is no reliable AppleScript-only way to force the new tab without
// Accessibility-permissioned keystroke injection, so we accept Terminal's
// pref as the final word on tab-vs-window even when asTab is true.
func launchAppleTerminal(argv []string, _ bool) error {
	cmd := shellJoin(argv)
	script := fmt.Sprintf(`tell application "Terminal"
activate
do script %q
end tell`, cmd)
	return spawnDetached("osascript", "-e", script)
}

// launchITerm runs argv in iTerm. iTerm exposes both
// `create window … command "cmd"` and `create tab … command "cmd"` via
// AppleScript, so tab-vs-window is honoured precisely. Falls back to
// creating a new window when no window is currently open.
func launchITerm(argv []string, asTab bool) error {
	cmd := shellJoin(argv)
	var script string
	if asTab {
		script = fmt.Sprintf(`tell application "iTerm"
activate
if (count of windows) is 0 then
create window with default profile command %q
else
tell current window to create tab with default profile command %q
end if
end tell`, cmd, cmd)
	} else {
		script = fmt.Sprintf(`tell application "iTerm"
activate
create window with default profile command %q
end tell`, cmd)
	}
	return spawnDetached("osascript", "-e", script)
}

// launchMacWezTerm prefers `wezterm cli spawn` for tabs (talks to the
// running GUI instance) and falls back to `open -na WezTerm.app --args
// start --` for new windows. When asTab is true but `wezterm` isn't on
// PATH, we fall through to the window form.
func launchMacWezTerm(argv []string, asTab bool) error {
	if asTab {
		if _, err := exec.LookPath("wezterm"); err == nil {
			args := append([]string{"cli", "spawn", "--"}, argv...)
			return spawnDetached("wezterm", args...)
		}
	}
	args := append([]string{"-na", "WezTerm.app", "--args", "start", "--"}, argv...)
	return spawnDetached("open", args...)
}

// launchMacGhostty drives Ghostty via its AppleScript dictionary (1.3+).
// Ghostty tokenises the `command` surface-config string with shell rules
// and then `exec -l`s those tokens directly (inside a `bash --noprofile
// --norc` wrapper), so a bare command runs without the user's profile —
// limactl/incus aren't on PATH. Routing through `$SHELL -lc …` instead
// puts the user's login shell in front of our command so .zprofile /
// .bash_profile / etc. run and PATH is populated. `input text` would
// avoid this entirely, but it races the surface startup and gets dropped.
func launchMacGhostty(argv []string, asTab bool) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	target := "window"
	if asTab {
		target = "tab"
	}
	// shellJoin POSIX-quotes each argv element; we then hand that joined
	// string to `$SHELL -lc` as a single double-quoted token so Ghostty's
	// tokeniser keeps it intact across the boundary.
	value := fmt.Sprintf("%s -lc %q", shell, shellJoin(argv))
	script := fmt.Sprintf(`tell application "Ghostty"
activate
set cfg to new surface configuration
set command of cfg to %q
new %s with configuration cfg
end tell`, value, target)
	return spawnDetached("osascript", "-e", script)
}

// launchMacOpenArgs is the generic `open -na <App>.app --args <flag>
// <argv...>` form for Mac terminals whose argv parses a "run this command"
// list (Alacritty's -e).
func launchMacOpenArgs(appName, runFlag string, argv []string) error {
	full := []string{"-na", appName + ".app", "--args", runFlag}
	full = append(full, argv...)
	return spawnDetached("open", full...)
}

// launchMacKitty prefers `kitty @ launch --type=tab` when asTab is true and
// remote control is reachable; otherwise opens a new window via the kitty
// CLI directly (which is more reliable than `open -na` for argv passing).
func launchMacKitty(argv []string, asTab bool) error {
	if asTab {
		if _, err := exec.LookPath("kitty"); err == nil {
			args := append([]string{"@", "launch", "--type=tab"}, argv...)
			if err := exec.Command("kitty", args...).Start(); err == nil {
				return nil
			}
		}
	}
	if _, err := exec.LookPath("kitty"); err == nil {
		return spawnDetached("kitty", argv...)
	}
	full := []string{"-na", "kitty.app", "--args"}
	full = append(full, argv...)
	return spawnDetached("open", full...)
}

func spawnDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Start()
}

// shellJoin renders argv as a POSIX shell command string. Each token is
// wrapped in single quotes with embedded single quotes escaped via the
// `'\”` idiom; safe to paste into bash/sh and into AppleScript `do
// script` payloads.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}
