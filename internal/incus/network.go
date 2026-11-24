package incus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// runIncusCapture runs `incus <args...>` through the execCommand seam,
// optionally feeding stdin, and returns stdout. A non-zero exit surfaces as an
// error carrying the exit code and the captured stderr, so callers see why
// incus refused instead of a bare "exit 1".
func runIncusCapture(stdin string, args ...string) ([]byte, error) {
	cmd := execCommand(context.Background(), "incus", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		msg := strings.TrimSpace(string(ee.Stderr))
		if msg != "" {
			return out, fmt.Errorf("incus %s: exit %d: %s", strings.Join(args, " "), ee.ExitCode(), msg)
		}
		return out, fmt.Errorf("incus %s: exit %d", strings.Join(args, " "), ee.ExitCode())
	}
	return out, fmt.Errorf("incus %s: %w", strings.Join(args, " "), err)
}

// incusGet runs a `get` style command and returns the trimmed value; an unset
// key yields "" with no error (incus prints an empty line and exits 0).
func incusGet(args ...string) (string, error) {
	out, err := runIncusCapture("", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// incusSet runs a `set` style command. kv entries are "key=value" strings
// passed as separate argv entries; incus (>= the multi-key `set` syntax)
// applies them in one call.
func incusSet(prefix []string, kv []string) error {
	if len(kv) == 0 {
		return fmt.Errorf("incus %s: no key=value pairs given", strings.Join(prefix, " "))
	}
	_, err := runIncusCapture("", append(append([]string{}, prefix...), kv...)...)
	return err
}

// NetworkACLExists reports whether a network ACL with this exact name exists.
// Uses the structured `network acl list --format=json` rather than parsing a
// "not found" error string, so a genuine failure is never mistaken for absence.
func NetworkACLExists(name string) (bool, error) {
	out, err := runIncusCapture("", "network", "acl", "list", "--format=json")
	if err != nil {
		return false, err
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return false, fmt.Errorf("parse incus network acl list: %w", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// NetworkACLCreate creates an empty network ACL.
func NetworkACLCreate(name string) error {
	_, err := runIncusCapture("", "network", "acl", "create", name)
	return err
}

// NetworkACLReplace replaces the ACL's full definition with the given YAML,
// fed on stdin to `incus network acl edit` (which reads stdin instead of
// opening $EDITOR when stdin is not a terminal).
func NetworkACLReplace(name, yaml string) error {
	_, err := runIncusCapture(yaml, "network", "acl", "edit", name)
	return err
}

// NetworkACLGet reads one ACL config key; an unset key returns "".
func NetworkACLGet(name, key string) (string, error) {
	return incusGet("network", "acl", "get", name, key)
}

// NetworkACLSet sets ACL config keys given as "key=value" strings.
func NetworkACLSet(name string, kv ...string) error {
	return incusSet([]string{"network", "acl", "set", name}, kv)
}

// NetworkGet reads one network config key; an unset key returns "".
func NetworkGet(network, key string) (string, error) {
	return incusGet("network", "get", network, key)
}

// NetworkSet sets network config keys given as "key=value" strings.
func NetworkSet(network string, kv ...string) error {
	return incusSet([]string{"network", "set", network}, kv)
}

// ProfileDeviceGet reads one device key on a profile; an unset key returns "".
func ProfileDeviceGet(profile, device, key string) (string, error) {
	return incusGet("profile", "device", "get", profile, device, key)
}

// ProfileDeviceSet sets device keys on a profile given as "key=value" strings.
func ProfileDeviceSet(profile, device string, kv ...string) error {
	return incusSet([]string{"profile", "device", "set", profile, device}, kv)
}
