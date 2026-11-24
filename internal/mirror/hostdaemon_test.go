package mirror

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestStateRoundTrip(t *testing.T) {
	isolateHome(t)
	if st, err := LoadState(); err != nil || st != nil {
		t.Fatalf("empty LoadState = %v, %v", st, err)
	}
	want := &State{Session: NewSessionID(), Container: "c", Slug: "s", Alias: "a", Target: "/t", NoSkiplist: true}
	if err := SaveState(want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState()
	if err != nil || got == nil || got.Session != want.Session || got.Target != "/t" || !got.NoSkiplist {
		t.Fatalf("LoadState = %+v, %v", got, err)
	}
	if err := ClearState(); err != nil {
		t.Fatal(err)
	}
	if st, _ := LoadState(); st != nil {
		t.Fatal("state survived ClearState")
	}
}

func TestDaemonAliveTracksLock(t *testing.T) {
	isolateHome(t)
	if DaemonAlive() {
		t.Fatal("alive with no lock file")
	}
	f, err := acquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	if !DaemonAlive() {
		t.Error("not alive while the lock is held")
	}
	_ = f.Close()
	if DaemonAlive() {
		t.Error("alive after the lock was released")
	}
}

// fakeSession serves canned bytes, then either EOFs or blocks until stopped.
func fakeSession(data []byte, block bool) *Session {
	pr, pw := io.Pipe()
	var once sync.Once
	go func() {
		_, _ = pw.Write(data)
		if !block {
			_ = pw.Close()
		}
	}()
	return &Session{Stdout: pr, Stop: func() { once.Do(func() { pw.CloseWithError(io.ErrClosedPipe) }) }}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunDaemon_ServesRestartsAndExits(t *testing.T) {
	isolateHome(t)
	target := t.TempDir()
	st := &State{Session: NewSessionID(), Container: "c", Slug: "s", Target: target}
	if err := SaveState(st); err != nil {
		t.Fatal(err)
	}

	first := newStream(t).file("one.txt", "1", 0o644, true).raw(FrameBootstrapDone, nil)
	second := newStream(t).file("two.txt", "2", 0o644, true).raw(FrameBootstrapDone, nil)
	var spawns atomic.Int32
	var logMu sync.Mutex
	var log bytes.Buffer
	opts := DaemonOptions{
		Version:         "v-test",
		RetryMin:        10 * time.Millisecond,
		RetryMax:        20 * time.Millisecond,
		WatchdogTimeout: 300 * time.Millisecond,
		Logf: func(f string, a ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			log.WriteString(sprintfLine(f, a...))
		},
		Spawn: func(ctx context.Context, got *State, force bool) (*Session, error) {
			if got.Session != st.Session {
				t.Errorf("spawn got session %q", got.Session)
			}
			switch spawns.Add(1) {
			case 1:
				return fakeSession(first.buf.Bytes(), false), nil
			default:
				return fakeSession(second.buf.Bytes(), true), nil
			}
		},
	}
	done := make(chan error, 1)
	go func() { done <- RunDaemon(context.Background(), opts) }()

	waitFor(t, "second session live", func() bool {
		s := LoadStatus()
		return spawns.Load() >= 2 && s != nil && s.Phase == PhaseLive && lexists(filepath.Join(target, "two.txt"))
	})
	if !DaemonAlive() {
		t.Error("DaemonAlive false while RunDaemon is serving")
	}
	if s := LoadStatus(); s.Version != "v-test" || s.Session != st.Session {
		t.Errorf("status %+v", s)
	}
	if got := mustRead(t, filepath.Join(target, "one.txt")); got != "1" {
		t.Errorf("one.txt = %q", got)
	}

	// Tombstone: the daemon must exit at the next session boundary (here the
	// watchdog ends the blocked session) without spawning again.
	st.Stopping = true
	if err := SaveState(st); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunDaemon: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not exit after the state was tombstoned")
	}
	if DaemonAlive() {
		t.Error("lock still held after exit")
	}
	logMu.Lock()
	defer logMu.Unlock()
	if !strings.Contains(log.String(), "watcher silent") {
		t.Errorf("watchdog never fired; log:\n%s", log.String())
	}
}

func TestRunDaemon_RefusesTombstonedOrMissingState(t *testing.T) {
	isolateHome(t)
	spawn := func(context.Context, *State, bool) (*Session, error) {
		t.Error("spawned a watcher without a live state")
		return nil, io.EOF
	}
	if err := RunDaemon(context.Background(), DaemonOptions{Spawn: spawn}); err != nil {
		t.Fatalf("no state: %v", err)
	}
	if err := SaveState(&State{Session: "x", Target: t.TempDir(), Stopping: true}); err != nil {
		t.Fatal(err)
	}
	if err := RunDaemon(context.Background(), DaemonOptions{Spawn: spawn}); err != nil {
		t.Fatalf("tombstoned state: %v", err)
	}
}

func TestRunDaemon_ForcePushAfterSessionWithoutHello(t *testing.T) {
	isolateHome(t)
	st := &State{Session: NewSessionID(), Container: "c", Target: t.TempDir()}
	if err := SaveState(st); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var forces []bool
	var mu sync.Mutex
	opts := DaemonOptions{
		RetryMin: 5 * time.Millisecond, RetryMax: 5 * time.Millisecond,
		Spawn: func(context.Context, *State, bool) (*Session, error) { return nil, nil },
	}
	opts.Spawn = func(_ context.Context, _ *State, force bool) (*Session, error) {
		mu.Lock()
		defer mu.Unlock()
		forces = append(forces, force)
		if len(forces) >= 2 {
			cancel()
		}
		return fakeSession([]byte("garbage that is not a frame stream"), false), nil
	}
	if err := RunDaemon(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if len(forces) < 2 || forces[0] || !forces[1] {
		t.Errorf("forcePush sequence %v, want [false true …]", forces)
	}
}

func TestCopyLinesTruncatesLongLines(t *testing.T) {
	in := strings.Repeat("x", 3*maxLogLine) + "\nshort\n"
	var got []string
	copyLines(strings.NewReader(in), func(s string) { got = append(got, s) })
	if len(got) != 2 || len(got[0]) > maxLogLine || got[1] != "short" {
		t.Errorf("got %d lines, first len %d", len(got), len(got[0]))
	}
}

func TestFileLoggerCapsLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d.log")
	l, err := OpenFileLogger(p)
	if err != nil {
		t.Fatal(err)
	}
	l.Logf("%s", strings.Repeat("y", 2*maxLogLine)+"\ninjected")
	_ = l.Close()
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), "\n") != 1 || len(b) > maxLogLine+64 {
		t.Errorf("log not bounded to one capped line: %d bytes, %d newlines", len(b), strings.Count(string(b), "\n"))
	}
}
