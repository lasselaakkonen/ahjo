package mirror

// pathcheck.go holds the host-side validation of everything the untrusted
// watcher names: relative paths and symlink targets. These are the security
// invariants of the mirror — gitignore and the skiplist are only functional
// filters, but nothing here may be relaxed by a flag.

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	maxRelPathLen   = 4096
	maxComponentLen = 255
)

// ErrUnsafePath is returned for a relative path the host refuses to write.
var ErrUnsafePath = errors.New("unsafe path")

// nameIs reports whether a file name equals want (an ASCII name) the way a
// case-insensitive Mac filesystem would see it: under Unicode simple case
// folding (so U+017F `ſ` matches `s`), ignoring the code points HFS+ ignores.
func nameIs(name, want string) bool {
	return strings.EqualFold(stripIgnorable(name), want)
}

// stripIgnorable drops the code points HFS+ ignores in file names (the set
// git strips in is_hfs_dotgit).
func stripIgnorable(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 0x200C && r <= 0x200F,
			r >= 0x202A && r <= 0x202E,
			r >= 0x206A && r <= 0x206F,
			r == 0xFEFF:
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// IsDotGit reports whether a single path component would be treated as `.git`
// by any filesystem a mirror target can live on: case-insensitively, ignoring
// HFS+-ignorable code points, with trailing dots/spaces trimmed (NTFS/SMB),
// or via the NTFS short name.
func IsDotGit(component string) bool {
	f := stripIgnorable(component)
	if strings.EqualFold(f, ".git") || strings.EqualFold(f, "git~1") {
		return true
	}
	return strings.EqualFold(strings.TrimRight(f, ". "), ".git")
}

// ValidateRelPath checks a watcher-supplied path. err means the path is
// refused outright; skip means it is dropped silently because a component is
// on the static skiplist (a functional filter the watcher normally applies
// itself). A `.git` component is refused regardless of noSkiplist.
func ValidateRelPath(p string, noSkiplist bool) (skip bool, err error) {
	switch {
	case p == "":
		return false, fmt.Errorf("%w: empty", ErrUnsafePath)
	case len(p) > maxRelPathLen:
		return false, fmt.Errorf("%w: too long", ErrUnsafePath)
	case !utf8.ValidString(p):
		return false, fmt.Errorf("%w: not valid UTF-8", ErrUnsafePath)
	case strings.ContainsRune(p, 0):
		return false, fmt.Errorf("%w: contains NUL", ErrUnsafePath)
	case p != path.Clean(p), path.IsAbs(p), !filepath.IsLocal(filepath.FromSlash(p)):
		return false, fmt.Errorf("%w: not a clean relative path", ErrUnsafePath)
	}
	for _, c := range strings.Split(p, "/") {
		switch {
		case c == "" || c == "." || c == "..":
			return false, fmt.Errorf("%w: not a clean relative path", ErrUnsafePath)
		case len(c) > maxComponentLen:
			return false, fmt.Errorf("%w: component too long", ErrUnsafePath)
		case IsDotGit(c):
			return false, fmt.Errorf("%w: .git component", ErrUnsafePath)
		}
		if !noSkiplist && SkipDirNames[c] {
			skip = true
		}
	}
	return skip, nil
}

// ValidateSymlink checks a symlink the watcher wants at linkPath (already
// validated) pointing at target. Only relative targets of the form
// `../../a/b` are accepted: once a `..` may follow a name, lexical resolution
// is unsound (the name can itself be a symlink), so that form is refused. The
// target must stay inside the mirror root and must not name `.git`.
func ValidateSymlink(linkPath, target string) error {
	switch {
	case target == "":
		return fmt.Errorf("%w: empty target", ErrUnsafeSymlink)
	case len(target) > maxRelPathLen:
		return fmt.Errorf("%w: target too long", ErrUnsafeSymlink)
	case !utf8.ValidString(target), strings.ContainsRune(target, 0):
		return fmt.Errorf("%w: target not valid text", ErrUnsafeSymlink)
	case strings.HasPrefix(target, "/"):
		return fmt.Errorf("%w: absolute target", ErrUnsafeSymlink)
	}
	depth := 0
	if dir := path.Dir(linkPath); dir != "." {
		depth = len(strings.Split(dir, "/"))
	}
	ups, seenName := 0, false
	for _, c := range strings.Split(target, "/") {
		switch c {
		case "", ".":
			continue
		case "..":
			if seenName {
				return fmt.Errorf("%w: `..` after a name", ErrUnsafeSymlink)
			}
			ups++
		default:
			seenName = true
			if IsDotGit(c) {
				return fmt.Errorf("%w: target names .git", ErrUnsafeSymlink)
			}
		}
	}
	if ups > depth {
		return fmt.Errorf("%w: target escapes mirror root", ErrUnsafeSymlink)
	}
	return nil
}
