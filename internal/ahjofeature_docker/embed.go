// Package ahjofeature_docker embeds the `ahjo/docker` built-in
// devcontainer Feature so the repo-add path can apply it without an OCI
// fetch. The upstream docker-in-docker / docker-outside-of-docker
// Features declare `mounts` and `privileged: true`, both of which
// ahjo's runner rejects. The setxattr syscall intercept, the btrfs
// rootfs, and systemd-as-PID 1 already provide the base surface Docker
// needs on an Incus system container (see CONTAINER-ISOLATION.md).
//
// security.nesting is declared via `customizations.ahjo.nesting: true` in
// this Feature's devcontainer-feature.json; ahjo reads that at repo-add
// time and enables nesting on the container before warm-install and
// lifecycle hooks run. This keeps the userns/overlayfs kernel attack
// surface closed for repos that don't declare Docker. The feature
// ships in the same release as the runtime it depends on.
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
