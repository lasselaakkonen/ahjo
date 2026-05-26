package ahjofeatures

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wantBuiltins is the canonical list of user-addressable `ahjo/<name>`
// built-in Features. A new built-in package must register itself in the table
// AND land here — mirrors internal/stacks' wantStacks guard so "I added a
// Feature package but forgot to wire it" fails CI instead of in the field.
var wantBuiltins = []string{"docker", "prek"}

// TestTableMatchesWantBuiltins keeps the registry and the documented set in
// lockstep.
func TestTableMatchesWantBuiltins(t *testing.T) {
	got := List() // sorted, comma-joined
	want := strings.Join(wantBuiltins, ", ")
	if got != want {
		t.Fatalf("List() = %q, want %q (update wantBuiltins or the registry table)", got, want)
	}
}

// TestAllBuiltinsMaterialize is the feature-side mirror of internal/stacks'
// TestLoad_AllStacksParseAndCarryFeatures: every registered built-in must
// materialize to a valid Feature dir without an Incus container — a
// devcontainer-feature.json whose id matches the registry key, plus an
// executable install.sh. Catches a broken embed path, a mis-wired
// Materializer, or an id/key drift.
func TestAllBuiltinsMaterialize(t *testing.T) {
	for _, name := range wantBuiltins {
		t.Run(name, func(t *testing.T) {
			m, ok := Lookup(name)
			if !ok || m == nil {
				t.Fatalf("Lookup(%q) = (%v, %v); want a non-nil materializer", name, m, ok)
			}
			dst := t.TempDir()
			if err := m(dst); err != nil {
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
			if meta.ID != name {
				t.Errorf("feature id %q != registry key %q", meta.ID, name)
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

func TestLookupUnknown(t *testing.T) {
	if _, ok := Lookup("postgres"); ok {
		t.Fatal("Lookup(postgres) returned ok; expected false")
	}
}
