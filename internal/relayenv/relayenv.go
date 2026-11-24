// Package relayenv hands secrets from the Mac shim to the in-VM ahjo without
// putting them on the `limactl shell` command line (argv of limactl and its
// ssh child is visible in `ps` for the whole session).
//
// The shim writes KEY=VALUE pairs to a fresh 0600 file under Dir() — which
// lives in ~/.ahjo-shared, the virtiofs directory both sides see at the same
// absolute path — and passes only the file's basename in EnvVar. The in-VM
// ahjo consumes the file first thing in main: it reads it, deletes it, and
// copies the allowlisted pairs into its own process env.
package relayenv

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// EnvVar carries the relay file's basename (never a path, never a value)
// from the shim to the in-VM ahjo.
const EnvVar = "AHJO_RELAY_ENV"

// staleAfter is how old an unconsumed relay file may get before Write sweeps
// it. A relay that died before the in-VM side consumed its file (limactl
// failed to start, an old in-VM binary that ignores EnvVar) leaves one
// behind; nothing legitimate takes this long between write and read.
const staleAfter = 5 * time.Minute

// allowed is the set of keys Consume returns. Anything else in a relay file
// is dropped, so the file cannot be used to set arbitrary env (PATH,
// LD_PRELOAD, …) in the in-VM process.
var allowed = map[string]bool{"GH_TOKEN": true}

var nameRE = regexp.MustCompile(`^r-[A-Za-z0-9]+$`)

// Dir is the directory relay files live in.
func Dir() string { return filepath.Join(paths.SharedDir(), ".relay-env") }

// Write stores pairs ("KEY=VALUE") in a new 0600 file under Dir() (dir 0700)
// and returns its basename. Rejects \n, \r, \x00. Also removes files in Dir()
// older than 5 minutes (left behind when a relay died before consuming).
// Errors never include a value.
func Write(pairs []string) (name string, err error) {
	for i, p := range pairs {
		k, _, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return "", fmt.Errorf("relay env: pair %d is not KEY=VALUE", i)
		}
		if strings.ContainsAny(p, "\n\r\x00") {
			return "", fmt.Errorf("relay env: %s contains a newline, carriage return or NUL", k)
		}
	}
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("relay env: %w", err)
	}
	sweepStale(dir, time.Now())

	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("relay env: random name: %w", err)
	}
	name = "r-" + hex.EncodeToString(b[:])
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("relay env: %w", err)
	}
	_, werr := f.WriteString(strings.Join(pairs, "\n") + "\n")
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("relay env: write %s: %w", name, werr)
	}
	return name, nil
}

// sweepStale removes relay files in dir last modified before now-staleAfter.
// Best-effort: errors are ignored, a leftover file is retried next Write.
func sweepStale(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !nameRE.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if now.Sub(info.ModTime()) > staleAfter {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Consume reads and deletes Dir()/name and returns the allowlisted pairs.
// name must match ^r-[A-Za-z0-9]+$; the target must be a regular file
// (Lstat, no symlink). The file is removed before parsing. Only keys in the
// allowlist (GH_TOKEN) are returned. If only the removal fails, the pairs
// are returned together with the error.
func Consume(name string) (map[string]string, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("relay env: invalid name %q", name)
	}
	path := filepath.Join(Dir(), name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("relay env: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("relay env: %s is not a regular file", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("relay env: %w", err)
	}
	rmErr := os.Remove(path)

	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || !allowed[k] {
			continue
		}
		out[k] = v
	}
	if rmErr != nil {
		return out, fmt.Errorf("relay env: remove %s: %w", name, rmErr)
	}
	return out, nil
}

// Remove deletes Dir()/name, ignoring not-exist. Used by the shim when the
// relay ran as a child and the file may not have been consumed.
func Remove(name string) {
	if !nameRE.MatchString(name) {
		return
	}
	if err := os.Remove(filepath.Join(Dir(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "warn: remove relay env %s: %v\n", name, err)
	}
}
