package featurefs

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// TestMaterialize_ModesAndLayout pins the shared contract every built-in
// Feature package now relies on: the "feature" subtree is copied into dst with
// *.sh at 0o755 and everything else at 0o644, nested dirs preserved, and files
// outside "feature" ignored.
func TestMaterialize_ModesAndLayout(t *testing.T) {
	src := fstest.MapFS{
		"feature/devcontainer-feature.json": {Data: []byte(`{"id":"x"}`)},
		"feature/install.sh":                {Data: []byte("#!/bin/sh\n")},
		"feature/lib/helper.sh":             {Data: []byte("echo hi\n")},
		"feature/lib/data.txt":              {Data: []byte("data\n")},
		"README.md":                         {Data: []byte("ignored\n")}, // outside feature/
	}
	dst := t.TempDir()
	if err := Materialize(src, dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	wantModes := map[string]os.FileMode{
		"devcontainer-feature.json": 0o644,
		"install.sh":                0o755,
		"lib/helper.sh":             0o755,
		"lib/data.txt":              0o644,
	}
	for name, want := range wantModes {
		st, err := os.Stat(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s: mode %o, want %o", name, st.Mode().Perm(), want)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "README.md")); !os.IsNotExist(err) {
		t.Errorf("README.md outside feature/ should not be copied (err=%v)", err)
	}
}
