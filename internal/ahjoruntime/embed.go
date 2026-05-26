// Package ahjoruntime embeds the ahjo-runtime devcontainer Feature so the
// build pipeline can apply it against a fresh images:ubuntu/24.04 container
// without depending on a checked-out repo.
package ahjoruntime

import (
	"embed"

	"github.com/lasselaakkonen/ahjo/internal/featurefs"
)

//go:embed all:feature
var FeatureFS embed.FS

// FeatureID is the Feature's stable identifier as declared in
// devcontainer-feature.json. Used by the runner for tmp-dir naming and
// error messages.
const FeatureID = "ahjo-runtime"

// Materialize copies the embedded feature dir into dst. The runner pushes dst
// into the build container, where install.sh runs as root with the
// devcontainer Feature env vars set. Mode handling (exec bit on *.sh) is
// shared across every built-in Feature via featurefs.Materialize.
func Materialize(dst string) error {
	return featurefs.Materialize(FeatureFS, dst)
}
