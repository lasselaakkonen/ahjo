// Package incus wraps the host `incus` binary for the bits ahjo needs:
// proxy device management and container existence queries.
package incus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ContainerExists returns true if a container with this exact name is registered.
func ContainerExists(name string) (bool, error) {
	cmd := exec.Command("incus", "list", "--format=json", name)
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("incus list: %w", err)
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return false, fmt.Errorf("parse incus list: %w", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// AddProxyDevice adds a proxy device, tolerating "already exists" errors.
func AddProxyDevice(container, device, listen, connect string) error {
	args := []string{
		"config", "device", "add", container, device, "proxy",
		"listen=" + listen,
		"connect=" + connect,
	}
	cmd := exec.Command("incus", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		os.Stdout.Write(out)
		return nil
	}
	if strings.Contains(strings.ToLower(string(out)), "already exists") {
		return nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus %s: exit %d", strings.Join(args, " "), ee.ExitCode())
	}
	return fmt.Errorf("incus %s: %w", strings.Join(args, " "), err)
}

// ImageAliasExists returns true if alias resolves to an Incus image.
func ImageAliasExists(alias string) (bool, error) {
	cmd := exec.Command("incus", "image", "alias", "list", "--format=json")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("incus image alias list: %w", err)
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return false, fmt.Errorf("parse alias list: %w", err)
	}
	for _, r := range rows {
		if r.Name == alias {
			return true, nil
		}
	}
	return false, nil
}

// DeleteImageAlias deletes the image referenced by alias. Returns nil when the
// alias didn't exist (so callers can use it as a "force-clean before rebuild"
// step without first checking).
func DeleteImageAlias(alias string) error {
	cmd := exec.Command("incus", "image", "delete", alias)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	low := strings.ToLower(string(out))
	if strings.Contains(low, "not found") || strings.Contains(low, "no such") {
		return nil
	}
	return fmt.Errorf("incus image delete %s: %w: %s", alias, err, strings.TrimSpace(string(out)))
}

// StoragePoolDriver returns the driver of the default storage pool, e.g. "btrfs".
func StoragePoolDriver() (string, error) {
	cmd := exec.Command("incus", "storage", "list", "--format=json")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("incus storage list: %w", err)
	}
	var rows []struct {
		Name   string `json:"name"`
		Driver string `json:"driver"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return "", fmt.Errorf("parse storage list: %w", err)
	}
	for _, r := range rows {
		if r.Name == "default" {
			return r.Driver, nil
		}
	}
	if len(rows) > 0 {
		return rows[0].Driver, nil
	}
	return "", nil
}
