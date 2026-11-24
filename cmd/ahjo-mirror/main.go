// Package main is the in-container `ahjo-mirror` watcher. It watches /repo
// via inotify and emits a frame stream (internal/mirror/proto.go) on stdout;
// the host-side `ahjo _mirror-daemon` reads that stream through `incus exec`
// and is the only thing that writes the mirror target. The watcher has no
// access to the host directory at all.
//
// One git-faithful gitignore matcher governs every decision; bootstrap and
// live event handling share the same per-file emit routine. stdout carries
// nothing but frames — all logging goes to stderr.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/fsnotify/fsnotify"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/lasselaakkonen/ahjo/internal/mirror"
)

// version is overridden via -ldflags '-X main.version=…' at build time
// (see Makefile / cmd/ahjo-mirror/generate.go). The host reconciles the
// in-container binary by comparing this stamp.
var version = "dev"

const heartbeatInterval = 5 * time.Second

func main() {
	log.SetFlags(0)
	log.SetPrefix("")
	log.SetOutput(os.Stderr)

	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *versionFlag {
		fmt.Println(version)
		return
	}

	args := flag.Args()
	if len(args) < 1 || args[0] != "watch" {
		log.Fatalf("usage: ahjo-mirror watch [--no-skiplist] <src>")
	}
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	noSkiplist := fs.Bool("no-skiplist", false, "also emit skiplisted directories (node_modules etc.); .git is never emitted")
	_ = fs.Parse(args[1:])
	if fs.NArg() != 1 {
		log.Fatalf("usage: ahjo-mirror watch [--no-skiplist] <src>")
	}
	src := filepath.Clean(fs.Arg(0))
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		log.Fatalf("src %s is not a directory: %v", src, err)
	}

	e := &emitter{src: src, noSkiplist: *noSkiplist, fw: mirror.NewFrameWriter(os.Stdout)}

	// Hello first, then the heartbeat from its own goroutine: loading the
	// matcher, installing watches and the bootstrap walk all run inline and
	// can take longer than the host's idle watchdog on a big tree. A failed
	// write means the host side is gone — exit so no orphan lingers.
	e.must(e.fw.WriteJSON(mirror.FrameHello, mirror.Hello{Proto: mirror.ProtoVersion, Version: version, NoSkiplist: *noSkiplist}))
	go func() {
		for range time.Tick(heartbeatInterval) {
			e.must(e.fw.Write(mirror.FramePing, nil))
		}
	}()

	log.Printf("starting (version %s) src=%s no-skiplist=%t", version, src, *noSkiplist)

	matcher, err := mirror.LoadIgnoreMatcher(src, *noSkiplist)
	if err != nil {
		log.Fatalf("load gitignore matcher: %v", err)
	}
	e.m = matcher

	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("fsnotify new: %v", err)
	}
	defer w.Close()

	// Order matters: install watches BEFORE bootstrap. Any change during the
	// window between watch-install and bootstrap-end queues an event we'll
	// drain after.
	count, err := e.installWatches(w, src)
	if err != nil {
		log.Fatalf("install watches: %v", err)
	}
	log.Printf("watches installed: %d", count)

	e.must(e.bootstrapSubtree(src, true))
	e.must(e.fw.Write(mirror.FrameBootstrapDone, nil))
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
			e.must(e.handleEvent(ev, w))
		case err, ok := <-w.Errors:
			if !ok {
				log.Fatalf("watcher errors channel closed")
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				log.Printf("IN_Q_OVERFLOW; re-installing watches and re-bootstrapping")
				if _, werr := e.installWatches(w, src); werr != nil {
					log.Printf("re-install watches: %v", werr)
				}
				e.must(e.bootstrapSubtree(src, true))
				e.must(e.fw.Write(mirror.FrameBootstrapDone, nil))
				continue
			}
			log.Printf("watch error: %v", err)
		}
	}
}

// emitter turns source-tree state into frames.
type emitter struct {
	src        string
	noSkiplist bool
	m          gitignore.Matcher
	fw         *mirror.FrameWriter
}

// must exits quietly when the frame stream can no longer be written (the
// host daemon went away); any other state is unrecoverable for a one-way
// stream anyway.
func (e *emitter) must(err error) {
	if err != nil {
		log.Printf("frame stream closed (%v); exiting", err)
		os.Exit(0)
	}
}

// skipDirName reports whether a directory named name is never descended.
// `.git` is excluded unconditionally — --no-skiplist does not bring it back.
func (e *emitter) skipDirName(name string) bool {
	return mirror.IsDotGit(name) || (!e.noSkiplist && mirror.SkipDirNames[name])
}

// installWatches walks `root` and registers an inotify watch on every
// non-skipped, non-ignored directory. Idempotent: fsnotify.Add returns nil
// for already-watched paths.
func (e *emitter) installWatches(w *fsnotify.Watcher, root string) (int, error) {
	count := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if p != e.src && e.skipDirName(d.Name()) {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(e.src, p)
		if mirror.IsIgnored(e.m, rel, true) {
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

// bootstrapSubtree walks the subtree rooted at walkRoot (under e.src) and
// emits every kept directory, regular file and symlink. bootstrap=true marks
// the frames so the host may skip files it already holds unchanged; the live
// new-dir handler passes false so files written before the watch attached
// are always re-sent. The only error returned is a frame-write failure.
func (e *emitter) bootstrapSubtree(walkRoot string, bootstrap bool) error {
	var writeErr error
	err := filepath.WalkDir(walkRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(e.src, p)
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != e.src && e.skipDirName(d.Name()) {
				return filepath.SkipDir
			}
			if mirror.IsIgnored(e.m, rel, true) {
				return filepath.SkipDir
			}
			if rel != "." {
				if !utf8.ValidString(rel) {
					log.Printf("skip %q: name is not valid UTF-8", rel)
					return filepath.SkipDir
				}
				if writeErr = e.fw.WriteJSON(mirror.FrameMkdir, mirror.MkdirMsg{Path: filepath.ToSlash(rel)}); writeErr != nil {
					return filepath.SkipAll
				}
			}
			return nil
		}
		if e.shouldSkipPath(rel, false) {
			return nil
		}
		if writeErr = e.emitPath(p, rel, bootstrap); writeErr != nil {
			return filepath.SkipAll
		}
		return nil
	})
	if writeErr != nil {
		return writeErr
	}
	if err != nil {
		log.Printf("walk %s: %v", walkRoot, err)
	}
	return nil
}

// emitPath emits one regular file or symlink. Sockets, devices and fifos are
// skipped. Per-file problems are logged; only a frame-write failure is
// returned.
func (e *emitter) emitPath(p, rel string, bootstrap bool) error {
	if !utf8.ValidString(rel) {
		log.Printf("skip %q: name is not valid UTF-8", rel)
		return nil
	}
	rel = filepath.ToSlash(rel)
	info, err := os.Lstat(p)
	if err != nil {
		return nil // raced with deletion
	}
	switch mode := info.Mode(); {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			log.Printf("readlink %s: %v", rel, err)
			return nil
		}
		return e.fw.WriteJSON(mirror.FrameSymlink, mirror.SymlinkMsg{Path: rel, Target: target, Bootstrap: bootstrap})
	case mode.IsRegular():
		return e.emitFile(p, rel, bootstrap)
	}
	return nil
}

func (e *emitter) emitFile(p, rel string, bootstrap bool) error {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		log.Printf("open %s: %v", rel, err)
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	if info.Size() > mirror.MaxFileSize {
		log.Printf("skip %s: %d bytes exceeds the %d byte cap", rel, info.Size(), mirror.MaxFileSize)
		return nil
	}
	if err := e.fw.WriteJSON(mirror.FrameFile, mirror.FileMsg{
		Path:      rel,
		Mode:      uint32(info.Mode().Perm()),
		MtimeNs:   info.ModTime().UnixNano(),
		Size:      info.Size(),
		Bootstrap: bootstrap,
	}); err != nil {
		return err
	}
	ok := true
	var sent int64
	buf := make([]byte, mirror.DataChunkSize)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			sent += int64(n)
			if sent > mirror.MaxFileSize {
				log.Printf("abort %s: grew past the %d byte cap while reading", rel, mirror.MaxFileSize)
				ok = false
				break
			}
			if err := e.fw.Write(mirror.FrameData, buf[:n]); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			log.Printf("read %s: %v", rel, rerr)
			ok = false
			break
		}
	}
	return e.fw.WriteJSON(mirror.FrameEnd, mirror.EndMsg{OK: ok})
}

// handleEvent dispatches one inotify event. Decisions are deliberately the
// same as bootstrapSubtree's so live and bootstrap can never disagree.
func (e *emitter) handleEvent(ev fsnotify.Event, w *fsnotify.Watcher) error {
	rel, err := filepath.Rel(e.src, ev.Name)
	if err != nil {
		return nil
	}

	// .gitignore changes: exit non-zero; the host daemon restarts the watcher
	// and bootstrap re-runs with fresh rules. Crude but very simple — no
	// in-memory invalidation logic to test or debug. Triggers on Write
	// (vim/JetBrains) AND Create/Rename (VS Code et al. save via
	// tempfile-then-rename).
	if filepath.Base(rel) == ".gitignore" &&
		(ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) || ev.Has(fsnotify.Rename)) {
		log.Printf(".gitignore %s changed (%s); exiting for restart", rel, ev.Op)
		os.Exit(1)
	}

	// Don't propagate skiplisted/ignored events. Note we deliberately don't
	// check ignore here for directories — installWatches has already
	// excluded them, so we won't see events for ignored subtrees.
	if e.shouldSkipPath(rel, false) {
		return nil
	}

	// Remove events: nothing to do (no delete tracking, by design).
	if ev.Has(fsnotify.Remove) {
		return nil
	}

	info, err := os.Lstat(ev.Name)
	if err != nil {
		// Raced with deletion; or stat-after-rename. Benign.
		return nil
	}

	if info.IsDir() {
		if !ev.Has(fsnotify.Create) {
			return nil
		}
		// New directory: install watches inside it and emit anything already
		// present (closes the new-dir-CREATE race where files are written
		// before fsnotify can Add the new dir).
		if _, werr := e.installWatches(w, ev.Name); werr != nil {
			log.Printf("watch new dir %s: %v", ev.Name, werr)
		}
		return e.bootstrapSubtree(ev.Name, false)
	}

	return e.emitPath(ev.Name, rel, false)
}

// shouldSkipPath returns true for paths the mirror must not propagate: any
// `.git` component (always), any component on the skiplist (unless
// noSkiplist), or a path matching gitignore rules.
func (e *emitter) shouldSkipPath(rel string, isDir bool) bool {
	if rel == "" || rel == "." {
		return false
	}
	for _, p := range strings.Split(filepath.ToSlash(rel), "/") {
		if e.skipDirName(p) {
			return true
		}
	}
	return mirror.IsIgnored(e.m, rel, isDir)
}
