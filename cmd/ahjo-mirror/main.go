// Package main is the in-container `ahjo-mirror` daemon. It watches /repo
// via inotify and pushes per-event copies into /mirror (a writable bind-mount
// of a Mac-side path). One git-faithful gitignore matcher governs every
// decision; bootstrap and live event handling share the same per-file copy
// routine, so the Mac side can never observe a half-written file.
//
// See designdocs/in-container-mirror.md for the full design.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/lasselaakkonen/ahjo/internal/mirror"
)

// version is overridden via -ldflags '-X main.version=…' at build time
// (see Makefile / cmd/ahjo-mirror/generate.go). The CLI's `mirror on` flow
// reconciles the in-container binary by comparing this stamp.
var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("ahjo-mirror: ")

	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *versionFlag {
		fmt.Println(version)
		return
	}

	args := flag.Args()
	if len(args) != 2 {
		log.Fatalf("usage: ahjo-mirror <src> <dst>  (got %d args)", len(args))
	}
	src := filepath.Clean(args[0])
	dst := filepath.Clean(args[1])
	noSkiplist := os.Getenv("AHJO_MIRROR_NO_SKIPLIST") == "1"

	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		log.Fatalf("src %s is not a directory: %v", src, err)
	}
	if fi, err := os.Stat(dst); err != nil || !fi.IsDir() {
		log.Fatalf("dst %s is not a directory: %v", dst, err)
	}

	log.Printf("starting (version %s) src=%s dst=%s no-skiplist=%t", version, src, dst, noSkiplist)

	matcher, err := mirror.LoadIgnoreMatcher(src, noSkiplist)
	if err != nil {
		log.Fatalf("load gitignore matcher: %v", err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("fsnotify new: %v", err)
	}
	defer w.Close()

	// Order matters: install watches BEFORE bootstrap. Any change during the
	// window between watch-install and bootstrap-end queues an event we'll
	// drain after — and the bootstrap+drain converge atomically because
	// CopyFile uses tempfile+rename.
	count, err := installWatches(w, src, matcher, noSkiplist)
	if err != nil {
		log.Fatalf("install watches: %v", err)
	}
	log.Printf("watches installed: %d", count)

	bootstrapSubtree(src, dst, src, matcher, noSkiplist, true)
	log.Printf("bootstrap complete")

	ctx, cancel := mirror.InstallSignalHandler()
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			log.Printf("shutdown")
			return
		case ev, ok := <-w.Events:
			if !ok {
				log.Fatalf("watcher events channel closed")
			}
			handleEvent(ev, w, src, dst, matcher, noSkiplist)
		case err, ok := <-w.Errors:
			if !ok {
				log.Fatalf("watcher errors channel closed")
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				log.Printf("IN_Q_OVERFLOW; re-installing watches and re-bootstrapping")
				if _, werr := installWatches(w, src, matcher, noSkiplist); werr != nil {
					log.Printf("re-install watches: %v", werr)
				}
				bootstrapSubtree(src, dst, src, matcher, noSkiplist, true)
				continue
			}
			log.Printf("watch error: %v", err)
		}
	}
}

// installWatches walks `root` and registers an inotify watch on every
// non-skipped, non-ignored directory. Idempotent: fsnotify.Add returns nil
// for already-watched paths.
func installWatches(w *fsnotify.Watcher, root string, m gitignore.Matcher, noSkiplist bool) (int, error) {
	count := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if p != root && !noSkiplist && mirror.SkipDirNames[d.Name()] {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(root, p)
		if mirror.IsIgnored(m, rel, true) {
			return filepath.SkipDir
		}
		if err := w.Add(p); err != nil {
			log.Printf("watch %s: %v", p, err)
			return nil
		}
		count++
		return nil
	})
	return count, err
}

// bootstrapSubtree walks the subtree rooted at `walkRoot` (which must be
// under `src`) and copies every kept regular file / symlink to the
// corresponding path under `dst`. Directories without a corresponding `dst`
// counterpart are mkdir-ed.
//
// fastSkip=true: regular files whose dst already matches size+mtime are
// skipped, and symlinks whose dst already points to the same target are
// skipped. Used by initial bootstrap and IN_Q_OVERFLOW recovery to keep
// repeated walks cheap.
//
// fastSkip=false: used by the live new-dir-Create handler — files written
// after our watch install are unconditionally re-copied to catch the race
// where a write landed before fsnotify could attach a watch to a fresh dir.
func bootstrapSubtree(src, dst, walkRoot string, m gitignore.Matcher, noSkiplist, fastSkip bool) {
	err := filepath.WalkDir(walkRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != walkRoot && !noSkiplist && mirror.SkipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			if mirror.IsIgnored(m, rel, true) {
				return filepath.SkipDir
			}
			if rel != "." {
				dstDir := filepath.Join(dst, rel)
				if err := os.MkdirAll(dstDir, 0o755); err != nil {
					log.Printf("mkdir %s: %v", dstDir, err)
				}
			}
			return nil
		}
		if shouldSkipPath(rel, false, m, noSkiplist) {
			return nil
		}
		dstPath := filepath.Join(dst, rel)
		if err := mirror.CopyFile(p, dstPath, fastSkip); err != nil {
			if !errors.Is(err, mirror.ErrUnsupportedFileType) {
				log.Printf("copy %s: %v", rel, err)
			}
			return nil
		}
		return nil
	})
	if err != nil {
		log.Printf("walk %s: %v", walkRoot, err)
	}
}

// handleEvent dispatches one inotify event. Decisions are deliberately the
// same as bootstrapSubtree's so live and bootstrap can never disagree.
func handleEvent(ev fsnotify.Event, w *fsnotify.Watcher, src, dst string, m gitignore.Matcher, noSkiplist bool) {
	rel, err := filepath.Rel(src, ev.Name)
	if err != nil {
		return
	}

	// .gitignore changes: exit non-zero, systemd restarts (RestartSec=2),
	// bootstrap re-runs with fresh rules. Crude but very simple — no
	// in-memory invalidation logic to test or debug. Triggers on Write
	// (vim/JetBrains) AND Create/Rename (VS Code et al. save via
	// tempfile-then-rename).
	if filepath.Base(rel) == ".gitignore" &&
		(ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) || ev.Has(fsnotify.Rename)) {
		log.Printf(".gitignore %s changed (%s); exiting for systemd restart", rel, ev.Op)
		os.Exit(1)
	}

	// Don't propagate skiplisted/ignored events. Note we deliberately don't
	// check ignore here for directories — installWatches has already
	// excluded them, so we won't see events for ignored subtrees.
	if shouldSkipPath(rel, false, m, noSkiplist) {
		return
	}

	// Remove events: nothing to do (no delete tracking, by design).
	if ev.Has(fsnotify.Remove) {
		return
	}

	info, err := os.Lstat(ev.Name)
	if err != nil {
		// Raced with deletion; or stat-after-rename. Benign.
		return
	}

	if info.IsDir() {
		if !ev.Has(fsnotify.Create) {
			return
		}
		// New directory: re-install watches inside it and bootstrap any
		// files already present (closes the new-dir-CREATE race where
		// files are written before fsnotify can Add the new dir).
		if _, werr := installWatches(w, ev.Name, m, noSkiplist); werr != nil {
			log.Printf("watch new dir %s: %v", ev.Name, werr)
		}
		bootstrapSubtree(src, dst, ev.Name, m, noSkiplist, false)
		return
	}

	if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		// Socket / device / fifo: skip. Same decision as bootstrap.
		return
	}

	// Ensure dst parent dir exists (handles the rare case where the parent
	// was created and the file written before our event-driven mkdir from
	// the parent's Create event landed).
	dstPath := filepath.Join(dst, rel)
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		log.Printf("mkdir %s: %v", filepath.Dir(dstPath), err)
		return
	}

	if err := mirror.CopyFile(ev.Name, dstPath, false); err != nil {
		if errors.Is(err, mirror.ErrUnsupportedFileType) {
			return
		}
		log.Printf("copy %s: %v", rel, err)
		return
	}
	log.Printf("cp %s", rel)
}

// shouldSkipPath returns true for paths the mirror must not propagate:
// any path component matching SkipDirNames (unless noSkiplist) or a path
// matching gitignore rules.
func shouldSkipPath(rel string, isDir bool, m gitignore.Matcher, noSkiplist bool) bool {
	if rel == "" || rel == "." {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if !noSkiplist {
		for _, p := range parts {
			if mirror.SkipDirNames[p] {
				return true
			}
		}
	}
	return mirror.IsIgnored(m, rel, isDir)
}
