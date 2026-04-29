// Package coi wraps the host `coi` binary.
package coi

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// ExecShell replaces the current process with `coi shell` from worktreeDir.
// Stdio + signals + exit code passthrough is automatic via execve.
func ExecShell(worktreeDir string) error {
	bin, err := exec.LookPath("coi")
	if err != nil {
		return fmt.Errorf("coi not on PATH: %w", err)
	}
	if err := os.Chdir(worktreeDir); err != nil {
		return fmt.Errorf("chdir %s: %w", worktreeDir, err)
	}
	return syscall.Exec(bin, []string{"coi", "shell"}, os.Environ())
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
