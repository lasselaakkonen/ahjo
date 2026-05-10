// Package mirror provides shared helpers for the in-container `ahjo-mirror`
// daemon (cmd/ahjo-mirror) and for unit tests that pin git-faithful gitignore
// parity. The daemon itself is Linux-only; this package compiles everywhere
// because the helpers are pure Go (filesystem walking + go-git gitignore).
package mirror

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// SkipDirNames is the bounded skiplist used to keep inotify watch counts
// reasonable on real-world repos. Per the v3 design (designdocs/in-container-mirror.md):
// only directories that are categorically never source code AND reliably blow
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

// ErrUnsupportedFileType is returned by CopyFile for sockets, devices, fifos.
var ErrUnsupportedFileType = errors.New("unsupported file type")

// LoadIgnoreMatcher walks `root`, reads `.gitignore` from each kept directory
// and `.git/info/exclude` once, and builds a single git-faithful matcher. The
// walk honors SkipDirNames so we never parse a `.gitignore` deep inside
// `node_modules/`.
//
// Library: github.com/go-git/go-git/v5/plumbing/format/gitignore — validated
// against `git check-ignore` on six fixtures + the live ahjo repo (0%
// disagreement, see designdocs/in-container-mirror.md "Spike: gitignore parity").
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
		if !skipNoSkiplist && p != root && SkipDirNames[d.Name()] {
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

// CopyFile copies srcPath to dstPath using lstat-first dispatch.
//   - Regular file → tempfile in dstPath's dir, io.Copy, fchmod, atomic rename.
//   - Symlink     → readlink, remove existing dst (if any), os.Symlink.
//   - Anything else → ErrUnsupportedFileType (caller logs and skips).
//
// When fastSkip is true AND the destination already exists with matching
// size+mtime (regular file) or matching link target (symlink), the copy is
// skipped. The bootstrap walk passes fastSkip=true so repeated bootstraps are
// cheap; live event handling passes fastSkip=false because the event itself
// is the "something changed" signal.
//
// Caller is responsible for ensuring dstPath's parent dir exists.
func CopyFile(srcPath, dstPath string, fastSkip bool) error {
	srcInfo, err := os.Lstat(srcPath)
	if err != nil {
		return err
	}
	mode := srcInfo.Mode()
	switch {
	case mode.IsRegular():
		return copyRegular(srcPath, dstPath, srcInfo, fastSkip)
	case mode&os.ModeSymlink != 0:
		return copySymlink(srcPath, dstPath, fastSkip)
	default:
		return ErrUnsupportedFileType
	}
}

func copyRegular(srcPath, dstPath string, srcInfo os.FileInfo, fastSkip bool) error {
	if fastSkip {
		if dstInfo, err := os.Lstat(dstPath); err == nil && dstInfo.Mode().IsRegular() &&
			dstInfo.Size() == srcInfo.Size() && dstInfo.ModTime().Equal(srcInfo.ModTime()) {
			return nil
		}
	}
	src, err := os.OpenFile(srcPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open src %s: %w", srcPath, err)
	}
	defer src.Close()

	dstDir := filepath.Dir(dstPath)
	tmp, err := tempName(dstDir, filepath.Base(dstPath))
	if err != nil {
		return err
	}
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, srcInfo.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create tmp %s: %w", tmp, err)
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("copy %s -> %s: %w", srcPath, tmp, err)
	}
	if err := out.Chmod(srcInfo.Mode().Perm()); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("fchmod %s: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	// Preserve mtime so fastSkip works on the next bootstrap.
	_ = os.Chtimes(tmp, srcInfo.ModTime(), srcInfo.ModTime())
	if err := os.Rename(tmp, dstPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, dstPath, err)
	}
	return nil
}

func copySymlink(srcPath, dstPath string, fastSkip bool) error {
	target, err := os.Readlink(srcPath)
	if err != nil {
		return fmt.Errorf("readlink %s: %w", srcPath, err)
	}
	if fastSkip {
		if cur, err := os.Readlink(dstPath); err == nil && cur == target {
			return nil
		}
	}
	// os.Symlink fails if dstPath exists; remove it first. Tolerate ENOENT.
	if err := os.Remove(dstPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove existing %s: %w", dstPath, err)
	}
	if err := os.Symlink(target, dstPath); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", dstPath, target, err)
	}
	return nil
}

func tempName(dir, base string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, "."+base+".ahjo-mirror.tmp."+hex.EncodeToString(b[:])), nil
}

// InstallSignalHandler returns a context that cancels on SIGTERM/SIGINT so
// the daemon shuts down cleanly when systemd stops the unit.
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
