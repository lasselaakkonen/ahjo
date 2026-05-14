//go:build darwin

// Mac-side integration with the user's ~/.ssh/config. ahjo writes a marked
// `Include ~/.ahjo-shared/ssh-config` block so the generated `Host ahjo-*`
// aliases — including per-branch ports, IdentityFile, and UserKnownHostsFile
// — are visible to every SSH client the user runs: system `ssh`, Cursor /
// VSCode Remote-SSH, plain `git`, `scp`, etc.
//
// The block is bracketed by markers so we can re-detect, no-op on repeat,
// and cleanly remove on `ahjo nuke`. If the user already has their own
// (unmarked) Include line pointing at ahjo's config, we leave the file
// alone and report OK — they're managing it themselves.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	sshIncludeBeginMarker = "# >>> ahjo-managed >>>"
	sshIncludeEndMarker   = "# <<< ahjo-managed <<<"
	sshIncludeBody        = "Include ~/.ahjo-shared/ssh-config"
)

// sshIncludeState is the result of inspecting ~/.ssh/config.
type sshIncludeState int

const (
	sshIncludeAbsent           sshIncludeState = iota // file missing or no relevant line
	sshIncludePresent                                 // ahjo-managed block present and correctly placed
	sshIncludePresentMisplaced                        // ahjo-managed block exists but sits below a Host/Match — silently ineffective
	sshIncludePresentManual                           // an unmarked Include of the same path exists
)

// manualIncludeRE matches a user-added Include line for ahjo's aggregate
// config, regardless of tilde vs absolute path. Anchored to the start of a
// line (allowing leading whitespace). Trailing comments are tolerated.
var manualIncludeRE = regexp.MustCompile(`(?m)^[ \t]*Include[ \t]+([^\s#]+)`)

// firstHostOrMatchRE locates the first `Host` or `Match` directive in an
// ssh_config so the ahjo block can be inserted ABOVE it. Putting the block
// below any Host pattern (even `Host *`) makes its included Host entries
// nest under that pattern, and they silently fail to match — see the
// "Include is conditional" semantics in ssh_config(5).
var firstHostOrMatchRE = regexp.MustCompile(`(?im)^[ \t]*(Host|Match)[ \t]+`)

// userSSHConfigPath returns ~/.ssh/config for the running user.
func userSSHConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "config"), nil
}

// matchesAhjoInclude reports whether an Include's target points at ahjo's
// aggregate ssh-config. Accepts both the tilde form (`~/.ahjo-shared/...`)
// and the absolute form (`/Users/<user>/.ahjo-shared/...`).
func matchesAhjoInclude(target, home string) bool {
	t := strings.Trim(target, `"'`)
	if strings.HasPrefix(t, "~/") {
		t = filepath.Join(home, t[2:])
	}
	if !filepath.IsAbs(t) {
		// Per ssh_config(5), relative paths resolve under ~/.ssh.
		t = filepath.Join(home, ".ssh", t)
	}
	want := filepath.Join(home, ".ahjo-shared", "ssh-config")
	return filepath.Clean(t) == filepath.Clean(want)
}

// sshIncludeStatusAt returns the current state of the Include in path,
// using home to resolve relative/tilde Include targets.
func sshIncludeStatusAt(path, home string) (sshIncludeState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sshIncludeAbsent, nil
		}
		return sshIncludeAbsent, err
	}
	content := string(b)
	if hasMarkedBlock(content) {
		bIdx := strings.Index(content, sshIncludeBeginMarker)
		if firstHostOrMatchRE.MatchString(content[:bIdx]) {
			return sshIncludePresentMisplaced, nil
		}
		return sshIncludePresent, nil
	}
	for _, m := range manualIncludeRE.FindAllStringSubmatch(content, -1) {
		if matchesAhjoInclude(m[1], home) {
			return sshIncludePresentManual, nil
		}
	}
	return sshIncludeAbsent, nil
}

// sshIncludeStatus is the no-arg variant for the running user.
func sshIncludeStatus() (sshIncludeState, error) {
	p, err := userSSHConfigPath()
	if err != nil {
		return sshIncludeAbsent, err
	}
	home, _ := os.UserHomeDir()
	return sshIncludeStatusAt(p, home)
}

// hasMarkedBlock reports whether content already contains a complete
// ahjo-managed marker pair.
func hasMarkedBlock(content string) bool {
	bIdx := strings.Index(content, sshIncludeBeginMarker)
	if bIdx < 0 {
		return false
	}
	eIdx := strings.Index(content[bIdx:], sshIncludeEndMarker)
	return eIdx > 0
}

// ensureSSHIncludeAt writes the ahjo-managed Include block into path so it
// sits ABOVE any Host/Match directive (otherwise it silently no-ops; see
// firstHostOrMatchRE for why). Creates the file (and its parent dir) if
// missing. Idempotent when the block is already present and correctly
// placed. When the block exists but is misplaced (left behind by an older
// ahjo that appended), it is removed and re-inserted at the correct
// position. Returns false when nothing on disk changed.
func ensureSSHIncludeAt(path, home string) (changed bool, err error) {
	state, err := sshIncludeStatusAt(path, home)
	if err != nil {
		return false, err
	}
	switch state {
	case sshIncludePresent, sshIncludePresentManual:
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	existing, err := os.ReadFile(path)
	mode := os.FileMode(0o600)
	if err == nil {
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}

	// Strip any misplaced block first so insertBlock can put a fresh copy
	// in the right spot without doubling up.
	if state == sshIncludePresentMisplaced && len(existing) > 0 {
		existing = []byte(stripMarkedBlock(string(existing)))
	}
	out := insertBlock(existing, buildBlock())
	if err := atomicWriteFile(path, out, mode); err != nil {
		return false, err
	}
	return true, nil
}

// insertBlock returns existing + block, placed so the block sits above the
// first `Host`/`Match` directive (because Include lines that appear below
// any Host pattern silently no-op for ssh_config's Host-matching). When
// existing has no Host/Match directives, the block is appended.
func insertBlock(existing []byte, block string) []byte {
	if len(existing) == 0 {
		return []byte(block)
	}
	loc := firstHostOrMatchRE.FindIndex(existing)
	var out bytes.Buffer
	if loc == nil {
		// No Host/Match in user's config — append.
		out.Write(existing)
		if !bytes.HasSuffix(existing, []byte("\n")) {
			out.WriteByte('\n')
		}
		out.WriteByte('\n')
		out.WriteString(block)
		return out.Bytes()
	}
	prefix := existing[:loc[0]]
	out.Write(prefix)
	if len(prefix) > 0 && !bytes.HasSuffix(prefix, []byte("\n")) {
		out.WriteByte('\n')
	}
	out.WriteString(block)
	out.WriteByte('\n')
	out.Write(existing[loc[0]:])
	return out.Bytes()
}

// ensureSSHInclude is the no-arg variant for the running user.
func ensureSSHInclude() (bool, error) {
	p, err := userSSHConfigPath()
	if err != nil {
		return false, err
	}
	home, _ := os.UserHomeDir()
	return ensureSSHIncludeAt(p, home)
}

// removeSSHIncludeAt strips the ahjo-managed block (and a trailing blank
// line, if any) from path. Returns false when no block was present.
// Manual (unmarked) Include lines are never touched — ahjo only owns what
// it explicitly marked.
func removeSSHIncludeAt(path string) (removed bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	content := string(b)
	if !hasMarkedBlock(content) {
		return false, nil
	}
	stripped := stripMarkedBlock(content)
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if err := atomicWriteFile(path, []byte(stripped), st.Mode().Perm()); err != nil {
		return false, err
	}
	return true, nil
}

// removeSSHInclude is the no-arg variant for the running user.
func removeSSHInclude() (bool, error) {
	p, err := userSSHConfigPath()
	if err != nil {
		return false, err
	}
	return removeSSHIncludeAt(p)
}

// buildBlock returns the full marked block, including a trailing newline.
func buildBlock() string {
	return sshIncludeBeginMarker + "\n" + sshIncludeBody + "\n" + sshIncludeEndMarker + "\n"
}

// stripMarkedBlock removes the ahjo-managed block from content. It eats
// any newlines immediately bracketing the markers so the file doesn't
// gain leading blanks (when the block was at the top) or a triple blank
// line (when it sat between prefix and suffix); the boundary collapses
// to exactly one blank line between non-empty neighbors.
func stripMarkedBlock(content string) string {
	bIdx := strings.Index(content, sshIncludeBeginMarker)
	if bIdx < 0 {
		return content
	}
	rel := strings.Index(content[bIdx:], sshIncludeEndMarker)
	if rel < 0 {
		return content
	}
	eEnd := bIdx + rel + len(sshIncludeEndMarker)
	for eEnd < len(content) && content[eEnd] == '\n' {
		eEnd++
	}
	start := bIdx
	for start > 0 && content[start-1] == '\n' {
		start--
	}
	before := content[:start]
	after := content[eEnd:]
	switch {
	case before == "":
		return after
	case after == "":
		if !strings.HasSuffix(before, "\n") {
			return before + "\n"
		}
		return before
	default:
		return before + "\n\n" + after
	}
}

// atomicWriteFile writes data to a temp file in dst's directory then
// renames into place, preserving mode. Caller already validated mode.
func atomicWriteFile(dst string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".ahjo.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, bytes.NewReader(data)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
