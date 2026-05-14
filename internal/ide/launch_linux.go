//go:build !darwin

package ide

import (
	"fmt"
	"os/exec"
)

// LaunchOnHost invokes the local IDE's CLI shim with a vscode-remote URI
// (or, for Zed, an ssh:// URL). Used in the bare-Linux fallback path —
// when AHJO_HOST_IDES is unset and the user has the CLIs on PATH. The
// in-VM picker on a Mac-shim relay does NOT come through here; it posts
// to the paste daemon instead.
func LaunchOnHost(slug, host, path string) error {
	bin, args, err := cliInvocation(slug, host, path)
	if err != nil {
		return err
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("%s not on PATH", bin)
	}
	return spawnDetached(resolved, args...)
}

func cliInvocation(slug, host, path string) (string, []string, error) {
	uri := fmt.Sprintf("vscode-remote://ssh-remote+%s%s", host, path)
	switch slug {
	case Cursor:
		return "cursor", []string{"--folder-uri", uri}, nil
	case VSCode:
		return "code", []string{"--folder-uri", uri}, nil
	case VSCodeInsiders:
		return "code-insiders", []string{"--folder-uri", uri}, nil
	case Windsurf:
		return "windsurf", []string{"--folder-uri", uri}, nil
	case Zed:
		return "zed", []string{fmt.Sprintf("ssh://%s%s", host, path)}, nil
	}
	return "", nil, fmt.Errorf("unknown IDE slug %q", slug)
}

func spawnDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Start()
}
