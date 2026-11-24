package mirror

// receiver.go is the host-side writer. It consumes the watcher's frame stream
// and applies it to the mirror target through an *os.Root, treating every
// byte of the stream as hostile: the watcher runs inside the container, whose
// user can replace or impersonate it.
//
// Invariants enforced here, independent of any flag:
//   - every filesystem access goes through Root (nothing escapes the target);
//   - no path with a `.git` component is created or written;
//   - nothing is written through a symlink — parent directories are verified
//     component by component and any non-directory in the way is replaced;
//   - no directory is allowed to take the shape of a git repository
//     (HEAD next to objects/refs/commondir), which git would otherwise
//     discover and trust without any `.git` name involved.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// Receiver applies one watcher session to Root.
type Receiver struct {
	Root       *os.Root
	NoSkiplist bool
	// MaxFileSize overrides the per-file cap (tests); 0 means MaxFileSize.
	MaxFileSize int64
	// Logf receives one line per noteworthy event. Callers must treat the
	// formatted text as container-influenced; Receiver quotes every
	// container-derived string with %q.
	Logf func(format string, args ...any)
	// OnPhase is called with "bootstrapping" after the hello frame and "live"
	// after the bootstrap-done frame.
	OnPhase func(phase string)

	// open file state
	cur *openFile
}

type openFile struct {
	msg     FileMsg
	discard bool // consume D/E without writing
	f       *os.File
	tmp     string
	written int64
	confirm func() error // repo-shape re-check to run once the file exists
}

func (r *Receiver) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Receiver) maxFileSize() int64 {
	if r.MaxFileSize > 0 {
		return r.MaxFileSize
	}
	return MaxFileSize
}

// Run consumes frames until the stream ends. A clean EOF between frames
// returns nil; a protocol violation returns an error and the caller must end
// the session. Per-item problems (a refused path, a failed write) are logged
// and skipped without desynchronizing the stream.
func (r *Receiver) Run(in io.Reader) (err error) {
	defer func() {
		if r.cur != nil {
			r.abortCurrent()
		}
	}()
	fr := NewFrameReader(in)
	sawHello := false
	for {
		t, payload, err := fr.Next()
		if err == io.EOF {
			if r.cur != nil {
				return errors.New("stream ended inside a file")
			}
			if !sawHello {
				return errors.New("stream ended before hello")
			}
			return nil
		}
		if err != nil {
			return err
		}
		if !sawHello {
			if t != FrameHello {
				return fmt.Errorf("first frame is %q, want hello", byte(t))
			}
			var h Hello
			if err := json.Unmarshal(payload, &h); err != nil {
				return fmt.Errorf("bad hello: %w", err)
			}
			if h.Proto != ProtoVersion {
				return fmt.Errorf("watcher speaks protocol %d, want %d", h.Proto, ProtoVersion)
			}
			sawHello = true
			r.logf("watcher connected (version %q)", h.Version)
			if r.OnPhase != nil {
				r.OnPhase("bootstrapping")
			}
			continue
		}
		if r.cur != nil && t != FrameData && t != FrameEnd && t != FramePing {
			return fmt.Errorf("frame %q while a file is open", byte(t))
		}
		switch t {
		case FramePing:
		case FrameBootstrapDone:
			r.logf("bootstrap complete")
			if r.OnPhase != nil {
				r.OnPhase("live")
			}
		case FrameMkdir:
			var m MkdirMsg
			if err := json.Unmarshal(payload, &m); err != nil {
				return fmt.Errorf("bad mkdir frame: %w", err)
			}
			r.handleMkdir(m)
		case FrameSymlink:
			var m SymlinkMsg
			if err := json.Unmarshal(payload, &m); err != nil {
				return fmt.Errorf("bad symlink frame: %w", err)
			}
			r.handleSymlink(m)
		case FrameFile:
			var m FileMsg
			if err := json.Unmarshal(payload, &m); err != nil {
				return fmt.Errorf("bad file frame: %w", err)
			}
			r.beginFile(m)
		case FrameData:
			if r.cur == nil {
				return errors.New("data frame without an open file")
			}
			r.writeData(payload)
		case FrameEnd:
			if r.cur == nil {
				return errors.New("end frame without an open file")
			}
			var m EndMsg
			if err := json.Unmarshal(payload, &m); err != nil {
				return fmt.Errorf("bad end frame: %w", err)
			}
			r.endFile(m.OK)
		case FrameHello:
			return errors.New("duplicate hello")
		default:
			return fmt.Errorf("unknown frame type %q", byte(t))
		}
	}
}

func (r *Receiver) reject(p string, err error) {
	r.logf("rejected %q: %v", p, err)
}

// check validates p; ok=false means "do nothing for this item" (already
// logged when it was a refusal rather than a silent skiplist drop).
func (r *Receiver) check(p string) bool {
	skip, err := ValidateRelPath(p, r.NoSkiplist)
	if err != nil {
		r.reject(p, err)
		return false
	}
	return !skip
}

func (r *Receiver) handleMkdir(m MkdirMsg) {
	if !r.check(m.Path) {
		return
	}
	if err := r.ensureDir(m.Path); err != nil {
		r.reject(m.Path, err)
	}
}

func (r *Receiver) handleSymlink(m SymlinkMsg) {
	if !r.check(m.Path) {
		return
	}
	if err := ValidateSymlink(m.Path, m.Target); err != nil {
		r.reject(m.Path, err)
		return
	}
	dir, base := path.Split(m.Path)
	dir = strings.TrimSuffix(dir, "/")
	if err := r.ensureDir(dir); err != nil {
		r.reject(m.Path, err)
		return
	}
	if cur, err := r.Root.Readlink(m.Path); err == nil && cur == m.Target {
		return // already correct (bootstrap or repeated event)
	}
	confirm, err := r.checkRepoShape(dir, base)
	if err != nil {
		r.reject(m.Path, err)
		return
	}
	if err := r.Root.Remove(m.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.reject(m.Path, fmt.Errorf("replace existing entry: %w", err))
		return
	}
	if err := r.Root.Symlink(m.Target, m.Path); err != nil {
		r.reject(m.Path, err)
		return
	}
	if confirm != nil {
		if err := confirm(); err != nil {
			r.reject(m.Path, err)
			return
		}
	}
	if !m.Bootstrap {
		r.logf("ln %q", m.Path)
	}
}

func (r *Receiver) beginFile(m FileMsg) {
	r.cur = &openFile{msg: m, discard: true}
	if !r.check(m.Path) {
		return
	}
	if m.Size > r.maxFileSize() {
		r.reject(m.Path, fmt.Errorf("size %d exceeds cap %d", m.Size, r.maxFileSize()))
		return
	}
	dir, base := path.Split(m.Path)
	dir = strings.TrimSuffix(dir, "/")
	if err := r.ensureDir(dir); err != nil {
		r.reject(m.Path, err)
		return
	}
	mtime := time.Unix(0, m.MtimeNs)
	if m.Bootstrap {
		// Fast-skip: an identical file from an earlier bootstrap is left
		// alone so host-side watchers don't see the whole tree change.
		if fi, err := r.Root.Lstat(m.Path); err == nil && fi.Mode().IsRegular() &&
			fi.Size() == m.Size && fi.ModTime().Equal(mtime) {
			return
		}
	}
	if fi, err := r.Root.Lstat(m.Path); err == nil && fi.IsDir() {
		r.reject(m.Path, errors.New("a directory is in the way"))
		return
	}
	confirm, err := r.checkRepoShape(dir, base)
	if err != nil {
		r.reject(m.Path, err)
		return
	}
	r.cur.confirm = confirm
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		r.reject(m.Path, err)
		return
	}
	// Fixed-length name so a long (but valid) file name still fits.
	tmp := path.Join(dir, ".ahjo-mirror.tmp."+hex.EncodeToString(rnd[:]))
	f, err := r.Root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(m.Mode&0o777))
	if err != nil {
		r.reject(m.Path, err)
		return
	}
	r.cur.discard = false
	r.cur.f = f
	r.cur.tmp = tmp
}

func (r *Receiver) writeData(b []byte) {
	c := r.cur
	if c.discard {
		return
	}
	c.written += int64(len(b))
	if c.written > r.maxFileSize() {
		r.reject(c.msg.Path, fmt.Errorf("received more than the %d byte cap", r.maxFileSize()))
		r.dropTemp()
		return
	}
	if _, err := c.f.Write(b); err != nil {
		r.reject(c.msg.Path, err)
		r.dropTemp()
	}
}

// dropTemp discards the in-progress temp file and switches the open file to
// discard mode so the remaining D/E frames are consumed harmlessly.
func (r *Receiver) dropTemp() {
	c := r.cur
	if c.f != nil {
		_ = c.f.Close()
		_ = r.Root.Remove(c.tmp)
		c.f = nil
	}
	c.discard = true
}

func (r *Receiver) abortCurrent() {
	r.dropTemp()
	r.cur = nil
}

func (r *Receiver) endFile(ok bool) {
	c := r.cur
	r.cur = nil
	if c.discard {
		return
	}
	fail := func(err error) {
		_ = r.Root.Remove(c.tmp)
		if err != nil {
			r.reject(c.msg.Path, err)
		}
	}
	if !ok {
		_ = c.f.Close()
		fail(nil)
		return
	}
	perm := os.FileMode(c.msg.Mode & 0o777)
	if err := c.f.Chmod(perm); err != nil {
		_ = c.f.Close()
		fail(err)
		return
	}
	if err := c.f.Close(); err != nil {
		fail(err)
		return
	}
	mtime := time.Unix(0, c.msg.MtimeNs)
	_ = r.Root.Chtimes(c.tmp, mtime, mtime)
	if err := r.Root.Rename(c.tmp, c.msg.Path); err != nil {
		fail(err)
		return
	}
	if c.confirm != nil {
		if err := c.confirm(); err != nil {
			r.reject(c.msg.Path, err)
			return
		}
	}
	if !c.msg.Bootstrap {
		r.logf("cp %q", c.msg.Path)
	}
}

// ensureDir makes rel (slash-separated, "" = root) a real directory chain. It
// never traverses a symlink: each prefix is Lstat-ed and anything that is not
// a directory is removed and replaced. The source says these are directories,
// so that is also the correct mirror outcome.
func (r *Receiver) ensureDir(rel string) error {
	if rel == "" || rel == "." {
		return nil
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		fi, err := r.Root.Lstat(prefix)
		switch {
		case err == nil && fi.IsDir():
			continue
		case err == nil:
			if rerr := r.Root.Remove(prefix); rerr != nil {
				return fmt.Errorf("replace non-directory %q: %w", prefix, rerr)
			}
			r.logf("replaced non-directory %q with a directory", prefix)
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		confirm, err := r.checkRepoShape(strings.Join(parts[:i], "/"), parts[i])
		if err != nil {
			return err
		}
		if err := r.Root.Mkdir(prefix, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if confirm != nil {
			if err := confirm(); err != nil {
				return err
			}
		}
	}
	return nil
}

// repoShapePartners are the entries that, together with HEAD, make git treat
// a plain directory as a repository.
var repoShapePartners = []string{"objects", "refs", "commondir"}

func (r *Receiver) lexists(p string) bool {
	_, err := r.Root.Lstat(p)
	return err == nil
}

func (r *Receiver) hasShapePartner(dir string) bool {
	for _, n := range repoShapePartners {
		if r.lexists(path.Join(dir, n)) {
			return true
		}
	}
	return false
}

// checkRepoShape guards the creation of entry base in dir against giving dir
// the shape of a git repository: `HEAD` next to `objects`, `refs` or
// `commondir`. Git accepts any such directory as a repository — no `.git`
// name involved — and then honours its `config` (core.fsmonitor,
// core.worktree, …), so a container could otherwise assemble one from
// individually innocent paths.
//
// Which names collide is the filesystem's call, not ours (a case-insensitive
// APFS folds `refſ` onto `refs`), so existence is always probed through the
// filesystem with the canonical names. Name comparison is only used to refuse
// the obvious cases before anything is created. When the returned confirm is
// non-nil the caller must invoke it after creating the entry: it re-probes
// and, if the creation completed the shape after all, removes the entry.
func (r *Receiver) checkRepoShape(dir, base string) (confirm func() error, err error) {
	refuse := func() error {
		return fmt.Errorf("%w: would give the directory a git-repository shape", ErrUnsafePath)
	}
	if nameIs(base, "HEAD") {
		if r.hasShapePartner(dir) {
			return nil, refuse()
		}
		return nil, nil
	}
	if !r.lexists(path.Join(dir, "HEAD")) {
		return nil, nil // no HEAD here: nothing this entry can complete
	}
	if r.hasShapePartner(dir) {
		return nil, nil // already repository-shaped before us; not our doing
	}
	for _, n := range repoShapePartners {
		if nameIs(base, n) {
			return nil, refuse()
		}
	}
	entry := path.Join(dir, base)
	return func() error {
		if !r.hasShapePartner(dir) {
			return nil
		}
		_ = r.Root.Remove(entry)
		return refuse()
	}, nil
}
