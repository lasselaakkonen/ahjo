// Package coi wraps the host `coi` binary.
package coi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// ExecShell replaces the current process with `coi shell --container <name>`
// from worktreeDir. Stdio + signals + exit code passthrough is automatic via
// execve. Pinning the container by name (not by --slot) is required because
// `coi shell --slot N` auto-allocates a fresh slot whenever slot N already
// has a running container — which is always, for an existing ahjo worktree.
// `--container <name>` forces COI to reuse our prepared container instead.
func ExecShell(worktreeDir, containerName string) error {
	bin, err := exec.LookPath("coi")
	if err != nil {
		return fmt.Errorf("coi not on PATH: %w", err)
	}
	if err := os.Chdir(worktreeDir); err != nil {
		return fmt.Errorf("chdir %s: %w", worktreeDir, err)
	}
	return syscall.Exec(bin, []string{"coi", "shell", "--container", containerName}, os.Environ())
}

// Setup triggers COI's container creation + session setup (claude config push,
// sandbox injection, mounts) without launching the AI tool, by running
// `coi shell --debug --tmux=false --slot <slot>` and feeding it `exit\n` on
// stdin. The container persists after this returns. Use this on first
// `ahjo shell` so subsequent steps (e.g. ahjo-claude-prepare) can run inside
// the container before claude ever starts.
func Setup(worktreeDir string, slot int) error {
	bin, err := exec.LookPath("coi")
	if err != nil {
		return fmt.Errorf("coi not on PATH: %w", err)
	}
	cmd := exec.Command(bin, "shell", "--debug", "--tmux=false", "--slot", strconv.Itoa(slot))
	cmd.Dir = worktreeDir
	cmd.Stdin = strings.NewReader("exit\n")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Build runs `coi build --profile <name>` (optionally with --force) inheriting stdio.
func Build(profile string, force bool) error {
	args := []string{"build", "--profile", profile}
	if force {
		args = append(args, "--force")
	}
	cmd := exec.Command("coi", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ContainerStart runs `coi container start <name>`. Tolerant of "already running".
func ContainerStart(name string) error {
	return runTolerant([]string{"container", "start", name}, "already running", "is already")
}

// ContainerStop runs `coi container stop <name>`. Tolerant of "not running".
func ContainerStop(name string) error {
	return runTolerant([]string{"container", "stop", name}, "not running", "is stopped")
}

// ContainerDelete runs `coi container delete -f <name>`. Tolerant of "not found".
func ContainerDelete(name string) error {
	return runTolerant([]string{"container", "delete", "-f", name}, "not found", "no such")
}

// Shutdown runs `coi shutdown <name>`. Tolerant of "not found".
func Shutdown(name string) error {
	return runTolerant([]string{"shutdown", name}, "not found", "no such")
}

// ContainerExec runs a one-shot command in the named container as the given uid.
func ContainerExec(name string, asRoot bool, argv ...string) error {
	args := []string{"container", "exec", name}
	if asRoot {
		args = append(args, "--user", "0")
	}
	args = append(args, "--")
	args = append(args, argv...)
	cmd := exec.Command("coi", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ContainerExecAs runs a one-shot command in the named container as the given
// uid. Unlike ContainerExec(asRoot=false), this passes `--user <uid>` explicitly
// so we don't depend on COI's default-user behavior.
func ContainerExecAs(name string, uid int, argv ...string) error {
	args := []string{"container", "exec", name, "--user", strconv.Itoa(uid), "--"}
	args = append(args, argv...)
	cmd := exec.Command("coi", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ResolveContainer asks COI for the incus instance name backing the given
// workspace alias at the given slot. COI names containers `coi-<hash>-<slot>`
// where the hash is derived from the workspace path; the alias from
// `.coi/config.toml` is just a label. Returns ("", nil) when no matching
// container is registered (caller treats that as "not yet created").
func ResolveContainer(alias string, slot int) (string, error) {
	out, err := exec.Command("coi", "list", "--format", "json").Output()
	if err != nil {
		return "", fmt.Errorf("coi list: %w", err)
	}
	var payload struct {
		ActiveContainers []struct {
			Alias string `json:"alias"`
			Name  string `json:"name"`
		} `json:"active_containers"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", fmt.Errorf("parse coi list json: %w", err)
	}
	suffix := "-" + strconv.Itoa(slot)
	for _, c := range payload.ActiveContainers {
		if c.Alias == alias && strings.HasSuffix(c.Name, suffix) {
			return c.Name, nil
		}
	}
	return "", nil
}

func runTolerant(args []string, tolerantNeedles ...string) error {
	cmd := exec.Command("coi", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		os.Stdout.Write(out)
		return nil
	}
	low := strings.ToLower(string(out))
	for _, n := range tolerantNeedles {
		if strings.Contains(low, n) {
			return nil
		}
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("coi %s: exit %d", strings.Join(args, " "), ee.ExitCode())
	}
	return fmt.Errorf("coi %s: %w", strings.Join(args, " "), err)
}
