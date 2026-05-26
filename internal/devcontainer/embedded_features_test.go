package devcontainer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestEmbeddedBaseFeaturesMaterialize is the base-feature mirror of
// internal/ahjofeatures' TestAllBuiltinsMaterialize: every Feature baked into
// ahjo-base (ahjo-runtime, ahjo-default-dev-tools) must materialize to a valid
// Feature dir without an Incus container — a devcontainer-feature.json whose
// id matches the registered id, plus an executable install.sh. Iterating
// embeddedBaseFeatures means a newly-added base Feature is covered the moment
// it's wired into the build pipeline.
func TestEmbeddedBaseFeaturesMaterialize(t *testing.T) {
	for _, ef := range embeddedBaseFeatures {
		t.Run(ef.id, func(t *testing.T) {
			dst := t.TempDir()
			if err := ef.materialize(dst); err != nil {
				t.Fatalf("materialize: %v", err)
			}

			jsonPath := filepath.Join(dst, "devcontainer-feature.json")
			b, err := os.ReadFile(jsonPath)
			if err != nil {
				t.Fatalf("read devcontainer-feature.json: %v", err)
			}
			var meta struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(b, &meta); err != nil {
				t.Fatalf("parse devcontainer-feature.json: %v", err)
			}
			if meta.ID != ef.id {
				t.Errorf("feature id %q != registered id %q", meta.ID, ef.id)
			}
			if st, _ := os.Stat(jsonPath); st != nil && st.Mode().Perm() != 0o644 {
				t.Errorf("devcontainer-feature.json mode %o, want 0644", st.Mode().Perm())
			}

			st, err := os.Stat(filepath.Join(dst, "install.sh"))
			if err != nil {
				t.Fatalf("stat install.sh: %v", err)
			}
			if st.Mode().Perm() != 0o755 {
				t.Errorf("install.sh mode %o, want 0755", st.Mode().Perm())
			}
		})
	}
}
