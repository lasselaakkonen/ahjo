//go:build darwin

package terminal

import (
	"os"
	"path/filepath"
)

// DetectInstalled returns the slugs of recognised terminal emulators that
// have a .app bundle in /Applications or ~/Applications, in canonical
// display order. When Current() identifies the caller's own terminal and
// that slug is present in the list, it's moved to the front so the picker
// can show it first.
func DetectInstalled() []string {
	bundles := map[string]string{
		Ghostty:       "Ghostty.app",
		ITerm:         "iTerm.app",
		AppleTerminal: "", // Terminal.app is built into the OS; treat as always present
		WezTerm:       "WezTerm.app",
		Alacritty:     "Alacritty.app",
		Kitty:         "kitty.app",
	}
	order := []string{Ghostty, ITerm, AppleTerminal, WezTerm, Alacritty, Kitty}
	var out []string
	for _, slug := range order {
		if slug == AppleTerminal || hasAppBundle(bundles[slug]) {
			out = append(out, slug)
		}
	}
	if cur, ok := Current(); ok {
		out = promoteCurrent(out, cur)
	}
	return out
}

func hasAppBundle(name string) bool {
	candidates := []string{filepath.Join("/Applications", name)}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Applications", name))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
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
