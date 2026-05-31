package mirror_test

// revert_test.go drives the host-side revert plumbing against a real `git` in
// t.TempDir() (skipping when git is absent), matching gitignore_parity_test.go.
// The headline case is the staged/unstaged split: a mirror revert must restore
// the user's *pre-mirror* uncommitted work exactly — not collapse it to HEAD.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/mirror"
	"github.com/lasselaakkonen/ahjo/internal/paths"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// isolate points HOME at a throwaway dir so the developer's global gitconfig
// and ~/.ahjo state never leak into a test (CaptureEmpty writes under HOME).
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitC(t, dir, "init", "-q")
	gitC(t, dir, "config", "user.email", "test@example.com")
	gitC(t, dir, "config", "user.name", "Test")
	gitC(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

func gitC(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitErr(dir string, args ...string) error {
	return exec.Command("git", append([]string{"-C", dir}, args...)...).Run()
}

func refExists(dir, ref string) bool {
	return exec.Command("git", "-C", dir, "show-ref", "--verify", "--quiet", ref).Run() == nil
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(dir, rel string) bool {
	_, err := os.Stat(filepath.Join(dir, rel))
	return err == nil
}

func TestListSnapshots(t *testing.T) {
	requireGit(t)
	isolate(t)
	dir := newGitRepo(t)
	write(t, dir, "a", "v1")
	gitC(t, dir, "add", "a")
	gitC(t, dir, "commit", "-q", "-m", "init")

	if slugs, err := mirror.ListSnapshots(dir); err != nil || len(slugs) != 0 {
		t.Fatalf("ListSnapshots (none) = %v, %v; want [], nil", slugs, err)
	}

	if err := mirror.CaptureGit(dir, "b"); err != nil {
		t.Fatal(err)
	}
	if err := mirror.CaptureGit(dir, "a"); err != nil {
		t.Fatal(err)
	}

	slugs, err := mirror.ListSnapshots(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(slugs, ","); got != "a,b" {
		t.Errorf("ListSnapshots = %q, want \"a,b\" (sorted)", got)
	}
}

func TestListSnapshots_NonGitTarget(t *testing.T) {
	requireGit(t)
	isolate(t)
	slugs, err := mirror.ListSnapshots(t.TempDir()) // not a git work tree
	if err != nil {
		t.Fatalf("ListSnapshots (non-git) err = %v; want nil", err)
	}
	if len(slugs) != 0 {
		t.Errorf("ListSnapshots (non-git) = %v; want []", slugs)
	}
}

// Regression: a fresh-empty target's snapshot lives as an empty marker keyed by
// slug under ~/.ahjo (not in target's .git). `ahjo mirror revert <target>`
// resolves the slug via ListSnapshots, so the empty-marker slug must be
// discoverable from target alone — otherwise the orphan-recovery path reports
// "no pre-mirror snapshot" and can never wipe an empty mirror back.
func TestListSnapshots_DiscoversEmptyMarker(t *testing.T) {
	requireGit(t)
	isolate(t)
	target := t.TempDir() // empty, not a git repo

	if err := mirror.CaptureEmpty(target, "empty-slug"); err != nil {
		t.Fatal(err)
	}
	slugs, err := mirror.ListSnapshots(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(slugs, ","); got != "empty-slug" {
		t.Errorf("ListSnapshots = %q, want \"empty-slug\"", got)
	}

	// A marker for a different target must not surface for this one.
	if err := mirror.CaptureEmpty(t.TempDir(), "other-slug"); err != nil {
		t.Fatal(err)
	}
	slugs, err = mirror.ListSnapshots(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(slugs, ","); got != "empty-slug" {
		t.Errorf("ListSnapshots after foreign marker = %q, want \"empty-slug\"", got)
	}
}

// Headline: pre-mirror staged-only + unstaged edits both survive a revert.
func TestRevertGit_PreservesStagedUnstagedSplit(t *testing.T) {
	requireGit(t)
	isolate(t)
	dir := newGitRepo(t)
	write(t, dir, "a", "v1")
	gitC(t, dir, "add", "a")
	gitC(t, dir, "commit", "-q", "-m", "init")

	write(t, dir, "a", "v2") // unstaged modification
	write(t, dir, "b", "bee")
	gitC(t, dir, "add", "b") // staged-only

	if err := mirror.CaptureGit(dir, "slug"); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"worktree", "index", "head"} {
		if !refExists(dir, "refs/ahjo/mirror-snapshot/slug/"+kind) {
			t.Fatalf("capture did not write %s ref", kind)
		}
	}

	write(t, dir, "a", "MIRRORED") // mirror clobbers
	write(t, dir, "m.txt", "x")    // mirror adds a file

	if err := mirror.Revert(dir, "slug"); err != nil {
		t.Fatal(err)
	}

	if got := read(t, dir, "a"); got != "v2" {
		t.Errorf("a = %q, want v2 (unstaged edit restored)", got)
	}
	if got := read(t, dir, "b"); got != "bee" {
		t.Errorf("b = %q, want bee", got)
	}
	if exists(dir, "m.txt") {
		t.Error("m.txt should have been removed")
	}
	if got := gitC(t, dir, "show", ":a"); got != "v1" {
		t.Errorf("staged a = %q, want v1 (the v2 edit was never staged)", got)
	}
	if cached := gitC(t, dir, "diff", "--cached", "--name-status"); cached != "A\tb" {
		t.Errorf("staged changes = %q, want \"A\\tb\" (only b staged-added)", cached)
	}
	if unstaged := gitC(t, dir, "diff", "--name-status"); unstaged != "M\ta" {
		t.Errorf("unstaged changes = %q, want \"M\\ta\" (only a modified)", unstaged)
	}
	for _, kind := range []string{"worktree", "index", "head"} {
		if refExists(dir, "refs/ahjo/mirror-snapshot/slug/"+kind) {
			t.Errorf("%s ref should be consumed after a successful revert", kind)
		}
	}
}

func TestRevertGit_GitignoredFilesSurvive(t *testing.T) {
	requireGit(t)
	isolate(t)
	dir := newGitRepo(t)
	write(t, dir, ".gitignore", ".env\nnode_modules/\n")
	write(t, dir, "a", "v1")
	gitC(t, dir, "add", ".gitignore", "a")
	gitC(t, dir, "commit", "-q", "-m", "init")
	write(t, dir, ".env", "SECRET")
	write(t, dir, "node_modules/dep", "lib")

	if err := mirror.CaptureGit(dir, "s"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a", "MIRRORED")
	write(t, dir, "m.txt", "x")
	if err := mirror.Revert(dir, "s"); err != nil {
		t.Fatal(err)
	}

	if got := read(t, dir, ".env"); got != "SECRET" {
		t.Errorf(".env = %q, want SECRET (gitignored, never captured, never cleaned)", got)
	}
	if got := read(t, dir, "node_modules/dep"); got != "lib" {
		t.Errorf("node_modules/dep = %q, want lib", got)
	}
	if got := read(t, dir, "a"); got != "v1" {
		t.Errorf("a = %q, want v1", got)
	}
	if exists(dir, "m.txt") {
		t.Error("m.txt should be removed")
	}
}

func TestRevertGit_GitignoredMirrorAddIsResidue(t *testing.T) {
	requireGit(t)
	isolate(t)
	dir := newGitRepo(t)
	write(t, dir, ".gitignore", "node_modules/\n")
	write(t, dir, "a", "v1")
	gitC(t, dir, "add", ".")
	gitC(t, dir, "commit", "-q", "-m", "init")

	if err := mirror.CaptureGit(dir, "s"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "node_modules/x", "dep") // gitignored mirror-add
	write(t, dir, "m.txt", "x")            // non-ignored mirror-add
	if err := mirror.Revert(dir, "s"); err != nil {
		t.Fatal(err)
	}

	// Documents the no-`-x` tradeoff: clean cannot reach gitignored files, so a
	// mirror-added gitignored file is left behind. Removing it would require
	// `-x`, which would also destroy the user's own .env/node_modules.
	if !exists(dir, "node_modules/x") {
		t.Error("gitignored mirror-added node_modules/x should survive revert (no -x)")
	}
	if exists(dir, "m.txt") {
		t.Error("non-ignored mirror-added m.txt should be removed")
	}
}

func TestRevertGit_ReproducesTrackedDeletion(t *testing.T) {
	requireGit(t)
	isolate(t)
	dir := newGitRepo(t)
	write(t, dir, "a", "v1")
	write(t, dir, "b", "bee")
	gitC(t, dir, "add", "a", "b")
	gitC(t, dir, "commit", "-q", "-m", "init")
	if err := os.Remove(filepath.Join(dir, "b")); err != nil { // pre-mirror unstaged deletion
		t.Fatal(err)
	}

	if err := mirror.CaptureGit(dir, "s"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "b", "RESURRECTED") // mirror re-creates it
	if err := mirror.Revert(dir, "s"); err != nil {
		t.Fatal(err)
	}

	if exists(dir, "b") {
		t.Error("b should be deleted again after revert (pre-mirror state)")
	}
	if unstaged := gitC(t, dir, "diff", "--name-status"); unstaged != "D\tb" {
		t.Errorf("unstaged changes = %q, want \"D\\tb\" (unstaged deletion reproduced)", unstaged)
	}
}

func TestRevertGit_UnbornBranch(t *testing.T) {
	requireGit(t)
	isolate(t)
	dir := newGitRepo(t)
	write(t, dir, "b", "bee")
	gitC(t, dir, "add", "b")  // staged, never committed → unborn HEAD
	write(t, dir, "c", "cee") // untracked
	if gitErr(dir, "rev-parse", "--verify", "HEAD") == nil {
		t.Fatal("expected an unborn HEAD precondition")
	}

	if err := mirror.CaptureGit(dir, "s"); err != nil {
		t.Fatal(err)
	}
	if refExists(dir, "refs/ahjo/mirror-snapshot/s/head") {
		t.Error("head ref must not be written for an unborn branch")
	}
	write(t, dir, "b", "MIRRORED")
	write(t, dir, "m.txt", "x")
	if err := mirror.Revert(dir, "s"); err != nil {
		t.Fatal(err)
	}

	if gitErr(dir, "rev-parse", "--verify", "HEAD") == nil {
		t.Error("HEAD should still be unborn after revert")
	}
	if got := read(t, dir, "b"); got != "bee" {
		t.Errorf("b = %q, want bee", got)
	}
	if got := read(t, dir, "c"); got != "cee" {
		t.Errorf("c = %q, want cee", got)
	}
	if exists(dir, "m.txt") {
		t.Error("m.txt should be removed")
	}
	status := gitC(t, dir, "status", "--porcelain")
	if !strings.Contains(status, "A  b") {
		t.Errorf("status %q: want 'A  b'", status)
	}
	if !strings.Contains(status, "?? c") {
		t.Errorf("status %q: want '?? c'", status)
	}
}

func TestRevertGit_ScopeGuardRefusesSubdir(t *testing.T) {
	requireGit(t)
	isolate(t)
	dir := newGitRepo(t)
	write(t, dir, "a", "v1")
	gitC(t, dir, "add", "a")
	gitC(t, dir, "commit", "-q", "-m", "init")
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "sub/keep", "data")

	if err := mirror.CaptureGit(dir, "s"); err != nil {
		t.Fatal(err)
	}
	// Reverting a subdir of the repo must refuse: --show-toplevel != target,
	// otherwise clean/read-tree would reach the whole parent repo.
	err := mirror.Revert(sub, "s")
	if err == nil {
		t.Fatal("expected scope-guard refusal when reverting a subdir")
	}
	if !strings.Contains(err.Error(), "toplevel") {
		t.Errorf("error %q should mention the toplevel mismatch", err)
	}
	// An aborted revert must leave the snapshot intact for a retry...
	if !refExists(dir, "refs/ahjo/mirror-snapshot/s/worktree") {
		t.Error("worktree ref should survive an aborted revert")
	}
	// ...and must not have touched any files.
	if got := read(t, dir, "sub/keep"); got != "data" {
		t.Errorf("sub/keep = %q, want data (untouched)", got)
	}
}

func TestRevertGit_HeadMovedKeepsCommits(t *testing.T) {
	requireGit(t)
	isolate(t)
	dir := newGitRepo(t)
	write(t, dir, "a", "v1")
	gitC(t, dir, "add", "a")
	gitC(t, dir, "commit", "-q", "-m", "c1")

	if err := mirror.CaptureGit(dir, "s"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a", "v2")
	gitC(t, dir, "commit", "-q", "-am", "c2") // user commits during the session
	c2 := gitC(t, dir, "rev-parse", "HEAD")

	write(t, dir, "a", "MIRRORED")
	if err := mirror.Revert(dir, "s"); err != nil {
		t.Fatal(err)
	}
	if got := gitC(t, dir, "rev-parse", "HEAD"); got != c2 {
		t.Errorf("HEAD = %s, want %s (revert must never move HEAD / drop commits)", got, c2)
	}
}

func TestCaptureGit_Idempotent(t *testing.T) {
	requireGit(t)
	isolate(t)
	dir := newGitRepo(t)
	write(t, dir, "a", "v1")
	gitC(t, dir, "add", "a")
	gitC(t, dir, "commit", "-q", "-m", "init")

	if err := mirror.CaptureGit(dir, "s"); err != nil {
		t.Fatal(err)
	}
	first := gitC(t, dir, "rev-parse", "refs/ahjo/mirror-snapshot/s/worktree")
	write(t, dir, "a", "CHANGED")
	if err := mirror.CaptureGit(dir, "s"); err != nil {
		t.Fatal(err)
	}
	second := gitC(t, dir, "rev-parse", "refs/ahjo/mirror-snapshot/s/worktree")
	if first != second {
		t.Errorf("re-capture should reuse the existing snapshot: %s != %s", first, second)
	}
}

func TestDetectMode(t *testing.T) {
	requireGit(t)
	isolate(t)

	if m, err := mirror.DetectMode(newGitRepo(t)); err != nil || m != mirror.ModeGit {
		t.Errorf("git repo: mode=%v err=%v, want ModeGit", m, err)
	}
	absent := filepath.Join(t.TempDir(), "nope")
	if m, err := mirror.DetectMode(absent); err != nil || m != mirror.ModeFreshEmpty {
		t.Errorf("absent dir: mode=%v err=%v, want ModeFreshEmpty", m, err)
	}
	if m, err := mirror.DetectMode(t.TempDir()); err != nil || m != mirror.ModeFreshEmpty {
		t.Errorf("empty dir: mode=%v err=%v, want ModeFreshEmpty", m, err)
	}
	nonGit := t.TempDir()
	write(t, nonGit, "f", "x")
	if m, err := mirror.DetectMode(nonGit); err != nil || m != mirror.ModeFreshNonEmpty {
		t.Errorf("populated non-git dir: mode=%v err=%v, want ModeFreshNonEmpty", m, err)
	}
}

func TestTargetDirty(t *testing.T) {
	requireGit(t)
	isolate(t)
	dir := newGitRepo(t)
	write(t, dir, "a", "v1")
	gitC(t, dir, "add", "a")
	gitC(t, dir, "commit", "-q", "-m", "init")

	if sum, dirty, err := mirror.TargetDirty(dir); err != nil || dirty || sum != "" {
		t.Errorf("clean tree: sum=%q dirty=%v err=%v; want \"\",false,nil", sum, dirty, err)
	}

	write(t, dir, "a", "v2") // unstaged
	write(t, dir, "b", "bee")
	gitC(t, dir, "add", "b") // staged
	write(t, dir, "u", "untracked")
	sum, dirty, err := mirror.TargetDirty(dir)
	if err != nil || !dirty {
		t.Fatalf("dirty tree: dirty=%v err=%v; want true,nil", dirty, err)
	}
	for _, want := range []string{"staged", "unstaged", "untracked"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary %q should mention %q", sum, want)
		}
	}
}

func TestRevertEmpty_WipesAndRemovesMarker(t *testing.T) {
	requireGit(t)
	isolate(t)
	target := t.TempDir() // empty, not a git repo

	if m, err := mirror.DetectMode(target); err != nil || m != mirror.ModeFreshEmpty {
		t.Fatalf("DetectMode = %v err=%v, want ModeFreshEmpty", m, err)
	}
	if err := mirror.CaptureEmpty(target, "s"); err != nil {
		t.Fatal(err)
	}
	if !mirror.RevertPossible(target, "s") {
		t.Fatal("RevertPossible should be true after CaptureEmpty")
	}

	// mirror populates the target
	write(t, target, "f1", "x")
	write(t, target, "d/sub/f2", "y")

	if err := mirror.Revert(target, "s"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("target not empty after revert: %v", entries)
	}
	if _, err := os.Stat(paths.MirrorSnapshotDir("s")); !os.IsNotExist(err) {
		t.Errorf("snapshot marker dir should be removed; stat err = %v", err)
	}
}
