//go:build !darwin

package ide

import (
	"fmt"
	"os/exec"

	"github.com/lasselaakkonen/ahjo/internal/spawn"
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
	return spawn.Detached(resolved, args...)
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
