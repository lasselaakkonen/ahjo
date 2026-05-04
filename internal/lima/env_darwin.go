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
// When agent.Resolve fails (e.g. nothing configured yet, no candidates)
// we fall through to os.Environ() unchanged so existing behavior is
// preserved — `ahjo doctor` is the place that calls out the gap.
func Env() []string {
	sock, _, err := resolved()
	if err != nil {
		return os.Environ()
	}
	return overrideEnv(os.Environ(), "SSH_AUTH_SOCK", sock)
}

// EnvVerbose is like Env but also returns a one-line note describing what
// was applied. Useful for `ahjo doctor` and the init step. The note is ""
// when nothing was overridden.
func EnvVerbose() (env []string, note string) {
	sock, label, err := resolved()
	if err != nil {
		return os.Environ(), ""
	}
	return overrideEnv(os.Environ(), "SSH_AUTH_SOCK", sock),
		fmt.Sprintf("forwarding %s agent: %s", label, sock)
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
