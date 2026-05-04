//go:build darwin

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/agent"
	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/initflow"
	"github.com/lasselaakkonen/ahjo/internal/lima"
)

// pickAgentStep returns the init step that picks which host ssh-agent
// gets forwarded into the Lima VM. The chosen socket is persisted in
// ~/.ahjo/config.toml [mac].ssh_auth_sock and read by every limactl
// invocation via lima.Env (see internal/lima/env_darwin.go).
//
// macOS's default $SSH_AUTH_SOCK is launchd's empty agent, while real
// keys live behind 1Password / Secretive / gpg-agent — reached on the
// host via ~/.ssh/config IdentityAgent, which Lima doesn't honor when
// forwarding. Picking explicitly and overriding SSH_AUTH_SOCK only in
// our subprocess env avoids editing the user's shell rc.
func pickAgentStep(yes bool) initflow.Step {
	return initflow.Step{
		Title: "Detect ssh-agent for VM forwarding",
		Skip: func() (bool, string, error) {
			cfg, err := config.Load()
			if err != nil || cfg.Mac.SSHAuthSock == "" {
				return false, "", nil
			}
			sock, label, err := agent.Resolve()
			if err != nil || sock != cfg.Mac.SSHAuthSock {
				return false, "", nil
			}
			return true, fmt.Sprintf("using %s: %s", label, sock), nil
		},
		Show: "probe 1Password / Secretive / gpg-agent / ssh_config IdentityAgent;\n" +
			"persist the chosen socket to ~/.ahjo/config.toml [mac].ssh_auth_sock;\n" +
			"every `limactl` invocation will forward this agent into the VM",
		Action: func(out io.Writer) error {
			cands := agent.Detect()
			switch len(cands) {
			case 0:
				return fmt.Errorf("no ssh-agent with keys was found on this Mac. " +
					"Load a key into 1Password (or your agent) and re-run `ahjo init`")
			case 1:
				return saveAgentChoice(out, cands[0])
			}
			c, err := pickFromCandidates(out, cands, yes)
			if err != nil {
				return err
			}
			return saveAgentChoice(out, c)
		},
	}
}

// saveAgentChoice persists the chosen socket and closes any existing ssh
// ControlMaster Lima holds for the VM. The master close is what lets the
// override take effect on the *next* `limactl shell` without a VM bounce —
// see lima.CloseSSHControlMaster for the why.
func saveAgentChoice(out io.Writer, c agent.Candidate) error {
	if err := config.SaveMacSSHAuthSock(c.Socket); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(out, "  using %s: %s (%d key(s))\n", c.Label, c.Socket, c.Keys)
	if err := lima.CloseSSHControlMaster(vmName); err != nil {
		fmt.Fprintf(out, "  warn: could not close existing ssh master (%v); a `limactl stop %s && limactl start %s` may be needed\n", err, vmName, vmName)
	}
	return nil
}

// pickFromCandidates renders a numbered list and reads stdin for an index.
// With --yes it returns cands[0] without prompting.
func pickFromCandidates(out io.Writer, cands []agent.Candidate, yes bool) (agent.Candidate, error) {
	if yes {
		fmt.Fprintf(out, "  --yes: defaulting to %s\n", cands[0].Label)
		return cands[0], nil
	}
	fmt.Fprintln(out, "  multiple ssh-agents detected:")
	for i, c := range cands {
		fmt.Fprintf(out, "    [%d] %s — %s (%d key(s))\n", i+1, c.Label, c.Socket, c.Keys)
	}
	fmt.Fprintf(out, "  Choose [1-%d]: ", len(cands))
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return agent.Candidate{}, err
		}
		return agent.Candidate{}, fmt.Errorf("no input")
	}
	idx, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
	if err != nil || idx < 1 || idx > len(cands) {
		return agent.Candidate{}, fmt.Errorf("invalid selection %q", strings.TrimSpace(sc.Text()))
	}
	return cands[idx-1], nil
}
