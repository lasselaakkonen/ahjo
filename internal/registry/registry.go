// Package registry persists ahjo's repos + worktrees state in ~/.ahjo/registry.toml.
package registry

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

const Version = 1

type Registry struct {
	Version   int        `toml:"version"`
	Repos     []Repo     `toml:"repos"`
	Worktrees []Worktree `toml:"worktrees"`
}

type Repo struct {
	Name        string `toml:"name"`
	Remote      string `toml:"remote"`
	BarePath    string `toml:"bare_path"`
	DefaultBase string `toml:"default_base"`
}

type Worktree struct {
	Repo            string    `toml:"repo"`
	Branch          string    `toml:"branch"`
	Slug            string    `toml:"slug"`
	WorktreePath    string    `toml:"worktree_path"`
	ContainerAlias  string    `toml:"container_alias"`
	SSHPort         int       `toml:"ssh_port"`
	SSHHostKeysDir  string    `toml:"ssh_host_keys_dir"`
	CreatedAt       time.Time `toml:"created_at"`
}

var (
	repoNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	slugSafeRE = regexp.MustCompile(`[^a-z0-9-]+`)
)

// Load reads the registry from disk. Missing file returns an empty Registry{Version: 1}.
func Load() (*Registry, error) {
	b, err := os.ReadFile(paths.RegistryPath())
	if errors.Is(err, os.ErrNotExist) {
		return &Registry{Version: Version}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var r Registry
	if err := toml.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	if r.Version == 0 {
		r.Version = Version
	}
	if r.Version != Version {
		return nil, fmt.Errorf("registry version %d unsupported (this binary expects %d); upgrade or migrate manually", r.Version, Version)
	}
	return &r, nil
}

// Save writes the registry atomically (tempfile + rename).
func (r *Registry) Save() error {
	if err := paths.EnsureSkeleton(); err != nil {
		return err
	}
	r.Version = Version
	tmp, err := os.CreateTemp(paths.AhjoDir(), "registry-*.toml.tmp")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	defer os.Remove(tmp.Name())
	enc := toml.NewEncoder(tmp)
	if err := enc.Encode(r); err != nil {
		tmp.Close()
		return fmt.Errorf("encode registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), paths.RegistryPath())
}

func (r *Registry) FindRepo(name string) *Repo {
	for i := range r.Repos {
		if r.Repos[i].Name == name {
			return &r.Repos[i]
		}
	}
	return nil
}

func (r *Registry) FindWorktree(repo, branch string) *Worktree {
	for i := range r.Worktrees {
		if r.Worktrees[i].Repo == repo && r.Worktrees[i].Branch == branch {
			return &r.Worktrees[i]
		}
	}
	return nil
}

func (r *Registry) RepoHasWorktrees(repo string) bool {
	for i := range r.Worktrees {
		if r.Worktrees[i].Repo == repo {
			return true
		}
	}
	return false
}

func (r *Registry) RemoveWorktree(repo, branch string) {
	out := r.Worktrees[:0]
	for _, w := range r.Worktrees {
		if w.Repo == repo && w.Branch == branch {
			continue
		}
		out = append(out, w)
	}
	r.Worktrees = out
}

func (r *Registry) RemoveRepo(name string) {
	out := r.Repos[:0]
	for _, repo := range r.Repos {
		if repo.Name == name {
			continue
		}
		out = append(out, repo)
	}
	r.Repos = out
}

// ValidateRepoName returns an error if name doesn't match [a-z][a-z0-9-]{0,30}.
func ValidateRepoName(name string) error {
	if !repoNameRE.MatchString(name) {
		return fmt.Errorf("invalid repo name %q: must match [a-z][a-z0-9-]{0,30}", name)
	}
	return nil
}

// MakeSlug builds a unique slug from (repo, branch), checking r for collisions.
func (r *Registry) MakeSlug(repo, branch string) string {
	base := repo + "-" + slugSafeRE.ReplaceAllString(strings.ToLower(branch), "-")
	base = strings.Trim(base, "-")
	if len(base) > 50 {
		base = base[:50]
	}
	if !r.slugTaken(base) {
		return base
	}
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if len(cand) > 50 {
			cand = cand[:50]
		}
		if !r.slugTaken(cand) {
			return cand
		}
	}
	return base // give up; caller will likely error elsewhere
}

func (r *Registry) slugTaken(slug string) bool {
	for i := range r.Worktrees {
		if r.Worktrees[i].Slug == slug {
			return true
		}
	}
	return false
}
