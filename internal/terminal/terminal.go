// Package terminal is the shared vocabulary for the host's terminal-emulator
// picker in `ahjo top`. Detection (per-OS, in detect_{darwin,linux}.go)
// returns the slugs of installed emulators; launching (per-OS, in launch_*.go)
// takes a slug, the argv to run, and a tab-vs-window hint, and spawns the
// emulator pointed at that command. Current() identifies which slug the
// caller is itself running inside, so the picker can list it first.
package terminal

import "os"

// Slug identifiers. Stable strings shared across platforms, even when only
// one OS implements detection/launch for a given slug — Current() may return
// any of these from environment heuristics regardless of build tag.
const (
	AppleTerminal = "apple-terminal"
	ITerm         = "iterm"
	WezTerm       = "wezterm"
	Alacritty     = "alacritty"
	Kitty         = "kitty"
	Ghostty       = "ghostty"
	GnomeTerminal = "gnome-terminal"
	Konsole       = "konsole"
	Xterm         = "xterm"
	Tilix         = "tilix"
	Terminator    = "terminator"
)

// DisplayName is the user-facing label rendered in the picker. Unknown slugs
// round-trip as themselves so a future build that learns a new slug doesn't
// crash an older picker.
func DisplayName(slug string) string {
	switch slug {
	case AppleTerminal:
		return "Terminal"
	case ITerm:
		return "iTerm"
	case WezTerm:
		return "WezTerm"
	case Alacritty:
		return "Alacritty"
	case Kitty:
		return "kitty"
	case Ghostty:
		return "Ghostty"
	case GnomeTerminal:
		return "GNOME Terminal"
	case Konsole:
		return "Konsole"
	case Xterm:
		return "xterm"
	case Tilix:
		return "Tilix"
	case Terminator:
		return "Terminator"
	}
	return slug
}

// Current returns the slug of the terminal the current process is running
// inside, based on environment variables. Best-effort: parent-process
// inspection is intentionally out of scope. Returns ("", false) when no
// signal identifies a known terminal — callers should treat that as "no
// current terminal to prefer" rather than an error.
//
// Signal priority:
//  1. $TERM_PROGRAM — set by Apple Terminal, iTerm, vscode, WezTerm, ghostty.
//  2. $LC_TERMINAL — set by some terminals as a secondary marker (notably
//     iTerm).
//  3. $TERMINAL — occasionally set by Linux users to indicate their default.
//  4. $TERM — fall back to terminfo-style names like xterm-ghostty.
func Current() (string, bool) {
	switch os.Getenv("TERM_PROGRAM") {
	case "Apple_Terminal":
		return AppleTerminal, true
	case "iTerm.app":
		return ITerm, true
	case "WezTerm":
		return WezTerm, true
	case "ghostty":
		return Ghostty, true
	}
	if os.Getenv("LC_TERMINAL") == "iTerm2" {
		return ITerm, true
	}
	if v := os.Getenv("TERMINAL"); v != "" {
		if slug, ok := slugFromBinary(v); ok {
			return slug, true
		}
	}
	switch os.Getenv("TERM") {
	case "xterm-ghostty":
		return Ghostty, true
	case "xterm-kitty":
		return Kitty, true
	case "alacritty":
		return Alacritty, true
	case "wezterm":
		return WezTerm, true
	}
	return "", false
}

// slugFromBinary maps a $TERMINAL binary name (possibly a full path) to a
// known slug.
func slugFromBinary(v string) (string, bool) {
	// Strip any directory component without importing path/filepath — keeps
	// this file dependency-free.
	for i := len(v) - 1; i >= 0; i-- {
		if v[i] == '/' {
			v = v[i+1:]
			break
		}
	}
	switch v {
	case "alacritty":
		return Alacritty, true
	case "kitty":
		return Kitty, true
	case "wezterm":
		return WezTerm, true
	case "ghostty":
		return Ghostty, true
	case "gnome-terminal":
		return GnomeTerminal, true
	case "konsole":
		return Konsole, true
	case "xterm":
		return Xterm, true
	case "tilix":
		return Tilix, true
	case "terminator":
		return Terminator, true
	}
	return "", false
}
