// Package ahjodevtools embeds the ahjo-default-dev-tools devcontainer
// Feature. ahjo bakes this into ahjo-base alongside ahjo-runtime so every
// container ahjo creates ships with the small CLI utilities ahjo's
// preferred workflows assume (rg, fd, eza, yq, ast-grep, httpie, make, rtk).
// Tools that already have a curated upstream Feature (common-utils, git,
// github-cli) are not duplicated here — they're applied as their own
// Features ahead of this one in the build pipeline.
package ahjodevtools

import (
	"embed"

	"github.com/lasselaakkonen/ahjo/internal/featurefs"
)

//go:embed all:feature
var FeatureFS embed.FS

const FeatureID = "ahjo-default-dev-tools"

// Materialize copies the embedded feature dir into dst. Mode handling (exec
// bit on *.sh) is shared with every built-in Feature via featurefs.Materialize.
func Materialize(dst string) error {
	return featurefs.Materialize(FeatureFS, dst)
}
