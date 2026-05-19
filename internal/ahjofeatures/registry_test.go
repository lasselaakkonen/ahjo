package ahjofeatures

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLookupDocker(t *testing.T) {
	m, ok := Lookup("docker")
	if !ok {
		t.Fatal("Lookup(docker) returned !ok")
	}
	if m == nil {
		t.Fatal("Lookup(docker) returned nil materializer")
	}
	// Materializer should actually write the embedded dir contents to
	// disk — guards against a wiring mistake that registers an unrelated
	// function.
	dst := t.TempDir()
	if err := m(dst); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	for _, name := range []string{"devcontainer-feature.json", "install.sh"} {
		if _, err := filepath.Glob(filepath.Join(dst, name)); err != nil {
			t.Fatalf("expected %s in materialized dir: %v", name, err)
		}
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, ok := Lookup("postgres"); ok {
		t.Fatal("Lookup(postgres) returned ok; expected false")
	}
}

func TestListContainsDocker(t *testing.T) {
	got := List()
	if !strings.Contains(got, "docker") {
		t.Fatalf("List() = %q; expected to include docker", got)
	}
}

func TestLookupPreCommit(t *testing.T) {
	m, ok := Lookup("pre-commit")
	if !ok {
		t.Fatal("Lookup(pre-commit) returned !ok")
	}
	if m == nil {
		t.Fatal("Lookup(pre-commit) returned nil materializer")
	}
	dst := t.TempDir()
	if err := m(dst); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	for _, name := range []string{"devcontainer-feature.json", "install.sh"} {
		if _, err := filepath.Glob(filepath.Join(dst, name)); err != nil {
			t.Fatalf("expected %s in materialized dir: %v", name, err)
		}
	}
}

func TestListContainsPreCommit(t *testing.T) {
	got := List()
	if !strings.Contains(got, "pre-commit") {
		t.Fatalf("List() = %q; expected to include pre-commit", got)
	}
}
