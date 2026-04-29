package ssh

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

// RegenerateConfig writes ~/.ahjo-shared/ssh-config from the registry atomically.
func RegenerateConfig(reg *registry.Registry) error {
	if err := os.MkdirAll(paths.SharedDir(), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", paths.SharedDir(), err)
	}
	wts := append([]registry.Worktree(nil), reg.Worktrees...)
	sort.Slice(wts, func(i, j int) bool { return wts[i].Slug < wts[j].Slug })

	var b strings.Builder
	fmt.Fprintln(&b, "# ahjo-managed: do not edit")
	fmt.Fprintf(&b, "# Generated %s\n\n", time.Now().UTC().Format(time.RFC3339))
	for _, w := range wts {
		fmt.Fprintf(&b, "Host ahjo-%s\n", w.Slug)
		fmt.Fprintln(&b, "  HostName 127.0.0.1")
		fmt.Fprintf(&b, "  Port %d\n", w.SSHPort)
		fmt.Fprintln(&b, "  User code")
		fmt.Fprintln(&b, "  IdentityFile ~/.ssh/id_ed25519")
		fmt.Fprintf(&b, "  UserKnownHostsFile %s\n", filepath.Join(w.SSHHostKeysDir, "known_hosts"))
		fmt.Fprintln(&b, "  StrictHostKeyChecking yes")
		fmt.Fprintln(&b, "  ForwardAgent yes")
		fmt.Fprintln(&b)
	}

	tmp, err := os.CreateTemp(paths.SharedDir(), ".ssh-config.tmp.*")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), paths.SSHConfigPath())
}
