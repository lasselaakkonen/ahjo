package ahjofeature_docker

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

// TestNoLegacyGraphDriverDefault guards against the regression that
// caused the original ahjo/docker postgres-pull failure: install.sh
// writing `storage-driver: overlay2` (or any non-empty STORAGE_DRIVER
// default) into /etc/docker/daemon.json. On dockerd >=26 that key
// routes off the containerd snapshotter (xattr whiteouts, covered by
// security.syscalls.intercept.setxattr=true) onto the legacy graph
// driver (mknod-c-0-0 whiteouts, not reliably covered by the mknod
// intercept). The Feature must leave daemon.json absent by default and
// only write content when DAEMON_ARGS supplies it.
//
// The check is conservative: install.sh must not read a STORAGE_DRIVER
// env var, and devcontainer-feature.json must not advertise a
// storage_driver option. If a future change wants to bring either back,
// it has to update this test, which forces a conscious decision.
func TestNoLegacyGraphDriverDefault(t *testing.T) {
	t.Run("install.sh does not read STORAGE_DRIVER", func(t *testing.T) {
		b, err := FeatureFS.ReadFile("feature/install.sh")
		if err != nil {
			t.Fatalf("read install.sh: %v", err)
		}
		if bytes.Contains(b, []byte("STORAGE_DRIVER")) {
			t.Fatalf("install.sh references STORAGE_DRIVER; the option was removed " +
				"to keep dockerd on the containerd snapshotter by default")
		}
	})

	t.Run("manifest does not advertise storage_driver option", func(t *testing.T) {
		b, err := FeatureFS.ReadFile("feature/devcontainer-feature.json")
		if err != nil {
			t.Fatalf("read devcontainer-feature.json: %v", err)
		}
		var meta struct {
			Options map[string]any `json:"options"`
		}
		if err := json.Unmarshal(b, &meta); err != nil {
			t.Fatalf("parse devcontainer-feature.json: %v", err)
		}
		if _, ok := meta.Options["storage_driver"]; ok {
			t.Fatalf("devcontainer-feature.json advertises storage_driver option; " +
				"removed because setting it on dockerd >=26 in snapshotter mode " +
				"makes the daemon refuse to start. Callers needing the legacy " +
				"graph driver should use daemon_args.")
		}
	})
}
