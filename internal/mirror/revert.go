package mirror

// revert.go is the control-plane half of the mirror revert feature (the
// daemon half lives in cmd/ahjo-mirror). It snapshots the pristine state of
// the host target at `mirror on` and restores it at `mirror off`, so tearing
// down a mirror does not leave the host work tree clobbered.
//
// The CLI runs inside the Lima VM (uid 1000) and reaches the Mac target over
// virtiofs, so every git invocation passes `-c safe.directory=<target>` to
// avoid a "detected dubious ownership" failure — the same workaround the
// in-container clean-check uses for /repo.
//
// Mechanism by target type (see DetectMode):
//   - Git work tree → snapshot to private refs in the target's own .git. Cheap
//     (git dedups blobs), and it preserves the staged/unstaged split *and* the
//     user's pre-mirror uncommitted work, which the mirror clobbers on start.
//   - Empty/absent  → a ".empty" marker under paths.MirrorSnapshotDir; revert
//     wipes the target back to empty.
//   - Non-empty non-git → no snapshot (handled by a prompt in the CLI).

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// Mode classifies a mirror target, which selects the snapshot/restore mechanism.
type Mode int

const (
	ModeGit Mode = iota
	ModeFreshEmpty
	ModeFreshNonEmpty
)

const snapshotRefPrefix = "refs/ahjo/mirror-snapshot/"

func snapshotRef(slug, kind string) string {
	return snapshotRefPrefix + slug + "/" + kind
}

// gitAvailable returns a clear error when git is not on PATH inside the VM,
// rather than letting callers surface a cryptic exec failure.
func gitAvailable() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git not found on PATH (required for mirror snapshot/revert): %w", err)
	}
	return nil
}

// runGit runs git against target with the safe.directory override prepended,
// returning trimmed stdout. On failure it surfaces git's stderr.
func runGit(target string, env []string, args ...string) (string, error) {
	full := append([]string{"-C", target, "-c", "safe.directory=" + target}, args...)
	cmd := exec.Command("git", full...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func refExists(target, ref string) bool {
	_, err := runGit(target, nil, "show-ref", "--verify", "--quiet", ref)
	return err == nil
}

// IsGitWorkTree reports whether target is inside a git work tree, returning the
// resolved toplevel.
func IsGitWorkTree(target string) (string, bool) {
	root, err := runGit(target, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false
	}
	return root, true
}

// DetectMode classifies target. A non-existent or empty dir that is not a git
// work tree is FreshEmpty; a populated non-git dir is FreshNonEmpty.
func DetectMode(target string) (Mode, error) {
	if err := gitAvailable(); err != nil {
		return 0, err
	}
	if _, ok := IsGitWorkTree(target); ok {
		return ModeGit, nil
	}
	empty, err := dirEmptyOrAbsent(target)
	if err != nil {
		return 0, err
	}
	if empty {
		return ModeFreshEmpty, nil
	}
	return ModeFreshNonEmpty, nil
}

func dirEmptyOrAbsent(target string) (bool, error) {
	entries, err := os.ReadDir(target)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// TargetDirty reports whether the git work tree at target has uncommitted
// changes the mirror would clobber, with a short summary. Unpushed commits are
// ignored — .git is never mirrored.
func TargetDirty(target string) (summary string, dirty bool, err error) {
	out, err := runGit(target, nil, "status", "--porcelain=v2", "--branch")
	if err != nil {
		return "", false, err
	}
	counts := git.ParseStatusV2(out)
	return counts.WorkTreeSummary(), counts.WorkTreeDirty(), nil
}

// RevertPossible reports whether a snapshot exists for slug at target: a git
// worktree ref in the target's .git, or a fresh-empty ".empty" marker. It keys
// off snapshot artifacts (not live re-detection), so it stays correct after the
// tree has been mirrored over.
func RevertPossible(target, slug string) bool {
	if refExists(target, snapshotRef(slug, "worktree")) {
		return true
	}
	return hasEmptyMarker(slug)
}

// ListSnapshots returns the slugs that have a pre-mirror snapshot for target,
// sorted and de-duplicated. Used by the orphan-recovery path (`ahjo mirror
// revert <target>`) to discover which snapshot to restore when no
// container/branch remains. It covers both snapshot mechanisms: git-ref
// snapshots under target's own .git, and empty-marker snapshots recorded for
// target under ~/.ahjo. A target with neither yields an empty slice and no
// error.
func ListSnapshots(target string) ([]string, error) {
	if err := gitAvailable(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var slugs []string
	add := func(slug string) {
		if slug != "" && !seen[slug] {
			seen[slug] = true
			slugs = append(slugs, slug)
		}
	}

	if _, ok := IsGitWorkTree(target); ok {
		out, err := runGit(target, nil, "for-each-ref", "--format=%(refname)", snapshotRefPrefix)
		if err != nil {
			return nil, err
		}
		for _, ref := range strings.Split(out, "\n") {
			ref = strings.TrimSpace(ref)
			rest, ok := strings.CutPrefix(ref, snapshotRefPrefix)
			if !ok {
				continue
			}
			// rest is "<slug>/<kind>"; key off the worktree ref (the
			// capture-complete flag) so a half-written snapshot doesn't surface
			// as recoverable.
			slug, kind, ok := strings.Cut(rest, "/")
			if !ok || kind != "worktree" || slug == "" {
				continue
			}
			add(slug)
		}
	}

	emptySlugs, err := listEmptyMarkerSlugs(target)
	if err != nil {
		return nil, err
	}
	for _, slug := range emptySlugs {
		add(slug)
	}

	sort.Strings(slugs)
	return slugs, nil
}

// CaptureGit snapshots the target work tree and index as separate refs in the
// target's own .git, so a later Revert can reconstruct the exact pre-mirror
// staged/unstaged split. Idempotent: a pre-existing worktree ref is reused.
func CaptureGit(target, slug string) error {
	wtRef := snapshotRef(slug, "worktree")
	if refExists(target, wtRef) {
		fmt.Println("mirror: reusing existing pre-mirror snapshot")
		return nil
	}

	// HEAD; empty ⇒ unborn branch (fresh init, 0 commits) — record no head ref
	// and make both snapshot commits parentless.
	head, _ := runGit(target, nil, "rev-parse", "--verify", "HEAD")
	if head != "" {
		if _, err := runGit(target, nil, "update-ref", snapshotRef(slug, "head"), head); err != nil {
			return err
		}
	}

	// Index tree → commit → ref. write-tree captures the live staged state.
	idxTree, err := runGit(target, nil, "write-tree")
	if err != nil {
		return err
	}
	idxCommit, err := commitTree(target, idxTree, head, "ahjo idx "+slug)
	if err != nil {
		return err
	}
	if _, err := runGit(target, nil, "update-ref", snapshotRef(slug, "index"), idxCommit); err != nil {
		return err
	}

	// Worktree tree via a throwaway index *outside* the target, so the live
	// index and stash stack stay untouched. `add -A` records content blobs
	// (immutable as the mirror overwrites files), respects .gitignore (so
	// .env/node_modules are never captured), and records tracked deletions.
	realIdx, err := runGit(target, nil, "rev-parse", "--git-path", "index")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(realIdx) {
		realIdx = filepath.Join(target, realIdx)
	}
	tmpIdx, err := os.CreateTemp("", "ahjo-mirror-index-*")
	if err != nil {
		return err
	}
	tmpIdxPath := tmpIdx.Name()
	_ = tmpIdx.Close()
	defer os.Remove(tmpIdxPath)
	if err := copyFileContents(realIdx, tmpIdxPath); err != nil {
		return err
	}
	env := []string{"GIT_INDEX_FILE=" + tmpIdxPath}
	if _, err := runGit(target, env, "add", "-A"); err != nil {
		return err
	}
	wtTree, err := runGit(target, env, "write-tree")
	if err != nil {
		return err
	}
	wtCommit, err := commitTree(target, wtTree, head, "ahjo wt "+slug)
	if err != nil {
		return err
	}
	// Write the worktree ref LAST: its presence is the "capture complete" flag,
	// so a crash mid-capture never leaves a half-snapshot that looks usable.
	if _, err := runGit(target, nil, "update-ref", wtRef, wtCommit); err != nil {
		return err
	}
	return nil
}

func commitTree(target, tree, parent, msg string) (string, error) {
	args := []string{"commit-tree", tree, "-m", msg}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return runGit(target, nil, args...)
}

// CaptureEmpty records that target was empty (or absent) at mirror-on time, so
// Revert knows to wipe it back to empty. The marker stores target's absolute
// path so the orphan-recovery path (`ahjo mirror revert <target>`) can map a
// target back to its slug — empty-marker snapshots live under ~/.ahjo keyed by
// slug, not in the target's .git, so target alone can't otherwise find them.
func CaptureEmpty(target, slug string) error {
	dir := paths.MirrorSnapshotDir(slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(emptyMarkerPath(slug), []byte(target+"\n"), 0o600)
}

func emptyMarkerPath(slug string) string {
	return filepath.Join(paths.MirrorSnapshotDir(slug), ".empty")
}

func hasEmptyMarker(slug string) bool {
	_, err := os.Stat(emptyMarkerPath(slug))
	return err == nil
}

// emptyMarkerTarget returns the target path recorded in slug's empty marker.
// Empty string when the marker is absent or unreadable, or predates the
// target-recording marker format (written as an empty file) — such a snapshot
// is still revertable with an explicit slug, just not auto-discoverable.
func emptyMarkerTarget(slug string) string {
	b, err := os.ReadFile(emptyMarkerPath(slug))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// listEmptyMarkerSlugs returns the slugs whose empty marker records target,
// the empty-mode complement to ListSnapshots' git-ref scan. A missing
// snapshots dir is not an error (nothing has ever been mirrored).
func listEmptyMarkerSlugs(target string) ([]string, error) {
	entries, err := os.ReadDir(paths.MirrorSnapshotsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var slugs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		slug := e.Name()
		if recorded := emptyMarkerTarget(slug); recorded != "" && samePath(recorded, target) {
			slugs = append(slugs, slug)
		}
	}
	return slugs, nil
}

// Revert restores target to its captured pre-mirror state and consumes the
// snapshot. Git-mode refs win over the empty marker; with neither present it is
// a no-op. It must run only after the daemon is stopped and the mirror device
// removed (the CLI enforces that ordering).
func Revert(target, slug string) error {
	if refExists(target, snapshotRef(slug, "worktree")) {
		return revertGit(target, slug)
	}
	if hasEmptyMarker(slug) {
		return revertEmpty(target, slug)
	}
	return nil
}

func revertGit(target, slug string) error {
	// 1. Scope guard: refuse unless target is its own toplevel. If target were
	//    a subdir of a larger repo, read-tree -u / clean would reach the whole
	//    parent repo — a catastrophe well outside the mirror's scope.
	top, err := runGit(target, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("revert scope guard: %w", err)
	}
	if !samePath(top, target) {
		return fmt.Errorf("refusing to revert %q: it is not the git toplevel (%q); a restore here could reach outside the mirror target", target, top)
	}

	wtTree := snapshotRef(slug, "worktree") + "^{tree}"
	idxTree := snapshotRef(slug, "index") + "^{tree}"

	// 2. Restore tracked + previously-untracked files (index and work tree both
	//    become the worktree snapshot).
	if _, err := runGit(target, nil, "read-tree", "--reset", "-u", wtTree); err != nil {
		return err
	}
	// 3. Remove mirror-added files. NO -x, so the user's pre-existing gitignored
	//    files (.env, node_modules) survive untouched.
	if _, err := runGit(target, nil, "clean", "-f", "-d"); err != nil {
		return err
	}
	// 4. Restore the index snapshot (no -u): the work-tree-vs-index difference
	//    reappears exactly as the pre-mirror unstaged edits.
	if _, err := runGit(target, nil, "read-tree", idxTree); err != nil {
		return err
	}

	// Never move HEAD. If the user committed during the session, warn only —
	// reset --hard would discard those commits, and the restore above already
	// re-applied the pre-mirror work tree on top of wherever HEAD now points.
	warnIfHeadMoved(target, slug)

	// 5. Consume the refs — only now that every step above has succeeded. On any
	//    earlier failure we returned, leaving the refs intact for a retry.
	for _, kind := range []string{"worktree", "index", "head"} {
		ref := snapshotRef(slug, kind)
		if refExists(target, ref) {
			if _, err := runGit(target, nil, "update-ref", "-d", ref); err != nil {
				return err
			}
		}
	}
	return nil
}

func warnIfHeadMoved(target, slug string) {
	headRef := snapshotRef(slug, "head")
	if !refExists(target, headRef) {
		return // unborn at capture; nothing to compare
	}
	snap, err := runGit(target, nil, "rev-parse", "--verify", headRef)
	if err != nil {
		return
	}
	cur, err := runGit(target, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return
	}
	if snap != cur {
		fmt.Printf("mirror: note: HEAD moved since the snapshot (was %s, now %s); your commits are kept — only the work tree and index were restored.\n",
			shortSHA(snap), shortSHA(cur))
	}
}

func revertEmpty(target, slug string) error {
	entries, err := os.ReadDir(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(target, e.Name())); err != nil {
			return err
		}
	}
	return os.RemoveAll(paths.MirrorSnapshotDir(slug))
}

// samePath compares two filesystem paths, tolerating symlink differences
// (e.g. /tmp → /private/tmp) that would otherwise spuriously trip the scope
// guard.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
