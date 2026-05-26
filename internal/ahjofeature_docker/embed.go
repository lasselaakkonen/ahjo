// Package ahjofeature_docker embeds the `ahjo/docker` built-in
// devcontainer Feature so the repo-add path can apply it without an OCI
// fetch. The upstream docker-in-docker / docker-outside-of-docker
// Features declare `mounts` and `privileged: true`, both of which
// ahjo's runner rejects because the runtime profile
// (security.nesting=true + setxattr/mknod intercepts, btrfs rootfs,
// systemd PID 1 — see CONTAINER-ISOLATION.md) already provides the
// kernel surface Docker needs. Specifically, dockerd >=26 defaults to
// the containerd snapshotter, whose layer whiteouts are xattrs handled
// by the profile's setxattr intercept; the mknod intercept is retained
// for defense in depth but is not what makes pulls work. This Feature
// is the ahjo-shaped equivalent and ships in the same release as the
// profile it depends on.
package ahjofeature_docker

import (
	"embed"

	"github.com/lasselaakkonen/ahjo/internal/featurefs"
)

//go:embed all:feature
var FeatureFS embed.FS

// FeatureID is the Feature's stable identifier as declared in
// devcontainer-feature.json. Used by the runner for tmp-dir naming and
// error messages.
const FeatureID = "docker"

// Materialize copies the embedded feature dir into dst. The runner pushes dst
// into the per-repo container, where install.sh runs as root with the
// devcontainer Feature env vars set. Mode handling (exec bit on *.sh) is
// shared across every built-in Feature via featurefs.Materialize. Adding more
// built-in Features means copying this file and changing FeatureID + the
// embedded dir.
func Materialize(dst string) error {
	return featurefs.Materialize(FeatureFS, dst)
}
