// Package ahjoruntime embeds the ahjo-runtime devcontainer Feature so the
// build pipeline can apply it against a fresh images:ubuntu/24.04 container
// without depending on a checked-out repo. This is the in-tree replacement
// for the old internal/coi/assets/profiles/ahjo-base profile.
package ahjoruntime

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:feature
var FeatureFS embed.FS

// FeatureID is the Feature's stable identifier as declared in
// devcontainer-feature.json. Used by the runner for tmp-dir naming and
// error messages.
const FeatureID = "ahjo-runtime"

// Materialize copies the embedded feature dir into dst (created with 0o755),
// preserving file modes. The runner pushes dst into the build container,
// where install.sh runs as root with the devcontainer Feature env vars set.
// install.sh keeps its embedded executable bit; the JSON sits at 0644.
func Materialize(dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	return fs.WalkDir(FeatureFS, "feature", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("feature", p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := FeatureFS.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", p, err)
		}
		mode := os.FileMode(0o644)
		if filepath.Ext(rel) == ".sh" {
			mode = 0o755
		}
		return os.WriteFile(out, b, mode)
	})
}
