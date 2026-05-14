// Package ide is the shared vocabulary for SSH-capable IDE handling.
// Detection (per-OS, in detect_{darwin,linux}.go) returns the slugs that
// are launchable on this machine. Launching (per-OS, in launch_*.go) takes
// a slug + remote host alias + remote path and opens the IDE pointed at
// that target via ssh-remote.
package ide

// Slug identifiers. Stable strings — used as keys in lookup tables. Order
// in All() is the display order in the picker; keep most-popular-first so
// users don't have to scan.
const (
	Cursor         = "cursor"
	VSCode         = "vscode"
	VSCodeInsiders = "vscode-insiders"
	Windsurf       = "windsurf"
	Zed            = "zed"
)

// All returns every known slug in canonical display order. The shim only
// emits slugs that pass detection, the picker only renders slugs the shim
// emitted, but every consumer agrees on the order via this slice.
func All() []string {
	return []string{Cursor, VSCode, VSCodeInsiders, Windsurf, Zed}
}

// DisplayName is the user-facing label rendered in the picker for a slug.
// Unknown slugs round-trip as themselves so a future shim that knows about
// a new slug doesn't break older in-VM clients — they'll just show the raw
// slug instead of a friendly name.
func DisplayName(slug string) string {
	switch slug {
	case Cursor:
		return "Cursor"
	case VSCode:
		return "Visual Studio Code"
	case VSCodeInsiders:
		return "VS Code Insiders"
	case Windsurf:
		return "Windsurf"
	case Zed:
		return "Zed"
	}
	return slug
}

// IsKnown reports whether slug is one we recognize. Used by the daemon
// handler to reject unknown slugs without trying to construct a URL.
func IsKnown(slug string) bool {
	for _, s := range All() {
		if s == slug {
			return true
		}
	}
	return false
}
