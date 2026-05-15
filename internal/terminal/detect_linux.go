//go:build !darwin

package terminal

import "os/exec"

// DetectInstalled returns the slugs of recognised terminal emulators whose
// launcher binaries are on PATH, in canonical display order. When Current()
// identifies the caller's own terminal and that slug is present in the list,
// it's moved to the front so the picker can show it first.
func DetectInstalled() []string {
	bins := map[string]string{
		Ghostty:       "ghostty",
		Kitty:         "kitty",
		WezTerm:       "wezterm",
		Alacritty:     "alacritty",
		GnomeTerminal: "gnome-terminal",
		Konsole:       "konsole",
		Tilix:         "tilix",
		Terminator:    "terminator",
		Xterm:         "xterm",
	}
	order := []string{Ghostty, Kitty, WezTerm, Alacritty, GnomeTerminal, Konsole, Tilix, Terminator, Xterm}
	var out []string
	for _, slug := range order {
		if _, err := exec.LookPath(bins[slug]); err == nil {
			out = append(out, slug)
		}
	}
	if cur, ok := Current(); ok {
		out = promoteCurrent(out, cur)
	}
	return out
}

func promoteCurrent(slugs []string, cur string) []string {
	for i, s := range slugs {
		if s == cur {
			if i == 0 {
				return slugs
			}
			out := make([]string, 0, len(slugs))
			out = append(out, cur)
			out = append(out, slugs[:i]...)
			out = append(out, slugs[i+1:]...)
			return out
		}
	}
	return slugs
}
