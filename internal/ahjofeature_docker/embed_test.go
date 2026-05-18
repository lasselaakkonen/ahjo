package ahjofeature_docker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/devcontainer"
)

// TestFeatureMetadataEmbedded asserts the embedded Feature dir contains a
// valid devcontainer-feature.json whose ID matches the registry contract.
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
// devcontainer-feature.json at 0644 — Apply invokes install.sh through
// `bash install.sh`, but a future caller that exec's directly needs the
// bit; mirror ahjoruntime's invariant.
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

// TestMetadataAcceptedByRejectDockerFields guards against the
// upstream-shaped mistake — declaring `mounts:` or `privileged: true`
// in the built-in Feature would defeat the whole reason it exists.
// devcontainer.ReadMetadata runs rejectDockerFields; if the metadata
// ever grows one of those fields this test fails before the bad bits
// ship.
func TestMetadataAcceptedByRejectDockerFields(t *testing.T) {
	dst := t.TempDir()
	if err := Materialize(dst); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := devcontainer.ReadMetadata(dst, FeatureID); err != nil {
		t.Fatalf("ReadMetadata rejected built-in Feature: %v", err)
	}
}
