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

// RegenerateConfig writes ~/.ahjo-shared/{ssh-config,aliases,known_hosts}
// from the registry atomically.
func RegenerateConfig(reg *registry.Registry) error {
	if err := os.MkdirAll(paths.SharedDir(), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", paths.SharedDir(), err)
	}
	bs := append([]registry.Branch(nil), reg.Branches...)
	sort.Slice(bs, func(i, j int) bool { return bs[i].Slug < bs[j].Slug })

	knownHosts := paths.KnownHostsPath()

	var b strings.Builder
	fmt.Fprintln(&b, "# ahjo-managed: do not edit")
	fmt.Fprintf(&b, "# Generated %s\n\n", time.Now().UTC().Format(time.RFC3339))
	for _, br := range bs {
		fmt.Fprintf(&b, "Host ahjo-%s\n", br.Slug)
		fmt.Fprintln(&b, "  HostName 127.0.0.1")
		fmt.Fprintf(&b, "  Port %d\n", br.SSHPort)
		fmt.Fprintln(&b, "  User ubuntu")
		fmt.Fprintln(&b, "  IdentityFile ~/.ssh/id_ed25519")
		fmt.Fprintf(&b, "  UserKnownHostsFile %s\n", knownHosts)
		fmt.Fprintln(&b, "  StrictHostKeyChecking yes")
		fmt.Fprintln(&b, "  ForwardAgent yes")
		fmt.Fprintln(&b)
	}

	if err := writeAtomic(paths.SSHConfigPath(), b.String(), 0o644); err != nil {
		return err
	}

	var amap strings.Builder
	fmt.Fprintln(&amap, "# ahjo-managed: alias\tslug, do not edit")
	for _, br := range bs {
		for _, a := range br.Aliases {
			fmt.Fprintf(&amap, "%s\t%s\n", a, br.Slug)
		}
	}
	if err := writeAtomic(paths.AliasesPath(), amap.String(), 0o644); err != nil {
		return err
	}

	if err := writeRepoAliases(reg); err != nil {
		return err
	}

	return writeKnownHosts(knownHosts, bs)
}

// writeRepoAliases emits the alias→repo-slug map the Mac shim consults to
// pick the Keychain account for a user-typed alias. Each repo alias resolves
// to the repo's own slug; each branch alias resolves to the parent repo's
// slug (not the branch slug) so per-repo PATs key off one identity regardless
// of how many branches share them.
func writeRepoAliases(reg *registry.Registry) error {
	repos := append([]registry.Repo(nil), reg.Repos...)
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })

	var b strings.Builder
	fmt.Fprintln(&b, "# ahjo-managed: alias\trepo-slug, do not edit")
	for _, r := range repos {
		for _, a := range r.Aliases {
			fmt.Fprintf(&b, "%s\t%s\n", a, r.Name)
		}
	}
	for _, br := range reg.Branches {
		for _, a := range br.Aliases {
			fmt.Fprintf(&b, "%s\t%s\n", a, br.Repo)
		}
	}
	return writeAtomic(paths.RepoAliasesPath(), b.String(), 0o644)
}

// writeKnownHosts concatenates each branch's per-slug known_hosts into a
// single Mac-readable file. Branches with no host keys yet are skipped —
// they'll be picked up on the next regeneration.
func writeKnownHosts(dst string, bs []registry.Branch) error {
	var b strings.Builder
	fmt.Fprintln(&b, "# ahjo-managed: do not edit")
	for _, br := range bs {
		src := filepath.Join(paths.SlugHostKeysDir(br.Slug), paths.KnownHostsFile)
		c, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("read %s: %w", src, err)
		}
		s := strings.TrimRight(string(c), "\n")
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fmt.Fprintln(&b, line)
		}
	}
	return writeAtomic(dst, b.String(), 0o644)
}

func writeAtomic(dst, content string, mode os.FileMode) error {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".tmp.*")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
