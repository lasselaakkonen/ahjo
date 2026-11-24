package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateMirrorTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range []string{".ssh", ".claude", "Library/Application Support", "code/existing"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A link inside an allowed directory that leads into ~/.ssh.
	if err := os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(home, "code", "sneaky")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()

	rejected := []string{
		home,
		filepath.Dir(home),
		"/",
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".ssh", "x"),
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".ahjo", "mirror"),
		filepath.Join(home, ".not-yet", "deep", "dir"),
		filepath.Join(home, "Library", "x"),
		filepath.Join(home, "code", "sneaky"),
		filepath.Join(home, "code", "sneaky", "sub"),
		outside,
		filepath.Join(outside, "x"),
	}
	for _, p := range rejected {
		if err := validateMirrorTarget(p); err == nil {
			t.Errorf("validateMirrorTarget(%q) accepted", p)
		}
	}
	accepted := []string{
		filepath.Join(home, "code", "existing"),
		filepath.Join(home, "code", "new", "deeper"),
		filepath.Join(home, "mirror-target"),
		filepath.Join(home, "code", ".hidden-project"),
	}
	for _, p := range accepted {
		if err := validateMirrorTarget(p); err != nil {
			t.Errorf("validateMirrorTarget(%q) = %v, want ok", p, err)
		}
	}
}
