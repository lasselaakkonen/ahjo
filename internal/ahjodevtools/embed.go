// Package ahjodevtools embeds the ahjo-default-dev-tools devcontainer
// Feature. ahjo bakes this into ahjo-base alongside ahjo-runtime so every
// container ahjo creates ships with the small CLI utilities ahjo's
// preferred workflows assume (rg, fd, eza, yq, ast-grep, httpie, make).
// Tools that already have a curated upstream Feature (common-utils, git,
// github-cli) are not duplicated here — they're applied as their own
// Features ahead of this one in the build pipeline.
package ahjodevtools

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:feature
var FeatureFS embed.FS

const FeatureID = "ahjo-default-dev-tools"

// Materialize copies the embedded feature dir into dst (created with 0o755),
// preserving exec bit on shell scripts. Mirrors ahjoruntime.Materialize so
// the build pipeline drives both packages with the same shape.
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
