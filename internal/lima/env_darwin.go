//go:build darwin

package lima

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/lasselaakkonen/ahjo/internal/agent"
)

// resolvedOnce caches agent.Resolve for the lifetime of the process so
// frequent helpers (vmRunning, vmExists, doctor checks) don't re-probe
// ssh-add on every call.
var (
	resolvedOnce  sync.Once
	resolvedSock  string
	resolvedLabel string
	resolvedErr   error
)

func resolved() (string, string, error) {
	resolvedOnce.Do(func() {
		resolvedSock, resolvedLabel, resolvedErr = agent.Resolve()
	})
	return resolvedSock, resolvedLabel, resolvedErr
}

// Env returns the env that ahjo should use when invoking limactl. It's
// os.Environ() with SSH_AUTH_SOCK overridden to the agent socket picked
// during `ahjo init` (or auto-detected when only one candidate exists).
//
// When agent.Resolve fails (no agent configured, or the configured agent
// is currently empty/locked), we explicitly clear SSH_AUTH_SOCK rather
// than passing the user's shell value through. The reason is OpenSSH's
// ControlPersist: Lima opens an ssh ControlMaster on the very first
// limactl invocation and reuses it for the lifetime of the VM. Whatever
// SSH_AUTH_SOCK was active when that master is created becomes the
// agent-forwarding endpoint for every later session — `SSH_AUTH_SOCK`
// overrides on subsequent calls have no effect (see CloseSSHControlMaster
// below). If we silently passed through e.g. macOS's launchd-default
// empty agent, that empty agent would get pinned in and the user would
// get `Permission denied (publickey)` from inside the VM forever, even
// after they unlock 1Password. Clearing the var instead means the master
// forms with no forwarding, which is recoverable: `ahjo doctor --fix`
// closes the master and the next limactl call rebinds correctly.
func Env() []string {
	sock, _, err := resolved()
	return applyAgentEnv(os.Environ(), sock, err)
}

// EnvVerbose is like Env but also returns a one-line note describing what
// was applied. Useful for `ahjo doctor` and the init step. The note is ""
// when nothing was overridden.
func EnvVerbose() (env []string, note string) {
	sock, label, err := resolved()
	env = applyAgentEnv(os.Environ(), sock, err)
	if err != nil {
		return env, ""
	}
	return env, fmt.Sprintf("forwarding %s agent: %s", label, sock)
}

// applyAgentEnv returns base with SSH_AUTH_SOCK set to sock when
// resolveErr is nil, or to the empty string when resolveErr is non-nil.
// Pulled out of Env/EnvVerbose so the SSH_AUTH_SOCK decision is unit-
// testable without mocking agent.Resolve. See Env's doc comment for
// the rationale behind clearing rather than passing through.
func applyAgentEnv(base []string, sock string, resolveErr error) []string {
	if resolveErr != nil {
		return overrideEnv(base, "SSH_AUTH_SOCK", "")
	}
	return overrideEnv(base, "SSH_AUTH_SOCK", sock)
}

// Cmd returns exec.Command("limactl", args...) with cmd.Env preset to Env().
// All non-relay limactl invocations should use this so the right agent
// gets forwarded.
func Cmd(args ...string) *exec.Cmd {
	cmd := exec.Command("limactl", args...)
	cmd.Env = Env()
	return cmd
}

// CmdWithStdio is Cmd plus stdout/stderr piped to w. Convenience for steps.
func CmdWithStdio(w io.Writer, args ...string) *exec.Cmd {
	cmd := Cmd(args...)
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.Stdin = os.Stdin
	return cmd
}

// Exec replaces the current process with `limactl <args...>` using Env().
// Used for the relay path so the user's terminal stays tied to the in-VM
// process. Returns only on failure (success terminates the caller).
func Exec(args ...string) error {
	bin, err := exec.LookPath("limactl")
	if err != nil {
		return err
	}
	argv := append([]string{"limactl"}, args...)
	return syscall.Exec(bin, argv, Env())
}

// CloseSSHControlMaster gracefully closes the ssh ControlMaster Lima
// keeps for `limactl shell`. Lima's generated ssh config sets
// `ControlMaster auto` + `ControlPersist yes`, so the very first ssh
// session after VM boot establishes a multiplexed master and every
// subsequent shell piggybacks on it. Agent-forwarding state (which
// SSH_AUTH_SOCK to tunnel back to) is fixed at master-creation time,
// so changing SSH_AUTH_SOCK in our subprocess env has no effect until
// the master is closed and re-established.
//
// Call this after persisting a new agent socket so the next limactl
// shell session forwards the chosen agent — without bouncing the VM.
// Best-effort: returns nil when the master isn't running or ssh isn't
// on PATH; only surfaces real ssh errors.
func CloseSSHControlMaster(vmName string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cfg := home + "/.lima/" + vmName + "/ssh.config"
	if _, err := os.Stat(cfg); err != nil {
		return nil
	}
	sock := home + "/.lima/" + vmName + "/ssh.sock"
	if _, err := os.Stat(sock); err != nil {
		return nil
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		return nil
	}
	cmd := exec.Command("ssh", "-F", cfg, "-O", "exit", "lima-"+vmName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// "Exit request sent." prints to stderr on success; the command
		// itself can also fail benignly when the master is already gone.
		s := strings.TrimSpace(string(out))
		if strings.Contains(s, "No such file or directory") ||
			strings.Contains(s, "control socket") {
			return nil
		}
		return fmt.Errorf("ssh -O exit: %w (%s)", err, s)
	}
	return nil
}

// overrideEnv returns env with the named key set to value (replacing any
// existing entry, appending otherwise).
func overrideEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			out = append(out, prefix+value)
			replaced = true
			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, prefix+value)
	}
	return out
}
