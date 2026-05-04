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

// Exec runs a one-shot command in the container via `incus exec` and returns
// its captured stdout. Stderr is forwarded so any error context surfaces to
// the user without the caller having to plumb it through.
func Exec(container string, argv ...string) ([]byte, error) {
	args := append([]string{"exec", container, "--"}, argv...)
	cmd := exec.Command("incus", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out, fmt.Errorf("incus exec %s: exit %d", container, ee.ExitCode())
		}
		return out, fmt.Errorf("incus exec %s: %w", container, err)
	}
	return out, nil
}

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

// CopyContainer clones src into dst as a stateless (non-snapshot) copy.
func CopyContainer(src, dst string) error {
	cmd := exec.Command("incus", "copy", "--stateless", src, dst)
	out, err := cmd.CombinedOutput()
	if err == nil {
		os.Stdout.Write(out)
		return nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus copy %s %s: exit %d", src, dst, ee.ExitCode())
	}
	return fmt.Errorf("incus copy %s %s: %w", src, dst, err)
}

// ContainerDeleteForce deletes a container forcefully. Tolerant of "not found".
func ContainerDeleteForce(name string) error {
	cmd := exec.Command("incus", "delete", "--force", name)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	low := strings.ToLower(string(out))
	if strings.Contains(low, "not found") || strings.Contains(low, "no such") {
		return nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus delete -f %s: exit %d", name, ee.ExitCode())
	}
	return fmt.Errorf("incus delete -f %s: %w", name, err)
}

// ConfigDeviceSet updates a single key on a named device in a container.
func ConfigDeviceSet(container, device, key, value string) error {
	arg := key + "=" + value
	cmd := exec.Command("incus", "config", "device", "set", container, device, arg)
	out, err := cmd.CombinedOutput()
	if err == nil {
		os.Stdout.Write(out)
		return nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus config device set %s %s %s: exit %d", container, device, arg, ee.ExitCode())
	}
	return fmt.Errorf("incus config device set %s %s %s: %w", container, device, arg, err)
}

// ProxyDevice is one row of a container's proxy-device list. Listen and
// Connect carry the raw `proto:addr:port` strings as Incus stores them.
type ProxyDevice struct {
	Name    string
	Listen  string
	Connect string
}

// ListProxyDevices parses `incus config device show <container>` and returns
// every device with `type: proxy`. Auto-expose uses this to diff against the
// current set of listening ports inside the container.
func ListProxyDevices(container string) ([]ProxyDevice, error) {
	cmd := exec.Command("incus", "config", "device", "show", container)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("incus config device show %s: %w", container, err)
	}
	type devInfo struct {
		name  string
		props map[string]string
	}
	var devs []devInfo
	var cur *devInfo
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) > 0 && line[0] != ' ' && strings.HasSuffix(line, ":") {
			devs = append(devs, devInfo{name: strings.TrimSuffix(line, ":"), props: map[string]string{}})
			cur = &devs[len(devs)-1]
			continue
		}
		if cur == nil {
			continue
		}
		if kv := strings.SplitN(strings.TrimSpace(line), ": ", 2); len(kv) == 2 {
			cur.props[kv[0]] = kv[1]
		}
	}
	var proxies []ProxyDevice
	for _, d := range devs {
		if d.props["type"] != "proxy" {
			continue
		}
		proxies = append(proxies, ProxyDevice{
			Name:    d.name,
			Listen:  d.props["listen"],
			Connect: d.props["connect"],
		})
	}
	return proxies, nil
}

// FindMountDevice scans `incus config device show <container>` and returns the
// name of the first disk device whose source path equals sourcePath. This is
// used to find the worktree bind-mount that COI created so its source can be
// updated after an `incus copy`.
func FindMountDevice(container, sourcePath string) (string, error) {
	cmd := exec.Command("incus", "config", "device", "show", container)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("incus config device show %s: %w", container, err)
	}
	needle := "source: " + sourcePath
	var currentDevice string
	for _, line := range strings.Split(string(out), "\n") {
		// Device name: non-space-prefixed line ending with ":"
		if len(line) > 0 && line[0] != ' ' && strings.HasSuffix(line, ":") {
			currentDevice = strings.TrimSuffix(line, ":")
		}
		if strings.TrimSpace(line) == needle && currentDevice != "" {
			return currentDevice, nil
		}
	}
	return "", fmt.Errorf("no disk device with source %q in container %q", sourcePath, container)
}

// RemoveDevice removes a named device from a container. Tolerant of "not found".
func RemoveDevice(container, device string) error {
	cmd := exec.Command("incus", "config", "device", "remove", container, device)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	low := strings.ToLower(string(out))
	if strings.Contains(low, "not found") || strings.Contains(low, "no such") {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus config device remove %s %s: exit %d", container, device, ee.ExitCode())
	}
	return fmt.Errorf("incus config device remove %s %s: %w", container, device, err)
}

// UpdateWorktreeMounts rebases all disk device source paths in a COW-copied
// container from oldBase to newBase. Devices whose remapped source does not
// exist on the host are removed — COI may have created protect-* bind-mounts
// for directories (e.g. .husky, .vscode) that are absent in the new branch.
func UpdateWorktreeMounts(container, oldBase, newBase string) error {
	cmd := exec.Command("incus", "config", "device", "show", container)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("incus config device show %s: %w", container, err)
	}

	type devInfo struct {
		name  string
		props map[string]string
	}
	var devs []devInfo
	var cur *devInfo
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) > 0 && line[0] != ' ' && strings.HasSuffix(line, ":") {
			devs = append(devs, devInfo{name: strings.TrimSuffix(line, ":"), props: map[string]string{}})
			cur = &devs[len(devs)-1]
			continue
		}
		if cur == nil {
			continue
		}
		if kv := strings.SplitN(strings.TrimSpace(line), ": ", 2); len(kv) == 2 {
			cur.props[kv[0]] = kv[1]
		}
	}

	for _, d := range devs {
		if d.props["type"] != "disk" {
			continue
		}
		src := d.props["source"]
		if !strings.HasPrefix(src, oldBase) {
			continue
		}
		newSrc := newBase + src[len(oldBase):]
		if _, serr := os.Stat(newSrc); serr != nil {
			// Source path absent in new worktree — drop the device.
			rmCmd := exec.Command("incus", "config", "device", "remove", container, d.name)
			if rmOut, rmErr := rmCmd.CombinedOutput(); rmErr != nil {
				return fmt.Errorf("incus config device remove %s %s: %w: %s", container, d.name, rmErr, rmOut)
			}
			continue
		}
		setCmd := exec.Command("incus", "config", "device", "set", container, d.name, "source="+newSrc)
		if setOut, setErr := setCmd.CombinedOutput(); setErr != nil {
			return fmt.Errorf("incus config device set %s %s source=%s: %w: %s", container, d.name, newSrc, setErr, setOut)
		}
	}
	return nil
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
