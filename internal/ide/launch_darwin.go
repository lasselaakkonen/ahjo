//go:build darwin

package ide

import (
	"fmt"
	"os/exec"
)

// LaunchOnHost opens the IDE identified by slug pointed at <host>:<path>
// over SSH. Launched via `open` so the GUI app picks it up through its
// registered URL handler; the call returns as soon as `open` does, which
// is milliseconds — the IDE bootstraps in the background.
//
// Called by:
//   - the paste daemon's /open-ide handler (host bridge for in-VM picker)
//   - the bare-Mac fallback in internal/cli (compiled but never exercised
//     at runtime — internal/cli isn't built into the Mac shim binary)
func LaunchOnHost(slug, host, path string) error {
	args, err := openArgs(slug, host, path)
	if err != nil {
		return err
	}
	return spawnDetached("open", args...)
}

// openArgs returns the argv (excluding "open" itself) for launching slug
// against the given ssh target. VSCode-family clients all parse the same
// `vscode-remote://ssh-remote+host/path` shape under their own URL scheme;
// Zed takes a plain `ssh://` URL via its bundle argv.
func openArgs(slug, host, path string) ([]string, error) {
	switch slug {
	case Cursor:
		return []string{fmt.Sprintf("cursor://vscode-remote/ssh-remote+%s%s", host, path)}, nil
	case VSCode:
		return []string{fmt.Sprintf("vscode://vscode-remote/ssh-remote+%s%s", host, path)}, nil
	case VSCodeInsiders:
		return []string{fmt.Sprintf("vscode-insiders://vscode-remote/ssh-remote+%s%s", host, path)}, nil
	case Windsurf:
		return []string{fmt.Sprintf("windsurf://vscode-remote/ssh-remote+%s%s", host, path)}, nil
	case Zed:
		return []string{"-a", "Zed.app", "--args", fmt.Sprintf("ssh://%s%s", host, path)}, nil
	}
	return nil, fmt.Errorf("unknown IDE slug %q", slug)
}

// spawnDetached starts cmd without inheriting stdio, then returns
// immediately. We don't reap — `open` returns in milliseconds and the GUI
// app it dispatches to is the real long-running process.
func spawnDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Start()
}
