//go:build darwin

package lima

import (
	"errors"
	"strings"
	"testing"
)

// findEnv returns the value of key in env (split on the first '=') or
// ("", false) if the key isn't present. Returns the *last* occurrence
// to mirror exec.Cmd's view of the env.
func findEnv(env []string, key string) (string, bool) {
	prefix := key + "="
	val, found := "", false
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			val = strings.TrimPrefix(e, prefix)
			found = true
		}
	}
	return val, found
}

// TestApplyAgentEnv_clearsLaunchdDefault is the regression test for the
// stale-master bug: when agent.Resolve fails, SSH_AUTH_SOCK in the
// inherited environ must not leak through into the limactl subprocess.
// Pinning macOS's launchd-default empty agent into Lima's persistent ssh
// ControlMaster used to break GitHub auth from inside the VM until a VM
// bounce — see CONTAINER-ISOLATION.md and the Env() doc comment.
func TestApplyAgentEnv_clearsLaunchdDefault(t *testing.T) {
	const launchd = "/private/tmp/com.apple.launchd.X/Listeners"
	base := []string{
		"PATH=/usr/bin",
		"SSH_AUTH_SOCK=" + launchd,
		"USER=test",
	}
	out := applyAgentEnv(base, "", errors.New("no agent"))

	got, ok := findEnv(out, "SSH_AUTH_SOCK")
	if !ok {
		t.Fatalf("SSH_AUTH_SOCK missing entirely from output: %v", out)
	}
	if got != "" {
		t.Fatalf("SSH_AUTH_SOCK should be empty when resolve fails, got %q", got)
	}
	for _, e := range out {
		if e == "SSH_AUTH_SOCK="+launchd {
			t.Fatalf("launchd-default agent leaked through: %v", out)
		}
	}
}

// TestApplyAgentEnv_setsResolvedSock confirms the happy path still
// overrides SSH_AUTH_SOCK to the resolved socket, even when the inherited
// environ has a different value.
func TestApplyAgentEnv_setsResolvedSock(t *testing.T) {
	const resolved = "/Users/x/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"
	base := []string{
		"PATH=/usr/bin",
		"SSH_AUTH_SOCK=/private/tmp/com.apple.launchd.X/Listeners",
	}
	out := applyAgentEnv(base, resolved, nil)
	got, ok := findEnv(out, "SSH_AUTH_SOCK")
	if !ok {
		t.Fatal("SSH_AUTH_SOCK missing from output")
	}
	if got != resolved {
		t.Fatalf("SSH_AUTH_SOCK = %q, want %q", got, resolved)
	}
}

// TestApplyAgentEnv_appendsWhenMissing covers the path where the base
// env had no SSH_AUTH_SOCK at all — applyAgentEnv must still produce a
// deterministic output so child processes can't pick up an unrelated
// env-leaked value via fallback resolution.
func TestApplyAgentEnv_appendsWhenMissing(t *testing.T) {
	base := []string{"PATH=/usr/bin"}
	out := applyAgentEnv(base, "", errors.New("no agent"))
	got, ok := findEnv(out, "SSH_AUTH_SOCK")
	if !ok {
		t.Fatal("SSH_AUTH_SOCK should be appended even when absent in base")
	}
	if got != "" {
		t.Fatalf("SSH_AUTH_SOCK = %q, want empty", got)
	}
}
