package ahjofeature_prek

import (
	"bytes"
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
// bit; mirror ahjofeature_docker's invariant.
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

// TestMetadataAcceptedByRejectDockerFields keeps the built-in Feature
// honest against the runner's docker-field rejector. The prek Feature
// has no business declaring mounts or privileged, so a future edit that
// adds either gets caught here.
func TestMetadataAcceptedByRejectDockerFields(t *testing.T) {
	dst := t.TempDir()
	if err := Materialize(dst); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := devcontainer.ReadMetadata(dst, FeatureID); err != nil {
		t.Fatalf("ReadMetadata rejected built-in Feature: %v", err)
	}
}

// TestInstallScriptUsesPrepareHooks guards the design invariant: the
// warm-up command must be `prek prepare-hooks`, NOT `prek install`.
// `prek install` writes a shim into .git/hooks (and `prek install
// --prepare-hooks` warms AND writes it), mutating the repo working tree
// on what the user opted into as a warm-up. If a future edit swaps the
// verb, this test fails before the surprise ships.
func TestInstallScriptUsesPrepareHooks(t *testing.T) {
	b, err := FeatureFS.ReadFile("feature/install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	if !bytes.Contains(b, []byte("prek prepare-hooks")) {
		t.Fatalf("install.sh must invoke `prek prepare-hooks` for hook warming")
	}
	// `prek install` (in any form) writes the git shim — reject it in any
	// non-comment line. `prek prepare-hooks` does not contain the
	// substring, so a plain per-line Contains check is enough.
	for _, line := range bytes.Split(b, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("#")) {
			continue
		}
		if bytes.Contains(trimmed, []byte("prek install")) {
			t.Fatalf("install.sh has `prek install` — would mutate .git/hooks: %q", string(line))
		}
	}
}
