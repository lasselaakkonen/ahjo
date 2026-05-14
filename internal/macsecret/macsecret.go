//go:build darwin

// Package macsecret wraps `/usr/bin/security` so the Mac shim can store
// per-repo PATs in the user's login Keychain instead of as plaintext on the
// shared disk. The shim is the only writer; the in-VM ahjo receives values
// via GH_TOKEN injected on the limactl-shell command line, and never reads
// or writes Keychain directly (it can't — `security` doesn't exist in the
// Lima Linux VM).
//
// Storage shape, visible cleanly in Keychain Access.app:
//   - service: "ahjo." + key (e.g. "ahjo.GH_TOKEN")
//   - account: the repo slug ("owner/name")
//   - one row per (slug, key). `add-generic-password -U` upserts, so rotation
//     never duplicates rows.
package macsecret

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// errSecItemNotFound is the macOS Security.framework status code surfaced by
// `security find-generic-password` / `delete-generic-password` when the
// requested row doesn't exist. Treating it as a soft miss keeps Get nil-error
// for the "no Keychain entry yet" case and Delete idempotent.
const errSecItemNotFound = 44

func service(key string) string { return "ahjo." + key }

// Get looks up the value stored for (slug, key). Returns ("", false, nil) when
// no row exists; the boolean signals presence so callers can tell apart "no
// entry" from "entry with empty value." Errors are wrapped with the
// `security` stderr verbatim so a locked Keychain or a `security` failure
// surfaces unambiguously.
func Get(slug, key string) (string, bool, error) {
	cmd := exec.Command("security", "find-generic-password",
		"-s", service(key), "-a", slug, "-w")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == errSecItemNotFound {
			return "", false, nil
		}
		return "", false, wrap("find-generic-password", err)
	}
	return strings.TrimRight(string(out), "\r\n"), true, nil
}

// Set upserts (slug, key) -> val with `-U`. Note: the value crosses the
// command line, so it's briefly visible in `ps` to processes running as the
// same user. The only stricter alternatives are AppleScript or a tiny Swift
// helper linking against Security.framework — both disproportionate for a
// developer tool the user is interactively typing into.
func Set(slug, key, val string) error {
	cmd := exec.Command("security", "add-generic-password",
		"-U", "-s", service(key), "-a", slug, "-w", val)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("security add-generic-password: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Delete removes (slug, key). errSecItemNotFound is treated as success so the
// caller can sweep cleanup markers without first probing.
func Delete(slug, key string) error {
	cmd := exec.Command("security", "delete-generic-password",
		"-s", service(key), "-a", slug)
	if out, err := cmd.CombinedOutput(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == errSecItemNotFound {
			return nil
		}
		return fmt.Errorf("security delete-generic-password: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Probe reports whether (slug, key) exists without exposing the value. Used by
// `ahjo doctor` so the host check never reads PATs into a buffer it doesn't
// need.
func Probe(slug, key string) (bool, error) {
	cmd := exec.Command("security", "find-generic-password",
		"-s", service(key), "-a", slug)
	if out, err := cmd.CombinedOutput(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == errSecItemNotFound {
			return false, nil
		}
		return false, fmt.Errorf("security find-generic-password: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

func wrap(op string, err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("security %s: %w: %s", op, err, strings.TrimSpace(string(ee.Stderr)))
	}
	return fmt.Errorf("security %s: %w", op, err)
}
