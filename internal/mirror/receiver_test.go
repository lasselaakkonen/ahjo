package mirror

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateRelPath(t *testing.T) {
	long := strings.Repeat("a", 256)
	bad := []string{
		"", ".git", ".git/config", "a/.git/x", ".GIT/x", ".Git", ".g\u200cit/x", "git~1/x", "GIT~1",
		".git./x", ".git /x", "../x", "a/../x", "/abs", "a//b", "a/./b", "a/", "x\x00y", "a/\xff\xfe", long + "/x",
	}
	for _, p := range bad {
		for _, ns := range []bool{false, true} {
			if _, err := ValidateRelPath(p, ns); err == nil {
				t.Errorf("ValidateRelPath(%q, noSkiplist=%t) accepted", p, ns)
			}
		}
	}
	for _, p := range []string{".gitignore", ".github/x", "a/.gitkeep", "src/main.go", "a/b/c", ".gitmodules"} {
		if skip, err := ValidateRelPath(p, false); err != nil || skip {
			t.Errorf("ValidateRelPath(%q) = skip %t, err %v; want allowed", p, skip, err)
		}
	}
	if skip, err := ValidateRelPath("node_modules/x", false); err != nil || !skip {
		t.Errorf("node_modules: skip %t err %v, want skip", skip, err)
	}
	if skip, err := ValidateRelPath("node_modules/x", true); err != nil || skip {
		t.Errorf("node_modules with noSkiplist: skip %t err %v, want allowed", skip, err)
	}
}

func TestValidateSymlink(t *testing.T) {
	bad := []struct{ link, target string }{
		{"link", "/etc/passwd"}, {"link", "../x"}, {"a/link", "../../x"}, {"link", ".git"},
		{"a/link", "../.GIT"}, {"link", "sub/../.git/hooks"}, {"a/link", "d/../x"},
		{"p", "d/q/../../.ssh"}, {"a/link", "../.."}, {"link", ""}, {"link", "a\x00b"},
		{"a/link", "../sub/.git/hooks"},
	}
	for _, c := range bad {
		if err := ValidateSymlink(c.link, c.target); !errors.Is(err, ErrUnsafeSymlink) {
			t.Errorf("ValidateSymlink(%q -> %q) = %v, want ErrUnsafeSymlink", c.link, c.target, err)
		}
	}
	good := []struct{ link, target string }{
		{"link", "sibling"}, {"link", "nested/leaf"}, {"a/link", "../b/c"}, {"a/link", ".."}, {"a/b/link", "../../x"},
	}
	for _, c := range good {
		if err := ValidateSymlink(c.link, c.target); err != nil {
			t.Errorf("ValidateSymlink(%q -> %q) = %v, want ok", c.link, c.target, err)
		}
	}
}

func TestFrameRoundTripAndLimits(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	if err := fw.WriteJSON(FrameHello, Hello{Proto: ProtoVersion, Version: "v"}); err != nil {
		t.Fatal(err)
	}
	if err := fw.Write(FrameData, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := fw.Write(FramePing, nil); err != nil {
		t.Fatal(err)
	}
	if err := fw.Write(FrameData, make([]byte, MaxFramePayload+1)); err == nil {
		t.Error("writer accepted an oversize payload")
	}
	fr := NewFrameReader(&buf)
	for i, want := range []FrameType{FrameHello, FrameData, FramePing} {
		got, payload, err := fr.Next()
		if err != nil || got != want {
			t.Fatalf("frame %d: %q, %v", i, byte(got), err)
		}
		if want == FrameData && string(payload) != "abc" {
			t.Errorf("data payload %q", payload)
		}
	}
	if _, _, err := fr.Next(); err != io.EOF {
		t.Errorf("end of stream: %v, want io.EOF", err)
	}
	// A hostile length must be refused before any allocation.
	huge := []byte{'D', 0xff, 0xff, 0xff, 0xff}
	if _, _, err := NewFrameReader(bytes.NewReader(huge)).Next(); err == nil {
		t.Error("reader accepted an oversize frame length")
	}
}

// stream builds a watcher session for tests.
type stream struct {
	t   *testing.T
	buf bytes.Buffer
	fw  *FrameWriter
}

func newStream(t *testing.T) *stream {
	s := &stream{t: t}
	s.fw = NewFrameWriter(&s.buf)
	s.json(FrameHello, Hello{Proto: ProtoVersion, Version: "test"})
	return s
}

func (s *stream) json(ft FrameType, v any) *stream {
	s.t.Helper()
	if err := s.fw.WriteJSON(ft, v); err != nil {
		s.t.Fatal(err)
	}
	return s
}

func (s *stream) raw(ft FrameType, b []byte) *stream {
	s.t.Helper()
	if err := s.fw.Write(ft, b); err != nil {
		s.t.Fatal(err)
	}
	return s
}

var testMtime = time.Unix(1700000000, 123000000)

func (s *stream) file(p, content string, mode uint32, bootstrap bool) *stream {
	s.json(FrameFile, FileMsg{Path: p, Mode: mode, MtimeNs: testMtime.UnixNano(), Size: int64(len(content)), Bootstrap: bootstrap})
	if content != "" {
		s.raw(FrameData, []byte(content))
	}
	return s.json(FrameEnd, EndMsg{OK: true})
}

func (s *stream) mkdir(p string) *stream { return s.json(FrameMkdir, MkdirMsg{Path: p}) }
func (s *stream) link(p, target string) *stream {
	return s.json(FrameSymlink, SymlinkMsg{Path: p, Target: target})
}

type recvResult struct {
	dir string
	log string
	err error
}

func runRecv(t *testing.T, dir string, s *stream, tweak func(*Receiver)) recvResult {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	var log strings.Builder
	r := &Receiver{Root: root, Logf: func(f string, a ...any) { log.WriteString(sprintfLine(f, a...)) }}
	if tweak != nil {
		tweak(r)
	}
	err = r.Run(&s.buf)
	return recvResult{dir: dir, log: log.String(), err: err}
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), ".ahjo-mirror.tmp.") {
			t.Errorf("leftover temp file %s", p)
		}
		return nil
	})
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestReceiver_WritesFiles(t *testing.T) {
	dir := t.TempDir()
	s := newStream(t).mkdir("empty").file("a/b/f.txt", "hello", 0o4755, true).link("a/l", "b/f.txt").raw(FrameBootstrapDone, nil)
	var phases []string
	res := runRecv(t, dir, s, func(r *Receiver) { r.OnPhase = func(p string) { phases = append(phases, p) } })
	if res.err != nil {
		t.Fatal(res.err)
	}
	p := filepath.Join(dir, "a/b/f.txt")
	if got := mustRead(t, p); got != "hello" {
		t.Errorf("content %q", got)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o755 || fi.Mode()&os.ModeSetuid != 0 {
		t.Errorf("mode %v, want 0755 without setuid", fi.Mode())
	}
	if !fi.ModTime().Equal(testMtime) {
		t.Errorf("mtime %v, want %v", fi.ModTime(), testMtime)
	}
	if fi, err := os.Stat(filepath.Join(dir, "empty")); err != nil || !fi.IsDir() {
		t.Errorf("empty dir not created: %v", err)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "a/l")); got != "b/f.txt" {
		t.Errorf("symlink target %q", got)
	}
	if strings.Join(phases, ",") != "bootstrapping,live" {
		t.Errorf("phases %v", phases)
	}
	assertNoTemps(t, dir)
}

func TestReceiver_BootstrapFastSkip(t *testing.T) {
	dir := t.TempDir()
	if res := runRecv(t, dir, newStream(t).file("f", "same", 0o644, true), nil); res.err != nil {
		t.Fatal(res.err)
	}
	p := filepath.Join(dir, "f")
	// Same size + mtime but different bytes: a skipped bootstrap leaves the
	// host copy alone; a live event always rewrites.
	if err := os.WriteFile(p, []byte("HOST"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, testMtime, testMtime); err != nil {
		t.Fatal(err)
	}
	if res := runRecv(t, dir, newStream(t).file("f", "same", 0o644, true), nil); res.err != nil {
		t.Fatal(res.err)
	}
	if got := mustRead(t, p); got != "HOST" {
		t.Errorf("bootstrap rewrote an unchanged file: %q", got)
	}
	if res := runRecv(t, dir, newStream(t).file("f", "same", 0o644, false), nil); res.err != nil {
		t.Fatal(res.err)
	}
	if got := mustRead(t, p); got != "same" {
		t.Errorf("live event did not rewrite: %q", got)
	}
}

func TestReceiver_AbortAndTruncation(t *testing.T) {
	dir := t.TempDir()
	s := newStream(t).json(FrameFile, FileMsg{Path: "x", Mode: 0o644}).raw(FrameData, []byte("partial")).json(FrameEnd, EndMsg{OK: false})
	if res := runRecv(t, dir, s, nil); res.err != nil {
		t.Fatal(res.err)
	}
	if lexists(filepath.Join(dir, "x")) {
		t.Error("aborted file was written")
	}
	assertNoTemps(t, dir)

	s = newStream(t).json(FrameFile, FileMsg{Path: "y", Mode: 0o644}).raw(FrameData, []byte("partial"))
	if res := runRecv(t, dir, s, nil); res.err == nil {
		t.Error("truncated stream returned nil")
	}
	if lexists(filepath.Join(dir, "y")) {
		t.Error("truncated file was written")
	}
	assertNoTemps(t, dir)
}

func TestReceiver_RejectsDotGit(t *testing.T) {
	for _, noSkip := range []bool{false, true} {
		dir := t.TempDir()
		s := newStream(t).
			file(".git/hooks/pre-commit", "#!/bin/sh\n", 0o755, false).
			file(".GIT/config", "x", 0o644, false).
			mkdir("sub/.git").
			link("x", ".git").
			file("ok.txt", "fine", 0o644, false)
		res := runRecv(t, dir, s, func(r *Receiver) { r.NoSkiplist = noSkip })
		if res.err != nil {
			t.Fatal(res.err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 || entries[0].Name() != "ok.txt" {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("noSkiplist=%t: target holds %v, want only ok.txt", noSkip, names)
		}
		if strings.Count(res.log, "rejected") != 4 {
			t.Errorf("want 4 rejections, log:\n%s", res.log)
		}
	}
}

func TestReceiver_NeverWritesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git/config"), []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git", filepath.Join(dir, "x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	s := newStream(t).file("x/config", "pwned", 0o644, false).file("out/pwn", "pwned", 0o644, false)
	if res := runRecv(t, dir, s, nil); res.err != nil {
		t.Fatal(res.err)
	}
	if got := mustRead(t, filepath.Join(dir, ".git/config")); got != "orig" {
		t.Errorf(".git/config was modified: %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "x")); err != nil || !fi.IsDir() {
		t.Errorf("x should have been replaced by a real directory (err %v)", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("wrote outside the root: %v", entries)
	}
	if got := mustRead(t, filepath.Join(dir, "out/pwn")); got != "pwned" {
		t.Errorf("out/pwn not written inside the root: %q", got)
	}
}

func TestReceiver_RepoShapeGuard(t *testing.T) {
	dir := t.TempDir()
	s := newStream(t).
		file("HEAD", "ref: refs/heads/main\n", 0o644, false).
		file("config", "[core]\n\tfsmonitor = touch /tmp/pwned\n", 0o644, false).
		mkdir("objects").
		mkdir("refs").
		file("refs/heads/main", "x", 0o644, false).
		mkdir("sub/refs").
		file("sub/head", "ref: refs/heads/main\n", 0o644, false).
		mkdir("lone/objects").
		mkdir("lone/refs").
		file("solo/HEAD", "just a file", 0o644, false)
	res := runRecv(t, dir, s, nil)
	if res.err != nil {
		t.Fatal(res.err)
	}
	for _, p := range []string{"objects", "refs", "sub/head"} {
		if lexists(filepath.Join(dir, p)) {
			t.Errorf("%s was created; it completes a git-repository shape", p)
		}
	}
	for _, p := range []string{"HEAD", "config", "sub/refs", "lone/objects", "lone/refs", "solo/HEAD"} {
		if !lexists(filepath.Join(dir, p)) {
			t.Errorf("%s should have been allowed", p)
		}
	}
	if _, err := exec.LookPath("git"); err == nil {
		cmd := exec.Command("git", "-C", dir, "rev-parse", "--git-dir")
		cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(dir), "HOME="+t.TempDir())
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("git treats the target as a repository: %s", out)
		}
	}
}

func TestReceiver_SizeCap(t *testing.T) {
	dir := t.TempDir()
	s := newStream(t).
		file("declared-big", "0123456789", 0o644, false).
		json(FrameFile, FileMsg{Path: "lying", Mode: 0o644, Size: 1}).raw(FrameData, []byte("0123456789")).json(FrameEnd, EndMsg{OK: true}).
		file("small", "ok", 0o644, false)
	res := runRecv(t, dir, s, func(r *Receiver) { r.MaxFileSize = 4 })
	if res.err != nil {
		t.Fatal(res.err)
	}
	if lexists(filepath.Join(dir, "declared-big")) || lexists(filepath.Join(dir, "lying")) {
		t.Error("over-cap file was written")
	}
	if !lexists(filepath.Join(dir, "small")) {
		t.Error("stream desynchronized after an over-cap file")
	}
	assertNoTemps(t, dir)
}

func TestReceiver_LogQuotesHostileText(t *testing.T) {
	res := runRecv(t, t.TempDir(), newStream(t).mkdir(".git/\x1b[2Jx"), nil)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if strings.ContainsRune(res.log, 0x1b) || !strings.Contains(res.log, "rejected") {
		t.Errorf("log carries a raw escape or no rejection: %q", res.log)
	}
}

func TestReceiver_StructuralErrors(t *testing.T) {
	cases := map[string]func(*testing.T) *stream{
		"data without file": func(t *testing.T) *stream { return newStream(t).raw(FrameData, []byte("x")) },
		"end without file":  func(t *testing.T) *stream { return newStream(t).json(FrameEnd, EndMsg{OK: true}) },
		"unknown type":      func(t *testing.T) *stream { return newStream(t).raw(FrameType('Z'), nil) },
		"file inside file": func(t *testing.T) *stream {
			return newStream(t).json(FrameFile, FileMsg{Path: "a"}).json(FrameFile, FileMsg{Path: "b"})
		},
		"bad json": func(t *testing.T) *stream { return newStream(t).raw(FrameMkdir, []byte("{")) },
		"no hello": func(t *testing.T) *stream {
			s := &stream{t: t}
			s.fw = NewFrameWriter(&s.buf)
			return s.mkdir("a")
		},
		"proto mismatch": func(t *testing.T) *stream {
			s := &stream{t: t}
			s.fw = NewFrameWriter(&s.buf)
			return s.json(FrameHello, Hello{Proto: ProtoVersion + 1})
		},
		"empty stream": func(t *testing.T) *stream {
			s := &stream{t: t}
			s.fw = NewFrameWriter(&s.buf)
			return s
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if res := runRecv(t, dir, build(t), nil); res.err == nil {
				t.Error("want a structural error, got nil")
			}
			assertNoTemps(t, dir)
		})
	}
}

func sprintfLine(format string, args ...any) string {
	return fmt.Sprintf(format, args...) + "\n"
}

// TestReceiver_RepoShapeGuard_FilesystemFolding: which names collide with
// HEAD/objects/refs is up to the target filesystem (case-insensitive APFS
// folds U+017F onto `s`). Whatever it decides, the hostile sequence must not
// leave a directory git accepts as a repository.
func TestReceiver_RepoShapeGuard_FilesystemFolding(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for _, order := range [][]string{
		{"object\u017f", "ref\u017f", "HEAD"},
		{"HEAD", "object\u017f", "ref\u017f"},
		{"OBJECTS", "Refs", "head"},
		{"head", "OBJECTS", "Refs"},
	} {
		dir := t.TempDir()
		s := newStream(t).file("x/config", "[core]\n\tbare = false\n\tworktree = .\n", 0o644, false)
		for _, n := range order {
			if nameIs(n, "HEAD") {
				s.file("x/"+n, "ref: refs/heads/main\n", 0o644, false)
			} else {
				s.mkdir("x/" + n)
			}
		}
		if res := runRecv(t, dir, s, nil); res.err != nil {
			t.Fatal(res.err)
		}
		cmd := exec.Command("git", "-C", filepath.Join(dir, "x"), "rev-parse", "--git-dir")
		cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+dir, "HOME="+t.TempDir())
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("order %q: git accepts the directory as a repository: %s", order, out)
		}
	}
}

func TestReceiver_LongNameAndDirectoryInTheWay(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("n", 240)
	if err := os.Mkdir(filepath.Join(dir, "isdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := newStream(t).file(long, "x", 0o644, false).file("isdir", "x", 0o644, false).file("after", "x", 0o644, false)
	res := runRecv(t, dir, s, nil)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if !lexists(filepath.Join(dir, long)) {
		t.Error("a 240-byte file name was not mirrored")
	}
	if fi, err := os.Lstat(filepath.Join(dir, "isdir")); err != nil || !fi.IsDir() {
		t.Error("a directory was replaced by a file")
	}
	if !lexists(filepath.Join(dir, "after")) {
		t.Error("stream desynchronized")
	}
	assertNoTemps(t, dir)
}
