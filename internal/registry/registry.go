// Package registry persists ahjo's repos + worktrees state in ~/.ahjo/registry.toml.
package registry

import (
	"errors"
	"fmt"
	"net/url"
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
	Name              string   `toml:"name"`
	Aliases           []string `toml:"aliases"`
	Remote            string   `toml:"remote"`
	BarePath          string   `toml:"bare_path"`
	DefaultBase       string   `toml:"default_base"`
	BaseContainerName string   `toml:"base_container_name,omitempty"`
	// MacMirrorTarget is the per-repo default Mac path used by
	// `ahjo mirror`. Set on first activation; subsequent calls without
	// --target reuse it.
	MacMirrorTarget string `toml:"mac_mirror_target,omitempty"`
}

type Worktree struct {
	Repo           string    `toml:"repo"`
	Aliases        []string  `toml:"aliases"`
	Branch         string    `toml:"branch"`
	Slug           string    `toml:"slug"`
	WorktreePath   string    `toml:"worktree_path"`
	ContainerAlias string    `toml:"container_alias"`
	SSHPort        int       `toml:"ssh_port"`
	SSHHostKeysDir string    `toml:"ssh_host_keys_dir"`
	CreatedAt      time.Time `toml:"created_at"`
	IncusName      string    `toml:"incus_name,omitempty"`
}

var (
	slugSafeRE        = regexp.MustCompile(`[^a-z0-9-]+`)
	repoAliasRE       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]{0,62}[a-zA-Z0-9]$|^[a-zA-Z0-9]$`)
	scpLikeRE         = regexp.MustCompile(`^([^@]+@)?([^:]+):(.+)$`)
	gitURLPathSegment = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
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

// FindRepo returns the repo whose Name (slug) matches.
func (r *Registry) FindRepo(name string) *Repo {
	for i := range r.Repos {
		if r.Repos[i].Name == name {
			return &r.Repos[i]
		}
	}
	return nil
}

// FindRepoByAlias resolves any alias (auto or manual) to a repo.
func (r *Registry) FindRepoByAlias(alias string) *Repo {
	for i := range r.Repos {
		for _, a := range r.Repos[i].Aliases {
			if a == alias {
				return &r.Repos[i]
			}
		}
	}
	return nil
}

// FindWorktreeByAlias resolves any alias (auto or manual) to a worktree.
func (r *Registry) FindWorktreeByAlias(alias string) *Worktree {
	for i := range r.Worktrees {
		for _, a := range r.Worktrees[i].Aliases {
			if a == alias {
				return &r.Worktrees[i]
			}
		}
	}
	return nil
}

// AliasInUse reports whether alias is registered on any repo or worktree.
func (r *Registry) AliasInUse(alias string) bool {
	return r.FindRepoByAlias(alias) != nil || r.FindWorktreeByAlias(alias) != nil
}

// FindWorktree returns a worktree by (repo-name, branch). Used internally.
func (r *Registry) FindWorktree(repo, branch string) *Worktree {
	for i := range r.Worktrees {
		if r.Worktrees[i].Repo == repo && r.Worktrees[i].Branch == branch {
			return &r.Worktrees[i]
		}
	}
	return nil
}

func (r *Registry) RepoHasWorktrees(repoName string) bool {
	for i := range r.Worktrees {
		if r.Worktrees[i].Repo == repoName {
			return true
		}
	}
	return false
}

func (r *Registry) RemoveWorktree(repoName, branch string) {
	out := r.Worktrees[:0]
	for _, w := range r.Worktrees {
		if w.Repo == repoName && w.Branch == branch {
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

// ValidateAlias enforces a conservative character set so aliases stay
// safe in shell, paths, and SSH config without quoting.
func ValidateAlias(alias string) error {
	if alias == "" {
		return fmt.Errorf("alias must not be empty")
	}
	if !repoAliasRE.MatchString(alias) {
		return fmt.Errorf("invalid alias %q: use letters, digits, and any of . _ - / @", alias)
	}
	return nil
}

// AliasToSlug sanitizes any alias into a slug suitable for Incus container
// names and on-disk dirs: lowercase, [a-z0-9-] only, trimmed, capped at 50.
func AliasToSlug(alias string) string {
	s := slugSafeRE.ReplaceAllString(strings.ToLower(alias), "-")
	s = strings.Trim(s, "-")
	if len(s) > 50 {
		s = s[:50]
	}
	return s
}

// DeriveRepoAlias parses a git URL (https://, ssh://, scp-like, or path) and
// returns "<owner>/<repo>" with .git stripped and lowercased.
func DeriveRepoAlias(gitURL string) (string, error) {
	owner, repo, err := splitGitURL(gitURL)
	if err != nil {
		return "", err
	}
	owner = strings.ToLower(gitURLPathSegment.ReplaceAllString(owner, "-"))
	repo = strings.ToLower(gitURLPathSegment.ReplaceAllString(repo, "-"))
	owner = strings.Trim(owner, "-")
	repo = strings.Trim(repo, "-")
	if owner == "" || repo == "" {
		return "", fmt.Errorf("cannot derive alias from %q", gitURL)
	}
	return owner + "/" + repo, nil
}

func splitGitURL(s string) (owner, repo string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", fmt.Errorf("empty git URL")
	}
	var path string
	switch {
	case strings.Contains(s, "://"):
		u, perr := url.Parse(s)
		if perr != nil {
			return "", "", fmt.Errorf("parse %q: %w", s, perr)
		}
		path = strings.TrimPrefix(u.Path, "/")
	default:
		if m := scpLikeRE.FindStringSubmatch(s); m != nil {
			path = m[3]
		} else {
			// Local path or anything else: just use the basename(s).
			path = s
		}
	}
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")
	if path == "" {
		return "", "", fmt.Errorf("cannot extract path from %q", s)
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("cannot extract owner/repo from %q", s)
	}
	owner = parts[len(parts)-2]
	repo = parts[len(parts)-1]
	return owner, repo, nil
}

// AllocateRepoAlias returns an unused repo alias derived from gitURL,
// suffixing -2/-3/... if the base alias collides with an existing alias.
func (r *Registry) AllocateRepoAlias(gitURL string) (string, error) {
	base, err := DeriveRepoAlias(gitURL)
	if err != nil {
		return "", err
	}
	if !r.AliasInUse(base) {
		return base, nil
	}
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !r.AliasInUse(cand) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("could not allocate a unique alias for %q", gitURL)
}

// AllocateRepoSlug returns an unused on-disk slug for a repo, derived from
// its primary alias. Suffixes -2/-3/... on collision.
func (r *Registry) AllocateRepoSlug(primaryAlias string) string {
	base := AliasToSlug(primaryAlias)
	if base == "" {
		base = "repo"
	}
	if !r.repoSlugTaken(base) {
		return base
	}
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if len(cand) > 50 {
			cand = cand[:50]
		}
		if !r.repoSlugTaken(cand) {
			return cand
		}
	}
	return base
}

func (r *Registry) repoSlugTaken(slug string) bool {
	for i := range r.Repos {
		if r.Repos[i].Name == slug {
			return true
		}
	}
	return false
}

// MakeWorktreeAlias builds the canonical worktree alias as "<repo-alias>@<branch>".
func MakeWorktreeAlias(repoAlias, branch string) string {
	return repoAlias + "@" + branch
}

// MakeSlug builds a unique container slug from (repoSlug, branch).
func (r *Registry) MakeSlug(repoSlug, branch string) string {
	base := repoSlug + "-" + slugSafeRE.ReplaceAllString(strings.ToLower(branch), "-")
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
	return base
}

func (r *Registry) slugTaken(slug string) bool {
	for i := range r.Worktrees {
		if r.Worktrees[i].Slug == slug {
			return true
		}
	}
	return false
}
