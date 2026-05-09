// Package mirror implements `ahjo mirror`. Phase 1 (no-more-worktrees) ships
// with mirror temporarily disabled: the old VM-resident worktree path is
// gone, and the Phase 2 storage-pool-internal-path replacement is not yet
// wired. The State struct here carries the container name so Phase 2 can
// resume from existing per-repo MacMirrorTarget config without churn.
package mirror

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

const (
	stateFile = "mirror.json"
	logFile   = "mirror.log"
)

// State is the on-disk record of the currently active mirror, persisted at
// ~/.ahjo/mirror.json. There is at most one active mirror at a time.
type State struct {
	Alias     string    `json:"alias"`
	Slug      string    `json:"slug"`
	Container string    `json:"container"`
	Target    string    `json:"target"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

func StatePath() string { return filepath.Join(paths.AhjoDir(), stateFile) }
func LogPath() string   { return filepath.Join(paths.AhjoDir(), logFile) }

// Load reads the state file. Returns (nil, nil) when no mirror is active.
func Load() (*State, error) {
	b, err := os.ReadFile(StatePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read mirror state: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse mirror state: %w", err)
	}
	return &s, nil
}

// Save writes the state file atomically (tempfile + rename).
func (s *State) Save() error {
	if err := paths.EnsureSkeleton(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(paths.AhjoDir(), "mirror-*.json.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), StatePath())
}

// Clear removes the state file. No-op when missing.
func Clear() error {
	if err := os.Remove(StatePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// PIDAlive reports whether pid corresponds to a running process. Uses kill(pid, 0)
// per POSIX: signal 0 performs error-checking but does not deliver a signal.
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
