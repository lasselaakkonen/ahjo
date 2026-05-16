//go:build darwin

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// stageContainerConfigPaths walks args looking for `--container-config`
// values that point at an existing Mac-side file. For each such value it
// copies the file into <SharedDir>/tmp/container-config-<rand>.json —
// which Lima reverse-mounts into the VM at the same path via virtiofs —
// and rewrites the argv to point at the staged copy.
//
// Why: the in-VM ahjo reads --container-config paths with os.ReadFile.
// /Users/<user> resolves on both sides through Lima's default
// reverse-mount, but anything outside that (e.g. `/tmp/foo.json` or
// `/Volumes/...`) doesn't exist in the VM. Staging into SharedDir is
// the simplest fix: small file, well-defined mount window, no in-VM
// code changes.
//
// Identifier values (`node`, `ci`, `bare`, …) are left alone — they
// resolve inside the VM against bundled stacks or repo-local
// .ahjo/<name>.json. Mac-side staging is purely for path values.
//
// Stale staged files (>1h old) are swept on each call. Single-host,
// single-user assumption: nothing else writes here, so a timestamp
// check is enough.
func stageContainerConfigPaths(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	staged := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--container-config":
			// Flag and value are separate argv slots: --container-config <value>.
			if i+1 >= len(args) {
				// Malformed — let cobra surface the error in-VM.
				out = append(out, a)
				continue
			}
			val := args[i+1]
			rewritten, didStage, err := stageOneContainerConfigValue(val)
			if err != nil {
				return nil, err
			}
			out = append(out, a, rewritten)
			i++
			if didStage {
				staged = true
			}
		case strings.HasPrefix(a, "--container-config="):
			val := strings.TrimPrefix(a, "--container-config=")
			rewritten, didStage, err := stageOneContainerConfigValue(val)
			if err != nil {
				return nil, err
			}
			out = append(out, "--container-config="+rewritten)
			if didStage {
				staged = true
			}
		default:
			out = append(out, a)
		}
	}
	if staged {
		sweepStaleContainerConfigStaging()
	}
	return out, nil
}

// stageOneContainerConfigValue stages one --container-config value when
// it's a path to an existing Mac file. Returns the rewritten value, a
// flag indicating whether staging happened, and any error.
//
// Returns (value, false, nil) verbatim for:
//   - identifier-looking values (no separator, no .json suffix, etc.)
//   - paths to files that don't exist on Mac (let the in-VM resolver
//     surface "not found" against its own filesystem — same error the
//     user would see on Linux bare-metal)
//   - the literal "bare"
func stageOneContainerConfigValue(val string) (string, bool, error) {
	if val == "" || val == "bare" {
		return val, false, nil
	}
	if !looksLikeHostPath(val) {
		return val, false, nil
	}
	abs, err := filepath.Abs(val)
	if err != nil {
		return val, false, fmt.Errorf("--container-config %q: %w", val, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		// File doesn't exist on Mac — defer to in-VM for the canonical error.
		// Don't suppress unrelated stat failures (permission errors etc.)
		// either: passing them through means the in-VM ahjo errors with the
		// same path, so the user sees one consistent message.
		return val, false, nil
	}
	if !info.Mode().IsRegular() {
		return val, false, fmt.Errorf("--container-config %q: not a regular file", val)
	}

	stagingDir := containerConfigStagingDir()
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return val, false, fmt.Errorf("create staging dir %s: %w", stagingDir, err)
	}

	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return val, false, fmt.Errorf("nonce: %w", err)
	}
	dstName := "container-config-" + hex.EncodeToString(nonce) + ".json"
	dst := filepath.Join(stagingDir, dstName)

	if err := copyFile(abs, dst); err != nil {
		return val, false, fmt.Errorf("stage %s → %s: %w", abs, dst, err)
	}

	fmt.Fprintf(os.Stderr, "ahjo: staged %s → %s for VM relay\n", abs, dst)
	return dst, true, nil
}

// looksLikeHostPath mirrors the in-VM isPathLike heuristic so the shim
// and the resolver agree on which inputs are paths vs identifiers.
// Kept duplicated rather than imported to avoid pulling internal/cli
// (Linux-only build) into the darwin binary.
func looksLikeHostPath(ref string) bool {
	switch {
	case strings.ContainsRune(ref, filepath.Separator):
		return true
	case strings.HasPrefix(ref, "./"), strings.HasPrefix(ref, "../"):
		return true
	case strings.HasPrefix(ref, "~/"):
		return true
	case filepath.IsAbs(ref):
		return true
	case strings.HasSuffix(ref, ".json"):
		return true
	}
	return false
}

// containerConfigStagingDir lives under SharedDir() so it's virtiofs-
// mounted into the Lima VM at the same path — both sides read/write the
// same physical bytes.
func containerConfigStagingDir() string {
	return filepath.Join(paths.SharedDir(), "tmp", "container-config")
}

// copyFile copies src to dst with the default umask. Caps at 1MiB
// because an ahjocontainer.json larger than that is almost certainly a
// mistake (the schema is intentionally small) and we'd rather error
// loudly than silently stage a 100MB file the user accidentally pointed
// at.
func copyFile(src, dst string) error {
	const maxBytes = 1 << 20 // 1 MiB
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if info.Size() > maxBytes {
		return fmt.Errorf("file too large (%d bytes; cap %d) — an ahjocontainer.json shouldn't be this big", info.Size(), maxBytes)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(in, maxBytes+1)); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// sweepStaleContainerConfigStaging removes container-config-*.json
// entries from the staging dir that are older than 1h. Idempotent;
// silently ignores missing/unreadable dir. Single-host, single-user
// assumption: nothing else writes to this dir, so age is a sufficient
// staleness signal.
func sweepStaleContainerConfigStaging() {
	dir := containerConfigStagingDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Hour)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasPrefix(e.Name(), "container-config-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
