package ahjofeature_prek

import (
	"bytes"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/devcontainer"
)

// Embed validity (devcontainer-feature.json id matches FeatureID, install.sh
// at 0755) is covered generically by ahjofeatures' TestAllBuiltinsMaterialize.
// The tests below are prek-specific.

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
