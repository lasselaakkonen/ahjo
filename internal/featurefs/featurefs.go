// Package featurefs holds the one shared routine that every built-in
// devcontainer Feature package uses to copy its embedded files onto disk.
//
// It is a leaf package (stdlib-only) on purpose: the Feature packages
// (ahjoruntime, ahjodevtools, ahjofeature_docker, …) import it, and some of
// those are in turn imported by internal/devcontainer, so the shared helper
// can't live in devcontainer without an import cycle.
package featurefs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Materialize copies the "feature" subtree embedded in src into dst (created
// 0o755), giving *.sh files the 0o755 exec bit and everything else 0o644. The
// runner pushes dst into the target container, where install.sh runs as root
// with the devcontainer Feature env vars set.
//
// Every built-in Feature package embeds its files under `all:feature` and
// delegates here, so adding a built-in Feature is an embed directive plus a
// one-line Materialize wrapper.
func Materialize(src fs.FS, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	return fs.WalkDir(src, "feature", func(p string, d fs.DirEntry, err error) error {
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
		b, err := fs.ReadFile(src, p)
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
