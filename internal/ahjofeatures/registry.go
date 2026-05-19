// Package ahjofeatures is the lookup table for `ahjo/<name>` built-in
// devcontainer Features — Features shipped embedded in the ahjo binary
// rather than fetched from an OCI registry. Each entry is a Feature
// whose installation depends on ahjo's runtime profile
// (security.nesting=true + mknod/setxattr intercepts, btrfs rootfs,
// systemd PID 1 — see CONTAINER-ISOLATION.md), so the upstream
// equivalent's `mounts:` / `privileged: true` declarations would be
// rejected by the runner. Bundling them with the binary means the
// install logic ships at the same version as the profile it expects.
//
// Built-in Features must NOT declare `dependsOn` on upstream OCI
// Features in v1: the fetcher dispatch in internal/cli/features.go
// short-circuits the ParseFeatureRef path for `ahjo/*` keys, so a
// dependsOn chain would currently re-enter the OCI path with an
// unparseable ref. If a future built-in needs to chain on an OCI dep,
// lift the prefix dispatch into Resolve so transitive refs go through
// the same gate.
//
// Adding a new built-in Feature means: create
// `internal/ahjofeature_<name>/` mirroring `internal/ahjofeature_docker/`
// (FeatureID const + Materialize + embed.FS over a `feature/` dir), then
// register it in the table below.
package ahjofeatures

import (
	"sort"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/ahjofeature_docker"
	"github.com/lasselaakkonen/ahjo/internal/ahjofeature_pre_commit"
)

// Materializer copies an embedded Feature dir into dst. Signature
// matches internal/ahjofeature_docker.Materialize and
// internal/ahjoruntime.Materialize so the two embed pipelines stay
// interchangeable.
type Materializer func(dst string) error

// table is the lookup; the key is the leaf after `ahjo/` in the
// addressing form (e.g. `ahjo/docker` → "docker"). Keys must be
// lowercase ASCII; users get `ahjo/<name>` echoed back verbatim in
// errors so the case must match what the docs show.
var table = map[string]Materializer{
	ahjofeature_docker.FeatureID:     ahjofeature_docker.Materialize,
	ahjofeature_pre_commit.FeatureID: ahjofeature_pre_commit.Materialize,
}

// Lookup returns the materializer for name and true, or nil/false if no
// built-in by that name is registered. name is the bare leaf — callers
// strip the `ahjo/` prefix before calling.
func Lookup(name string) (Materializer, bool) {
	m, ok := table[name]
	return m, ok
}

// List returns the registered built-in Feature names, sorted. Used by
// the fetcher dispatch's error message so a user typo
// (`ahjo/dockerd`) gets pointed at the real options.
func List() string {
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
