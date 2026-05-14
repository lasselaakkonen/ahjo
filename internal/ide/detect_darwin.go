//go:build darwin

package ide

import (
	"os"
	"path/filepath"
)

// DetectInstalled returns the slugs whose .app bundles are present in
// /Applications or ~/Applications, in canonical display order. The Mac
// shim calls this before relaying into the VM and serializes the result
// into AHJO_HOST_IDES.
func DetectInstalled() []string {
	bundles := map[string]string{
		Cursor:         "Cursor.app",
		VSCode:         "Visual Studio Code.app",
		VSCodeInsiders: "Visual Studio Code - Insiders.app",
		Windsurf:       "Windsurf.app",
		Zed:            "Zed.app",
	}
	var out []string
	for _, slug := range All() {
		if hasAppBundle(bundles[slug]) {
			out = append(out, slug)
		}
	}
	return out
}

// hasAppBundle reports whether <name>.app exists in either the system or
// per-user Applications directory.
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
