// Package mirror implements `ahjo mirror`: a one-way push of a container's
// /repo into a host directory.
//
// The in-container half (cmd/ahjo-mirror, untrusted) watches /repo and emits
// a frame stream on stdout (proto.go). The host half (hostdaemon.go,
// receiver.go) reads that stream through `incus exec` and is the only thing
// that ever writes the target; nothing of the host is mounted into the
// container. This file holds the watcher-side filters: the static skiplist
// and the git-faithful gitignore matcher.
package mirror

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// SkipDirNames is the bounded skiplist used to keep inotify watch counts
// reasonable on real-world repos. The list is deliberately narrow: only
// directories that are categorically never source code AND reliably blow
// past inotify watch limits when populated. Things like dist/build/target/vendor
// were dropped — gitignore handles them in the typical case, and they collide
// too often with real source dirs.
var SkipDirNames = map[string]bool{
	".git":          true,
	"node_modules":  true,
	".next":         true,
	".nuxt":         true,
	".svelte-kit":   true,
	".turbo":        true,
	"__pycache__":   true,
	".venv":         true,
	"venv":          true,
	".pytest_cache": true,
	".ruff_cache":   true,
	".mypy_cache":   true,
}

// ErrUnsafeSymlink is returned by ValidateSymlink when a repo symlink's target
// would leave the mirror root or reach `.git`. The mirror replicates
// container-controlled repo contents onto a host-side dir, so a symlink like
// `x -> /etc/passwd` would otherwise plant a host symlink pointing at an
// arbitrary file.
var ErrUnsafeSymlink = errors.New("unsafe symlink")

// LoadIgnoreMatcher walks `root`, reads `.gitignore` from each kept directory
// and `.git/info/exclude` once, and builds a single git-faithful matcher. The
// walk honors SkipDirNames so we never parse a `.gitignore` deep inside
// `node_modules/`.
//
// Library: github.com/go-git/go-git/v5/plumbing/format/gitignore — validated
// against `git check-ignore` on six fixtures + the live ahjo repo (0%
// disagreement); gitignore_parity_test.go pins that parity as a regression check.
func LoadIgnoreMatcher(root string, skipNoSkiplist bool) (gitignore.Matcher, error) {
	var patterns []gitignore.Pattern
	patterns = append(patterns, parseIgnoreFile(filepath.Join(root, ".git", "info", "exclude"), nil)...)

	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if p != root && (IsDotGit(d.Name()) || (!skipNoSkiplist && SkipDirNames[d.Name()])) {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		var domain []string
		if rel != "." {
			domain = strings.Split(filepath.ToSlash(rel), "/")
		}
		patterns = append(patterns, parseIgnoreFile(filepath.Join(p, ".gitignore"), domain)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return gitignore.NewMatcher(patterns), nil
}

func parseIgnoreFile(path string, domain []string) []gitignore.Pattern {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var ps []gitignore.Pattern
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		ps = append(ps, gitignore.ParsePattern(line, domain))
	}
	return ps
}

// SplitRel turns a relative path into the []string components the matcher
// expects. Empty / "." returns nil (= the repo root, which never matches a
// file pattern).
func SplitRel(rel string) []string {
	if rel == "" || rel == "." {
		return nil
	}
	return strings.Split(filepath.ToSlash(rel), "/")
}

// IsIgnored is a convenience wrapper that splits and asks the matcher.
// A nil matcher (e.g. before patterns have been loaded) returns false.
func IsIgnored(m gitignore.Matcher, rel string, isDir bool) bool {
	if m == nil {
		return false
	}
	parts := SplitRel(rel)
	if len(parts) == 0 {
		return false
	}
	return m.Match(parts, isDir)
}

// InstallSignalHandler returns a context that cancels on SIGTERM/SIGINT so
// the daemon shuts down cleanly when systemd stops the unit. The returned
// stop func deregisters the signal handler (the daemon defers it), so there's
// no goroutine left blocked on a channel for the process's lifetime.
func InstallSignalHandler() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
}
