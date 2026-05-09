// Package incus wraps the host `incus` binary for the bits ahjo needs:
// proxy device management and container existence queries.
package incus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
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

// AddDiskDevice adds a disk (bind-mount) device, tolerating "already exists" errors.
func AddDiskDevice(container, device, source, path string, readonly bool) error {
	args := []string{
		"config", "device", "add", container, device, "disk",
		"source=" + source,
		"path=" + path,
	}
	if readonly {
		args = append(args, "readonly=true")
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

// ConfigGet reads a single container-level config key via `incus config get`.
// Returns the trimmed value; an unset key surfaces as the empty string.
func ConfigGet(container, key string) (string, error) {
	cmd := exec.Command("incus", "config", "get", container, key)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Stderr.Write(ee.Stderr)
			return "", fmt.Errorf("incus config get %s %s: exit %d", container, key, ee.ExitCode())
		}
		return "", fmt.Errorf("incus config get %s %s: %w", container, key, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ConfigSet writes a single container-level config key via `incus config
// set`. Uses the `<key>=<value>` argv form — the older `<key> <value>`
// form is deprecated and prints a warning per call on recent incus.
func ConfigSet(container, key, value string) error {
	cmd := exec.Command("incus", "config", "set", container, key+"="+value)
	out, err := cmd.CombinedOutput()
	if err == nil {
		os.Stdout.Write(out)
		return nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus config set %s %s=%s: exit %d", container, key, value, ee.ExitCode())
	}
	return fmt.Errorf("incus config set %s %s=%s: %w", container, key, value, err)
}

// Stop stops a container. Tolerant of "not running" / "already stopped" /
// "instance not found" so callers can use it as a "make sure it's stopped"
// step without first checking.
func Stop(container string) error {
	cmd := exec.Command("incus", "stop", container)
	out, err := cmd.CombinedOutput()
	if err == nil {
		os.Stdout.Write(out)
		return nil
	}
	low := strings.ToLower(string(out))
	if strings.Contains(low, "not running") ||
		strings.Contains(low, "already stopped") ||
		strings.Contains(low, "is stopped") ||
		strings.Contains(low, "not found") ||
		strings.Contains(low, "no such") {
		return nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus stop %s: exit %d", container, ee.ExitCode())
	}
	return fmt.Errorf("incus stop %s: %w", container, err)
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
	cmd.Stderr = os.Stderr
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

// EnsureSSHAgentProxy adds an `ssh-agent` proxy device on container,
// forwarding hostSocket inside as /tmp/ssh-agent.sock owned by uid/gid
// 1000. Verifies the listen socket actually materializes; if it doesn't
// (incus's bind=container proxy occasionally races with the container's
// init bringup), removes and re-adds up to a few times.
//
// On Lima the host SSH_AUTH_SOCK path changes per session, so callers
// re-run this every shell/claude/repo-add to keep the device pointing at
// the live socket. Always strips the existing device first so a stale
// connect path (or a previous attempt's dead forkproxy) can't shadow the
// new one.
//
// security.uid / security.gid are deliberately NOT set — on this incus
// version they silently prevent the proxy from creating the listen
// socket. uid/gid/mode are sufficient.
func EnsureSSHAgentProxy(container, hostSocket string) error {
	for attempt := 0; attempt < 3; attempt++ {
		_ = RemoveDevice(container, "ssh-agent")
		args := []string{
			"config", "device", "add", container, "ssh-agent", "proxy",
			"listen=unix:/tmp/ssh-agent.sock",
			"connect=unix:" + hostSocket,
			"bind=container",
			"uid=1000", "gid=1000", "mode=0600",
		}
		cmd := exec.Command("incus", args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			os.Stderr.Write(out)
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return fmt.Errorf("incus config device add ssh-agent: exit %d", ee.ExitCode())
			}
			return fmt.Errorf("incus config device add ssh-agent: %w", err)
		}
		// Verify the listen socket actually appeared inside the container.
		// Poll up to ~2s — incus reports success before forkproxy has
		// finished namespace setup, and on a freshly-started container
		// the first attempt commonly races and silently no-ops.
		for i := 0; i < 20; i++ {
			if probeErr := exec.Command("incus", "exec", container, "--", "test", "-S", "/tmp/ssh-agent.sock").Run(); probeErr == nil {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		// Wait progressively longer between retries so the container's
		// init has more time to settle on each go.
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return fmt.Errorf("ssh-agent proxy added but listen socket /tmp/ssh-agent.sock never appeared in %s", container)
}

// LaunchStopped runs `incus init <image> <name>`. The instance exists after
// this returns but is not started, letting the caller apply per-container
// config (raw.idmap, devices) before first boot.
func LaunchStopped(image, name string) error {
	cmd := exec.Command("incus", "init", image, name)
	out, err := cmd.CombinedOutput()
	if err == nil {
		os.Stdout.Write(out)
		return nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus init %s %s: exit %d", image, name, ee.ExitCode())
	}
	return fmt.Errorf("incus init %s %s: %w", image, name, err)
}

// Launch runs `incus launch <image> <name>` (init + start in one). Used by
// the image-build pipeline because the transient build container needs
// systemd up so apt + systemctl in the Feature install.sh work.
func Launch(image, name string) error {
	cmd := exec.Command("incus", "launch", image, name)
	out, err := cmd.CombinedOutput()
	if err == nil {
		os.Stdout.Write(out)
		return nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus launch %s %s: exit %d", image, name, ee.ExitCode())
	}
	return fmt.Errorf("incus launch %s %s: %w", image, name, err)
}

// ImageCopyRemote pulls remote (e.g. "images:ubuntu/24.04") into the local
// image store and tags it with alias. Tolerant of "already exists" so
// callers can use it as a "ensure" step without first checking.
func ImageCopyRemote(remote, alias string) error {
	cmd := exec.Command("incus", "image", "copy", remote, "local:", "--alias", alias)
	out, err := cmd.CombinedOutput()
	if err == nil {
		os.Stdout.Write(out)
		return nil
	}
	low := strings.ToLower(string(out))
	if strings.Contains(low, "already exists") {
		return nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus image copy %s -> %s: exit %d", remote, alias, ee.ExitCode())
	}
	return fmt.Errorf("incus image copy %s -> %s: %w", remote, alias, err)
}

// FilePushRecursive copies a host directory into the container at
// containerPath via `incus file push --recursive`. The container path is
// passed as `<name>/<path>`, matching the form `incus file push` expects.
func FilePushRecursive(name, hostDir, containerPath string) error {
	cmd := exec.Command("incus", "file", "push", "--recursive", hostDir, name+containerPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus file push -r %s %s%s: exit %d", hostDir, name, containerPath, ee.ExitCode())
	}
	return fmt.Errorf("incus file push -r %s %s%s: %w", hostDir, name, containerPath, err)
}

// Publish runs `incus publish <name> --alias <alias>`. If alias already
// exists it is force-deleted first — `incus publish` itself errors on a
// pre-existing alias instead of replacing it. Used by the image-build
// pipeline to write the freshly-baked ahjo-base over the previous one.
func Publish(name, alias string) error {
	if err := DeleteImageAlias(alias); err != nil {
		return fmt.Errorf("clear alias %s before publish: %w", alias, err)
	}
	cmd := exec.Command("incus", "publish", name, "--alias", alias)
	out, err := cmd.CombinedOutput()
	if err == nil {
		os.Stdout.Write(out)
		return nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus publish %s --alias %s: exit %d", name, alias, ee.ExitCode())
	}
	return fmt.Errorf("incus publish %s --alias %s: %w", name, alias, err)
}

// Start runs `incus start <name>`. Tolerant of "already running".
func Start(name string) error {
	cmd := exec.Command("incus", "start", name)
	out, err := cmd.CombinedOutput()
	if err == nil {
		os.Stdout.Write(out)
		return nil
	}
	low := strings.ToLower(string(out))
	if strings.Contains(low, "already running") || strings.Contains(low, "is running") {
		return nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus start %s: exit %d", name, ee.ExitCode())
	}
	return fmt.Errorf("incus start %s: %w", name, err)
}

// WaitReady polls `incus exec <name> -- echo ready` until it succeeds or
// timeout elapses. Used after Start to wait out PID 1 / network bring-up
// before issuing the first real exec.
func WaitReady(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		cmd := exec.Command("incus", "exec", name, "--", "echo", "ready")
		if err := cmd.Run(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("container %s not ready after %s: %w", name, timeout, lastErr)
	}
	return fmt.Errorf("container %s not ready after %s", name, timeout)
}

// envArgs renders an env map into stable `--env KEY=VAL` argv pairs (sorted
// for reproducibility). Keys with empty values fall through unchanged so the
// caller can decide whether to drop them.
func envArgs(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		args = append(args, "--env", k+"="+env[k])
	}
	return args
}

// ExecAs runs argv inside name as the given uid, with optional env + cwd,
// inheriting the caller's stdio. Non-interactive: stdin is the parent's,
// no force-interactive flag. Use this for one-shot setup commands where
// the child is expected to exit on its own.
func ExecAs(name string, uid int, env map[string]string, cwd string, argv ...string) error {
	args := []string{"exec", name, "--user", strconv.Itoa(uid)}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	args = append(args, envArgs(env)...)
	args = append(args, "--")
	args = append(args, argv...)
	cmd := exec.Command("incus", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("incus exec %s (uid %d) %s: exit %d", name, uid, strings.Join(argv, " "), ee.ExitCode())
		}
		return fmt.Errorf("incus exec %s (uid %d) %s: %w", name, uid, strings.Join(argv, " "), err)
	}
	return nil
}

// ExecAttach replaces the current process with `incus exec --force-interactive`
// against name, running argv as the given uid in cwd with optional env. Used
// for `ahjo shell` / `ahjo claude` so signals + exit code passthrough are
// automatic via execve.
//
// For uid 1000 (the canonical `ubuntu` user) we seed HOME/USER/LOGNAME/SHELL
// because `incus exec` doesn't read /etc/passwd the way sshd+PAM does — without
// these, `bash -l` runs with HOME="" and skips ~/.profile, so the user's
// prompt and rc-driven setup never load. Caller env wins on collision.
func ExecAttach(name string, uid int, env map[string]string, cwd string, argv ...string) error {
	bin, err := exec.LookPath("incus")
	if err != nil {
		return fmt.Errorf("incus not on PATH: %w", err)
	}
	if uid == 1000 {
		merged := map[string]string{
			"HOME":    "/home/ubuntu",
			"USER":    "ubuntu",
			"LOGNAME": "ubuntu",
			"SHELL":   "/bin/bash",
		}
		for k, v := range env {
			merged[k] = v
		}
		env = merged
	}
	cliArgs := []string{"incus", "exec", name, "--force-interactive", "--user", strconv.Itoa(uid)}
	if cwd != "" {
		cliArgs = append(cliArgs, "--cwd", cwd)
	}
	cliArgs = append(cliArgs, envArgs(env)...)
	cliArgs = append(cliArgs, "--")
	cliArgs = append(cliArgs, argv...)
	return syscall.Exec(bin, cliArgs, os.Environ())
}

// FilePush copies hostPath into the container at containerPath via
// `incus file push`. Tolerant of a missing host file: returns (false, nil)
// so callers can skip optional config files (e.g. a host's ~/.claude.json
// that doesn't exist yet).
func FilePush(name, hostPath, containerPath string) (pushed bool, err error) {
	if _, statErr := os.Stat(hostPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return false, statErr
	}
	cmd := exec.Command("incus", "file", "push", hostPath, name+containerPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, fmt.Errorf("incus file push %s %s%s: exit %d", hostPath, name, containerPath, ee.ExitCode())
	}
	return false, fmt.Errorf("incus file push %s %s%s: %w", hostPath, name, containerPath, err)
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
