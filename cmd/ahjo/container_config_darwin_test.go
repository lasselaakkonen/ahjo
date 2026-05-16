//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLooksLikeHostPath(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"node", false},
		{"my-stack", false},
		{"ci_2", false},
		{"bare", false},
		{"./foo.json", true},
		{"../shared/dev.json", true},
		{"/abs/path/cfg.json", true},
		{"~/cfg.json", true},
		{"some/relative.json", true},
		{"plain-name.json", true}, // .json suffix flags it as a path
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			if got := looksLikeHostPath(c.input); got != c.want {
				t.Fatalf("looksLikeHostPath(%q) = %v, want %v", c.input, got, c.want)
			}
		})
	}
}

func TestStageContainerConfigPaths_LeavesIdentifiersAlone(t *testing.T) {
	cases := [][]string{
		{"repo", "add", "https://github.com/x/y.git", "--container-config", "node"},
		{"repo", "add", "https://github.com/x/y.git", "--container-config=python"},
		{"repo", "add", "https://github.com/x/y.git", "--container-config", "bare"},
		{"claude", "x/y@main"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got, err := stageContainerConfigPaths(args)
			if err != nil {
				t.Fatalf("stageContainerConfigPaths: %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
				t.Fatalf("identifier args were rewritten:\n got: %v\nwant: %v", got, args)
			}
		})
	}
}

func TestStageContainerConfigPaths_NonexistentPathPassesThrough(t *testing.T) {
	// File doesn't exist on Mac — we let the in-VM resolver surface the
	// "not found" error against its own filesystem rather than failing
	// the shim. Argv must reach the VM unchanged.
	args := []string{"repo", "add", "https://github.com/x/y.git", "--container-config", "/tmp/ahjo-nope-does-not-exist.json"}
	got, err := stageContainerConfigPaths(args)
	if err != nil {
		t.Fatalf("stageContainerConfigPaths: %v", err)
	}
	if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("nonexistent path was rewritten:\n got: %v\nwant: %v", got, args)
	}
}

func TestStageContainerConfigPaths_RealFileGetsCopied(t *testing.T) {
	// Stage a real Mac-side file and verify:
	//   1. argv rewrites to a path under SharedDir/tmp/container-config/
	//   2. the staged file has the same content as the source
	//
	// SharedDir resolves to <mac-home>/.ahjo-shared, which exists on the
	// host this test runs on (CI/dev box). We don't try to assert about
	// Lima virtiofs here — that's covered by the manual verification.
	dir := t.TempDir()
	src := filepath.Join(dir, "mycfg.json")
	body := []byte(`{"features": {"ghcr.io/devcontainers/features/go:1": {}}}`)
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	args := []string{"repo", "add", "https://github.com/x/y.git", "--container-config", src}
	got, err := stageContainerConfigPaths(args)
	if err != nil {
		t.Fatalf("stageContainerConfigPaths: %v", err)
	}
	if len(got) != len(args) {
		t.Fatalf("len mismatch: got %v, want %v", got, args)
	}
	stagedPath := got[4]
	if stagedPath == src {
		t.Fatalf("argv was not rewritten; staged path equals source: %q", stagedPath)
	}
	if !strings.Contains(stagedPath, "container-config-") {
		t.Fatalf("staged path %q lacks the container-config- prefix", stagedPath)
	}
	stagedBytes, err := os.ReadFile(stagedPath)
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	if string(stagedBytes) != string(body) {
		t.Fatalf("staged content differs from source")
	}
	t.Cleanup(func() { _ = os.Remove(stagedPath) })
}

func TestStageContainerConfigPaths_HandlesEqualsForm(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "mycfg.json")
	if err := os.WriteFile(src, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	args := []string{"repo", "add", "https://github.com/x/y.git", "--container-config=" + src}
	got, err := stageContainerConfigPaths(args)
	if err != nil {
		t.Fatalf("stageContainerConfigPaths: %v", err)
	}
	if len(got) != len(args) {
		t.Fatalf("len mismatch: got %v, want %v", got, args)
	}
	last := got[len(got)-1]
	if !strings.HasPrefix(last, "--container-config=") {
		t.Fatalf("missing --container-config= prefix: %q", last)
	}
	staged := strings.TrimPrefix(last, "--container-config=")
	if staged == src {
		t.Fatalf("argv was not rewritten; staged path equals source")
	}
	t.Cleanup(func() { _ = os.Remove(staged) })
}
