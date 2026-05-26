// Package ahjofeature_prek embeds the `ahjo/prek` built-in devcontainer
// Feature so the repo-add path can apply it without an OCI fetch.
//
// prek is a dependency-free, Rust-based reimplementation of pre-commit:
// a single static binary with no Python runtime. That makes this Feature
// genuinely self-contained — a node-only / go-only repo that happens to
// carry a .pre-commit-config.yaml still gets its hook cache warmed,
// without staging a python surface. prek reads the existing
// .pre-commit-config.yaml as-is.
package ahjofeature_prek

import (
	"embed"

	"github.com/lasselaakkonen/ahjo/internal/featurefs"
)

//go:embed all:feature
var FeatureFS embed.FS

// FeatureID is the Feature's stable identifier as declared in
// devcontainer-feature.json. Used by the runner for tmp-dir naming and
// error messages.
const FeatureID = "prek"

// Materialize copies the embedded feature dir into dst. Mode handling (exec
// bit on *.sh) is shared across every built-in Feature via
// featurefs.Materialize.
func Materialize(dst string) error {
	return featurefs.Materialize(FeatureFS, dst)
}
