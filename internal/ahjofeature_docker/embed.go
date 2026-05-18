// Package ahjofeature_docker embeds the `ahjo/docker` built-in
// devcontainer Feature so the repo-add path can apply it without an OCI
// fetch. The upstream docker-in-docker / docker-outside-of-docker
// Features declare `mounts` and `privileged: true`, both of which
// ahjo's runner rejects because the runtime profile
// (security.nesting=true + mknod/setxattr intercepts, btrfs rootfs,
// systemd PID 1 — see CONTAINER-ISOLATION.md) already provides the
// kernel surface Docker needs. This Feature is the ahjo-shaped
// equivalent and ships in the same release as the profile it depends
// on.
package ahjofeature_docker

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
const FeatureID = "docker"

// Materialize copies the embedded feature dir into dst (created with 0o755),
// preserving file modes. The runner pushes dst into the per-repo container,
// where install.sh runs as root with the devcontainer Feature env vars set.
// install.sh keeps its embedded executable bit; the JSON sits at 0644.
//
// Mirrors internal/ahjoruntime/embed.go's Materialize so the two embed
// pipelines have the same shape — adding more built-in Features means
// copying this file and changing FeatureID + the embedded dir.
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
