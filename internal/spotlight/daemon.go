package spotlight

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// SkipDirNames is the bounded skiplist used to keep inotify watch counts
// reasonable on real-world repos. The names match anywhere in the tree.
// rsync's `:- .gitignore` filter is the source of truth for what gets copied;
// this list only constrains what we WATCH so a `node_modules/` of 80k dirs
// doesn't exhaust fs.inotify.max_user_watches before the first sync runs.
var SkipDirNames = map[string]bool{
	".git":          true,
	".coi":          true,
	"node_modules":  true,
	".next":         true,
	".nuxt":         true,
	".svelte-kit":   true,
	"dist":          true,
	"build":         true,
	"target":        true,
	"vendor":        true,
	"__pycache__":   true,
	".venv":         true,
	"venv":          true,
	".pytest_cache": true,
	".ruff_cache":   true,
	".mypy_cache":   true,
	".turbo":        true,
}

// debounce is the quiet window after the last fsnotify event before we run
// rsync. 200ms is short enough to feel live in an editor, long enough to
// coalesce save-on-blur bursts and tool-driven multi-file rewrites.
const debounce = 200 * time.Millisecond

// Bootstrap runs a single rsync from src to dst. Used at activation time to
// bring the target into sync before starting the live watcher.
func Bootstrap(src, dst string, out io.Writer) error {
	return rsync(src, dst, out)
}

// RunDaemon is the long-lived watcher loop. Returns when ctx is cancelled or
// the watcher fails fatally. Diagnostic output goes to out (typically the
// spotlight log file).
func RunDaemon(ctx context.Context, src, dst string, out io.Writer) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify new: %w", err)
	}
	defer w.Close()

	if err := addRecursive(w, src); err != nil {
		return fmt.Errorf("watch %s: %w", src, err)
	}

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	pending := false

	fmt.Fprintf(out, "[%s] watching %s -> %s\n", time.Now().Format(time.RFC3339), src, dst)
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(out, "[%s] shutdown\n", time.Now().Format(time.RFC3339))
			return nil

		case ev, ok := <-w.Events:
			if !ok {
				return fmt.Errorf("watcher events channel closed")
			}
			// Newly-created directories need to be added to the watch so events
			// inside them aren't missed. fsnotify auto-removes watches for
			// deleted dirs.
			if ev.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() && !SkipDirNames[filepath.Base(ev.Name)] {
					_ = addRecursive(w, ev.Name)
				}
			}
			if !pending {
				timer.Reset(debounce)
				pending = true
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(debounce)
			}

		case err, ok := <-w.Errors:
			if !ok {
				return fmt.Errorf("watcher errors channel closed")
			}
			fmt.Fprintf(out, "[%s] watch error: %v\n", time.Now().Format(time.RFC3339), err)

		case <-timer.C:
			pending = false
			if err := rsync(src, dst, out); err != nil {
				fmt.Fprintf(out, "[%s] rsync: %v\n", time.Now().Format(time.RFC3339), err)
			}
		}
	}
}

// addRecursive walks root and registers every non-skipped directory with w.
// Errors during walk are swallowed: missing dirs (raced with deletion) and
// per-dir Add failures (e.g., ENOSPC) shouldn't abort the whole walk.
func addRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && SkipDirNames[d.Name()] {
			return filepath.SkipDir
		}
		_ = w.Add(path)
		return nil
	})
}

// rsync invokes the system rsync with the standard ahjo-spotlight flags:
//   - -a: archive mode (recursive, perms, times, symlinks-as-symlinks)
//   - --delete-during: prune target files that disappeared from source, but
//     only those NOT excluded by the filter (so Mac-side build artifacts in
//     gitignored paths survive)
//   - --filter=':- .gitignore': per-dir merge of .gitignore as exclude rules,
//     respected at every level of the tree
//   - hard-coded `.git` and `.coi` excludes regardless of gitignore
func rsync(src, dst string, out io.Writer) error {
	if !strings.HasSuffix(src, "/") {
		src += "/"
	}
	if !strings.HasSuffix(dst, "/") {
		dst += "/"
	}
	cmd := exec.Command("rsync",
		"-a",
		"--delete-during",
		"--filter=:- .gitignore",
		"--exclude=.git",
		"--exclude=.coi",
		src, dst,
	)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// InstallSignalHandler returns a context that cancels on SIGTERM/SIGINT so
// the daemon shuts down cleanly when the user runs `ahjo spotlight off` (which
// SIGTERMs the recorded PID).
func InstallSignalHandler() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}
