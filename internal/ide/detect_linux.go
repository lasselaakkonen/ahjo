//go:build !darwin

package ide

import "os/exec"

// DetectInstalled returns slugs whose CLI shims are on PATH. Used as the
// bare-Linux fallback when AHJO_HOST_IDES is unset (i.e., ahjo is running
// outside the Mac shim → VM relay).
func DetectInstalled() []string {
	bins := map[string]string{
		Cursor:         "cursor",
		VSCode:         "code",
		VSCodeInsiders: "code-insiders",
		Windsurf:       "windsurf",
		Zed:            "zed",
	}
	var out []string
	for _, slug := range All() {
		if _, err := exec.LookPath(bins[slug]); err == nil {
			out = append(out, slug)
		}
	}
	return out
}
