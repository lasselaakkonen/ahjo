package ahjoruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestFeatureMetadataEmbedded asserts the embedded Feature dir contains a
// valid devcontainer-feature.json whose ID matches the runner contract.
// Catches a broken embed path / wrong directory layout without needing an
// Incus container.
func TestFeatureMetadataEmbedded(t *testing.T) {
	b, err := FeatureFS.ReadFile("feature/devcontainer-feature.json")
	if err != nil {
		t.Fatalf("read embedded devcontainer-feature.json: %v", err)
	}
	var meta struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatalf("parse devcontainer-feature.json: %v", err)
	}
	if meta.ID != FeatureID {
		t.Fatalf("feature id mismatch: got %q want %q", meta.ID, FeatureID)
	}
}

// TestMaterializePreservesExecBit asserts install.sh lands at 0755 and
// devcontainer-feature.json at 0644 — the runner's `incus exec bash
// install.sh` doesn't strictly need the bit, but a future caller might.
func TestMaterializePreservesExecBit(t *testing.T) {
	dst := t.TempDir()
	if err := Materialize(dst); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	cases := map[string]os.FileMode{
		"install.sh":                0o755,
		"devcontainer-feature.json": 0o644,
	}
	for name, want := range cases {
		st, err := os.Stat(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if st.Mode().Perm() != want {
			t.Fatalf("%s: got mode %o want %o", name, st.Mode().Perm(), want)
		}
	}
}
