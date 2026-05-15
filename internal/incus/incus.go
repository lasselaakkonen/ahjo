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

// ContainersWithPrefix returns names of containers that equal prefix or start
// with prefix+"-". The "-" boundary keeps unrelated names that merely share a
// fragment (e.g. "ahjo-foobar" when prefix is "ahjo-foo") out of the result.
func ContainersWithPrefix(prefix string) ([]string, error) {
	cmd := exec.Command("incus", "list", "--format=json")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("incus list: %w", err)
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("parse incus list: %w", err)
	}
	var matches []string
	for _, r := range rows {
		if r.Name == prefix || strings.HasPrefix(r.Name, prefix+"-") {
			matches = append(matches, r.Name)
		}
	}
	sort.Strings(matches)
	return matches, nil
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
//
// HOME/USER/LOGNAME/SHELL come from the container's environment.* config
// keys (set on container creation by wireBranchContainer), the same way
// Docker dev containers get them from the image's ENV layer. No per-call
// env seeding here.
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
// for the journalctl-follow path where ahjo has nothing to do post-exit, so
// signal + exit-code passthrough comes for free via execve.
//
// Callers that need to run code *after* the user exits (e.g. printing a
// post-exit status block) should use ExecAttachWait instead — it spawns
// `incus exec` as a child so control returns to Go on exit.
//
// User-session env (HOME/USER/LOGNAME/SHELL) lives on the container's
// environment.* config — see ExecAs's note.
func ExecAttach(name string, uid int, env map[string]string, cwd string, argv ...string) error {
	bin, err := exec.LookPath("incus")
	if err != nil {
		return fmt.Errorf("incus not on PATH: %w", err)
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

// ExecAttachWait spawns `incus exec --force-interactive` as a child process
// with the same argv layout as ExecAttach, but waits for it to exit and
// returns its exit code so the caller can run post-exit logic before
// terminating. Stdin/stdout/stderr are wired straight through so the
// inner shell keeps a normal TTY.
//
// Ctrl-C and other terminal signals are delivered to the foreground process
// group (which `incus exec` joins), so Go-side signal handling is
// unnecessary — the child receives them, and we just observe its exit.
func ExecAttachWait(name string, uid int, env map[string]string, cwd string, argv ...string) (int, error) {
	cliArgs := []string{"exec", name, "--force-interactive", "--user", strconv.Itoa(uid)}
	if cwd != "" {
		cliArgs = append(cliArgs, "--cwd", cwd)
	}
	cliArgs = append(cliArgs, envArgs(env)...)
	cliArgs = append(cliArgs, "--")
	cliArgs = append(cliArgs, argv...)
	cmd := exec.Command("incus", cliArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return 0, fmt.Errorf("incus exec %s: %w", name, err)
	}
	return 0, nil
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

// HasDevice reports whether `device` is configured on `container`. Used by
// `ahjo mirror` to detect single-active-mirror state across the registry, by
// the TUI to show which branch is currently mirroring, and by destroy paths
// to detect a mirror that needs `mirror off` first.
func HasDevice(container, device string) (bool, error) {
	cmd := exec.Command("incus", "config", "device", "list", container)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			low := strings.ToLower(string(ee.Stderr))
			if strings.Contains(low, "not found") {
				return false, nil
			}
			return false, fmt.Errorf("incus config device list %s: exit %d", container, ee.ExitCode())
		}
		return false, fmt.Errorf("incus config device list %s: %w", container, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == device {
			return true, nil
		}
	}
	return false, nil
}

// ContainerStatus returns the lifecycle status string from `incus list`
// (e.g. "Running", "Stopped"). Returns ("", nil) for unknown / not-listed.
func ContainerStatus(name string) (string, error) {
	cmd := exec.Command("incus", "list", "--format=json", name)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("incus list %s: %w", name, err)
	}
	var rows []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return "", fmt.Errorf("parse incus list: %w", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.Status, nil
		}
	}
	return "", nil
}

// SystemctlDaemonReload runs `systemctl daemon-reload` inside container.
// Idempotent and cheap.
func SystemctlDaemonReload(container string) error {
	cmd := exec.Command("incus", "exec", container, "--", "systemctl", "daemon-reload")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	os.Stderr.Write(out)
	return fmt.Errorf("incus exec %s -- systemctl daemon-reload: %w", container, err)
}

// SystemctlEnableNow runs `systemctl enable --now <unit>` inside container.
func SystemctlEnableNow(container, unit string) error {
	cmd := exec.Command("incus", "exec", container, "--", "systemctl", "enable", "--now", unit)
	out, err := cmd.CombinedOutput()
	if err == nil {
		os.Stdout.Write(out)
		return nil
	}
	os.Stderr.Write(out)
	return fmt.Errorf("incus exec %s -- systemctl enable --now %s: %w", container, unit, err)
}

// SystemctlDisableNow runs `systemctl disable --now <unit>` inside container.
// Tolerates "not loaded" (unit was never installed in this container).
func SystemctlDisableNow(container, unit string) error {
	cmd := exec.Command("incus", "exec", container, "--", "systemctl", "disable", "--now", unit)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	low := strings.ToLower(string(out))
	if strings.Contains(low, "not loaded") || strings.Contains(low, "no such file") || strings.Contains(low, "does not exist") {
		return nil
	}
	os.Stderr.Write(out)
	return fmt.Errorf("incus exec %s -- systemctl disable --now %s: %w", container, unit, err)
}

// SystemctlStop runs `systemctl stop <unit>` inside container. Tolerates
// "not loaded" (idempotent stop).
func SystemctlStop(container, unit string) error {
	cmd := exec.Command("incus", "exec", container, "--", "systemctl", "stop", unit)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	low := strings.ToLower(string(out))
	if strings.Contains(low, "not loaded") || strings.Contains(low, "no such file") || strings.Contains(low, "does not exist") {
		return nil
	}
	os.Stderr.Write(out)
	return fmt.Errorf("incus exec %s -- systemctl stop %s: %w", container, unit, err)
}

// SystemctlIsActive returns whether `unit` is currently active inside
// container. systemctl's documented exits: 0 = active, 3 = inactive, 4 =
// no-such-unit, others = error. We treat 3 and 4 as benign "not active."
func SystemctlIsActive(container, unit string) (bool, error) {
	cmd := exec.Command("incus", "exec", container, "--", "systemctl", "is-active", "--quiet", unit)
	if err := cmd.Run(); err == nil {
		return true, nil
	} else {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code := ee.ExitCode()
			if code == 3 || code == 4 {
				return false, nil
			}
			return false, fmt.Errorf("systemctl is-active %s in %s: exit %d", unit, container, code)
		}
		return false, fmt.Errorf("systemctl is-active %s in %s: %w", unit, container, err)
	}
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
