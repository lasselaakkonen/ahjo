package cli

// mirror_test.go covers the orphan-recovery path: `ahjo mirror revert <target>`
// must restore a host target from a kept pre-mirror snapshot when no container
// or registry row remains. It drives a real `git` in t.TempDir() (skipping when
// git is absent) and isolates HOME so lockfile.Acquire's ~/.ahjo skeleton lands
// in the temp dir.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/mirror"
)

func gitCLI(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestRunMirrorRevert_Orphan(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("HOME", t.TempDir()) // lockfile.Acquire writes ~/.ahjo here

	dir := t.TempDir()
	gitCLI(t, dir, "init", "-q")
	gitCLI(t, dir, "config", "user.email", "test@example.com")
	gitCLI(t, dir, "config", "user.name", "Test")
	gitCLI(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, dir, "add", "a")
	gitCLI(t, dir, "commit", "-q", "-m", "init")

	if err := mirror.CaptureGit(dir, "feat"); err != nil {
		t.Fatal(err)
	}

	// Mirror clobbers a tracked file and adds an untracked one.
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("MIRRORED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "added.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No registry/container exists → liveMirrorContainerForTarget is a no-op and
	// the single snapshot is auto-selected.
	if err := runMirrorRevert(dir, ""); err != nil {
		t.Fatalf("runMirrorRevert: %v", err)
	}

	if b, _ := os.ReadFile(filepath.Join(dir, "a")); string(b) != "v1" {
		t.Errorf("a = %q, want v1 (tracked file restored)", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "added.txt")); !os.IsNotExist(err) {
		t.Error("added.txt should have been removed by the revert")
	}
	if mirror.RevertPossible(dir, "feat") {
		t.Error("snapshot should be consumed after a successful revert")
	}

	// Idempotent: nothing left to revert is not an error.
	if err := runMirrorRevert(dir, ""); err != nil {
		t.Fatalf("second runMirrorRevert: %v", err)
	}
}
