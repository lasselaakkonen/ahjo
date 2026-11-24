package mirror

// hostdaemon.go is the host-side mirror daemon: the long-lived process
// (`ahjo _mirror-daemon`) that supervises the in-container watcher and feeds
// its frame stream to a Receiver. It also owns the small amount of on-disk
// state that replaced "the container has a mirror device + an active unit":
//
//   - state.json  — written by the CLI: which container mirrors to which
//     target. Its existence means "a mirror is configured".
//   - daemon.lock — flock-ed by the daemon for its lifetime. Held = alive.
//   - status.json — written by the daemon: phase + its own version.
//   - daemon.log  — the daemon's log (container-influenced; see FileLogger).

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// State is the CLI-owned description of the configured mirror.
type State struct {
	// Session is minted by every `mirror on`; the daemon only serves the
	// session it started with.
	Session    string `json:"session"`
	Container  string `json:"container"`
	Slug       string `json:"slug"`
	Alias      string `json:"alias"`
	Target     string `json:"target"`
	NoSkiplist bool   `json:"no_skiplist"`
	// Stopping is a tombstone written before the daemon is stopped, so no
	// concurrent command revives it and a just-started daemon exits before
	// writing — the target must be quiescent for the revert that follows.
	Stopping  bool      `json:"stopping,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// Status is the daemon-owned progress report.
type Status struct {
	Session   string    `json:"session"`
	Version   string    `json:"version"`
	Phase     string    `json:"phase"`
	Detail    string    `json:"detail,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Daemon phases.
const (
	PhaseStarting      = "starting"
	PhaseWaiting       = "waiting-for-container"
	PhaseRestarting    = "restarting"
	PhaseBootstrapping = "bootstrapping"
	PhaseLive          = "live"
)

// NewSessionID returns a random session id.
func NewSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func writeJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// LoadState returns the configured mirror, or (nil, nil) when none is.
func LoadState() (*State, error) {
	b, err := os.ReadFile(paths.MirrorStatePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", paths.MirrorStatePath(), err)
	}
	return &st, nil
}

// SaveState persists st. Callers hold the ahjo lockfile.
func SaveState(st *State) error { return writeJSONAtomic(paths.MirrorStatePath(), st) }

// ClearState removes the state and status files.
func ClearState() error {
	_ = os.Remove(paths.MirrorStatusPath())
	if err := os.Remove(paths.MirrorStatePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// LoadStatus returns the daemon's last status report, or nil.
func LoadStatus() *Status {
	b, err := os.ReadFile(paths.MirrorStatusPath())
	if err != nil {
		return nil
	}
	var s Status
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

// DaemonAlive reports whether a daemon currently holds the daemon lock.
func DaemonAlive() bool {
	f, err := os.Open(paths.MirrorDaemonLockPath())
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// daemonPid returns the pid recorded in the lock file, but only when
// /proc confirms that pid is an ahjo mirror daemon. The lock (not the pid) is
// the liveness signal; this guard keeps a stale pid from ever being signaled.
func daemonPid() (int, bool) {
	b, err := os.ReadFile(paths.MirrorDaemonLockPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0, false
	}
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || !strings.Contains(string(cmdline), DaemonCommandName) {
		return 0, false
	}
	return pid, true
}

// DaemonCommandName is the hidden CLI subcommand that runs the daemon; it is
// what StopDaemon looks for in /proc/<pid>/cmdline.
const DaemonCommandName = "_mirror-daemon"

// StopDaemon stops a running daemon and returns once the lock is free. It
// returns an error if the daemon is still alive after timeout (plus a short
// SIGKILL grace) — callers must not touch the target in that case.
func StopDaemon(timeout time.Duration) error {
	if !DaemonAlive() {
		return nil
	}
	if pid, ok := daemonPid(); ok {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !DaemonAlive() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pid, ok := daemonPid(); ok && DaemonAlive() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	for i := 0; i < 30; i++ {
		if !DaemonAlive() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("mirror daemon is still running after stop; aborting to avoid racing a live writer")
}

// acquireDaemonLock takes the exclusive daemon lock, retrying briefly:
// DaemonAlive probes hold a shared lock for an instant and must not make a
// starting daemon conclude another one is running.
func acquireDaemonLock() (*os.File, error) {
	if err := os.MkdirAll(paths.MirrorRunDir(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(paths.MirrorDaemonLockPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for i := 0; ; i++ {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || i >= 20 {
			_ = f.Close()
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return f, nil
}

// Session is one running watcher.
type Session struct {
	Stdout io.Reader
	Stderr io.Reader // may be nil
	// Stop kills and reaps the watcher. Must be idempotent and safe to call
	// concurrently with reads; it must make pending reads return.
	Stop func()
}

// SpawnFunc starts the in-container watcher for st. forcePush asks the
// implementation to re-push the watcher binary first (the previous session
// never produced a valid hello, so the binary there is suspect).
type SpawnFunc func(ctx context.Context, st *State, forcePush bool) (*Session, error)

// DaemonOptions configures RunDaemon.
type DaemonOptions struct {
	Spawn   SpawnFunc
	Version string
	Logf    func(format string, args ...any)
	// ContainerExists, when set, lets the daemon give up once the container
	// is gone for good.
	ContainerExists func(name string) (bool, error)
	// Tunables; zero values pick production defaults.
	RetryMin, RetryMax time.Duration
	WatchdogTimeout    time.Duration
}

func (o *DaemonOptions) defaults() {
	if o.RetryMin == 0 {
		o.RetryMin = 2 * time.Second
	}
	if o.RetryMax == 0 {
		o.RetryMax = 30 * time.Second
	}
	if o.WatchdogTimeout == 0 {
		o.WatchdogTimeout = 30 * time.Second
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
}

// RunDaemon serves the configured mirror until ctx is done or the state it
// started with is cleared, tombstoned or replaced. It returns nil when
// another daemon already holds the lock.
func RunDaemon(ctx context.Context, o DaemonOptions) error {
	o.defaults()
	lock, err := acquireDaemonLock()
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	defer func() { _ = lock.Close() }()

	st, err := LoadState()
	if err != nil {
		return err
	}
	if st == nil || st.Stopping {
		return nil
	}
	root, err := os.OpenRoot(st.Target)
	if err != nil {
		o.Logf("open target %q: %v", st.Target, err)
		return err
	}
	defer func() { _ = root.Close() }()

	// valid re-reads the state: the daemon must stop writing the moment the
	// CLI tombstones or replaces the session it serves.
	valid := func() bool {
		if ctx.Err() != nil {
			return false
		}
		cur, err := LoadState()
		return err == nil && cur != nil && !cur.Stopping && cur.Session == st.Session
	}
	setStatus := func(phase, detail string) {
		_ = writeJSONAtomic(paths.MirrorStatusPath(), Status{
			Session: st.Session, Version: o.Version, Phase: phase, Detail: detail, UpdatedAt: time.Now(),
		})
	}

	o.Logf("daemon started (version %q) container=%q target=%q no-skiplist=%t", o.Version, st.Container, st.Target, st.NoSkiplist)
	delay := o.RetryMin
	missing := 0
	forcePush := false
	for {
		if !valid() {
			o.Logf("daemon exiting (mirror stopped or replaced)")
			return nil
		}
		setStatus(PhaseStarting, "")
		reachedHello := false
		sess, err := o.Spawn(ctx, st, forcePush)
		forcePush = false
		if err != nil {
			o.Logf("start watcher: %v", err)
		} else {
			reachedHello = runSession(ctx, &o, st, root, sess, setStatus)
			forcePush = !reachedHello
		}
		if !valid() {
			o.Logf("daemon exiting (mirror stopped or replaced)")
			return nil
		}
		if o.ContainerExists != nil {
			if ok, err := o.ContainerExists(st.Container); err == nil && !ok {
				missing++
				if missing >= 3 {
					o.Logf("container %q no longer exists; daemon exiting (run `ahjo mirror off` to clean up)", st.Container)
					return nil
				}
			} else {
				missing = 0
			}
		}
		if reachedHello {
			delay = o.RetryMin
			setStatus(PhaseRestarting, "")
		} else {
			setStatus(PhaseWaiting, "")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		if !reachedHello {
			delay = min(delay*2, o.RetryMax)
		}
	}
}

// activityReader records when bytes last arrived, for the idle watchdog.
type activityReader struct {
	r    io.Reader
	last atomic.Int64
}

func (a *activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// runSession feeds one watcher session to a Receiver and reports whether the
// watcher got as far as a valid hello.
func runSession(ctx context.Context, o *DaemonOptions, st *State, root *os.Root, sess *Session, setStatus func(phase, detail string)) (reachedHello bool) {
	ar := &activityReader{r: sess.Stdout}
	ar.last.Store(time.Now().UnixNano())

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(max(o.WatchdogTimeout/4, 10*time.Millisecond))
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				sess.Stop()
				return
			case <-tick.C:
				if time.Since(time.Unix(0, ar.last.Load())) > o.WatchdogTimeout {
					o.Logf("watcher silent for %s; restarting it", o.WatchdogTimeout)
					sess.Stop()
					return
				}
			}
		}
	}()
	if sess.Stderr != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copyLines(sess.Stderr, func(line string) { o.Logf("watcher: %q", line) })
		}()
	}

	rc := &Receiver{
		Root:       root,
		NoSkiplist: st.NoSkiplist,
		Logf:       o.Logf,
		OnPhase: func(phase string) {
			reachedHello = true
			setStatus(phase, "")
		},
	}
	err := rc.Run(ar)
	close(done)
	sess.Stop()
	wg.Wait()
	if err != nil && ctx.Err() == nil {
		o.Logf("watcher session ended: %v", err)
	} else {
		o.Logf("watcher session ended")
	}
	return reachedHello
}

// maxLogLine caps a single log line; the log is fed by the container.
const maxLogLine = 4 << 10

// copyLines calls emit for each line of r, truncating lines longer than
// maxLogLine instead of buffering them (the source is untrusted).
func copyLines(r io.Reader, emit func(string)) {
	br := bufio.NewReaderSize(r, maxLogLine)
	for {
		line, isPrefix, err := br.ReadLine()
		if len(line) > 0 || err == nil {
			emit(string(line))
		}
		for isPrefix && err == nil {
			_, isPrefix, err = br.ReadLine()
		}
		if err != nil {
			return
		}
	}
}

// FileLogger appends timestamped lines to a log file that the daemon owns.
// The content is container-influenced, so it is bounded: lines are capped at
// maxLogLine and the file is truncated once it exceeds maxLogSize.
type FileLogger struct {
	mu   sync.Mutex
	path string
	f    *os.File
	n    int
}

const maxLogSize = 10 << 20

// OpenFileLogger opens (creating if needed) the log at path.
func OpenFileLogger(path string) (*FileLogger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l := &FileLogger{path: path, f: f}
	l.rotateIfLarge()
	return l, nil
}

func (l *FileLogger) rotateIfLarge() {
	if fi, err := l.f.Stat(); err == nil && fi.Size() > maxLogSize {
		_ = l.f.Truncate(0)
	}
}

// Logf writes one line.
func (l *FileLogger) Logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	line = strings.ReplaceAll(line, "\n", " ")
	if len(line) > maxLogLine {
		line = line[:maxLogLine] + "…"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.f, "%s %s\n", time.Now().Format("2006-01-02T15:04:05.000"), line)
	if l.n++; l.n%256 == 0 {
		l.rotateIfLarge()
	}
}

// Close closes the log file.
func (l *FileLogger) Close() error { return l.f.Close() }
