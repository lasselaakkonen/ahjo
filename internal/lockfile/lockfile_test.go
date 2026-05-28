package lockfile

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// redirectAhjoHome points AhjoDir() (and therefore LockPath()) at a temp dir
// by overriding $HOME, which os.UserHomeDir resolves on Linux. t.Setenv
// restores the previous value at test end.
func redirectAhjoHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// TestAcquireReleaseReacquire is the core no-leak invariant: a released lock
// must be immediately re-acquirable. If release ever failed to LOCK_UN, the
// second Acquire would block for the full 30s timeout and wedge commands like
// `ahjo ls`.
func TestAcquireReleaseReacquire(t *testing.T) {
	redirectAhjoHome(t)

	release, err := Acquire()
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	release()

	done := make(chan struct{})
	go func() {
		r2, err := AcquireWithTimeout(2 * time.Second)
		if err != nil {
			t.Errorf("re-Acquire after release: %v", err)
		} else {
			r2()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("re-Acquire blocked after release — lock leaked")
	}
}

// TestAcquireIsExclusive asserts a second acquisition blocks while the first is
// held and surfaces a timeout error. flock is associated with the open file
// description, so two independent OpenFile descriptions contend even within the
// same process.
func TestAcquireIsExclusive(t *testing.T) {
	redirectAhjoHome(t)

	release, err := Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	start := time.Now()
	r2, err := AcquireWithTimeout(200 * time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		r2()
		t.Fatal("expected timeout while lock was held, got success")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q, want it to mention %q", err.Error(), "timed out")
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("returned after %s, expected to wait at least the 200ms timeout", elapsed)
	}
}

// TestReleaseUnblocksWaiter confirms a contended waiter proceeds once the
// holder releases — the lock is handed off, not merely time-sliced.
func TestReleaseUnblocksWaiter(t *testing.T) {
	redirectAhjoHome(t)

	release, err := Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		release()
	}()

	r2, err := AcquireWithTimeout(3 * time.Second)
	if err != nil {
		t.Fatalf("waiter never acquired after holder released: %v", err)
	}
	r2()
}

// TestLockFilePermissions pins the lockfile to 0o600 — it lives under ~/.ahjo
// alongside the token store, so a wider mode would be a secret-dir regression.
func TestLockFilePermissions(t *testing.T) {
	redirectAhjoHome(t)

	release, err := Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	st, err := os.Stat(paths.LockPath())
	if err != nil {
		t.Fatalf("stat lockfile: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("lockfile mode = %o, want 600", perm)
	}
}
