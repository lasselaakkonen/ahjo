// Package ahjofeature_pre_commit embeds the `ahjo/pre-commit` built-in
// devcontainer Feature so the repo-add path can apply it without an OCI
// fetch.
//
// Self-contained on purpose: the Feature installs its own python+pipx
// surface rather than depending on the bundled python stack, so a node-
// only / go-only repo that happens to carry a .pre-commit-config.yaml
// still gets its hook cache warmed. Composing it with the python stack
// is harmless (pipx is idempotent) but never required.
package ahjofeature_pre_commit

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
const FeatureID = "pre-commit"

// Materialize copies the embedded feature dir into dst (created with 0o755),
// preserving file modes. Mirrors internal/ahjofeature_docker/embed.go's
// Materialize so the two embed pipelines have the same shape.
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
