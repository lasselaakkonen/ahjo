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

// Version 2 (2026-05-08): worktrees → branches; bare repo and worktree paths
// dropped (containers hold the full clone at /repo). Existing v1 registries
// are rejected with an upgrade error; users nuke ~/.ahjo/registry.toml and
// re-add repos. See designdocs/no-more-worktrees.md.
const Version = 2

// maxSlugLen caps slug length so "ahjo-" + slug stays within Incus's 63-char
// RFC 1123 hostname limit, with headroom for a `-N` collision suffix.
const maxSlugLen = 55

type Registry struct {
	Version  int      `toml:"version"`
	Repos    []Repo   `toml:"repos"`
	Branches []Branch `toml:"branches"`
}

type Repo struct {
	Name              string   `toml:"name"`
	Aliases           []string `toml:"aliases"`
	Remote            string   `toml:"remote"`
	DefaultBase       string   `toml:"default_base"`
	BaseContainerName string   `toml:"base_container_name,omitempty"`
	// MacMirrorTarget is the per-repo default Mac path used by
	// `ahjo mirror`. Set on first activation; subsequent calls without
	// --target reuse it.
	MacMirrorTarget string `toml:"mac_mirror_target,omitempty"`
	// FeatureConsent records the user's one-time trust decisions for
	// non-curated devcontainer Feature sources, keyed by glob pattern
	// (e.g. `ghcr.io/foo/*`). The curated `ghcr.io/devcontainers/features/*`
	// set is auto-trusted and never appears here. Phase 2a reserves the
	// schema; the consent prompt and OCI fetch land in Phase 2b.
	FeatureConsent map[string]bool `toml:"feature_consent,omitempty"`
}

// Branch is a per-branch container holding a checkout at /repo. Replaces
// the v1 Worktree struct (containers no longer bind-mount a host worktree).
type Branch struct {
	Repo           string    `toml:"repo"`
	Aliases        []string  `toml:"aliases"`
	Branch         string    `toml:"branch"`
	Slug           string    `toml:"slug"`
	ContainerAlias string    `toml:"container_alias"`
	SSHPort        int       `toml:"ssh_port"`
	IncusName      string    `toml:"incus_name"`
	IsDefault      bool      `toml:"is_default,omitempty"`
	CreatedAt      time.Time `toml:"created_at"`
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

// FindBranchByAlias resolves any alias (auto or manual) to a branch.
func (r *Registry) FindBranchByAlias(alias string) *Branch {
	for i := range r.Branches {
		for _, a := range r.Branches[i].Aliases {
			if a == alias {
				return &r.Branches[i]
			}
		}
	}
	return nil
}

// AliasInUse reports whether alias is registered on any repo or branch.
func (r *Registry) AliasInUse(alias string) bool {
	return r.FindRepoByAlias(alias) != nil || r.FindBranchByAlias(alias) != nil
}

// FindBranch returns a branch by (repo-name, branch). Used internally.
func (r *Registry) FindBranch(repo, branch string) *Branch {
	for i := range r.Branches {
		if r.Branches[i].Repo == repo && r.Branches[i].Branch == branch {
			return &r.Branches[i]
		}
	}
	return nil
}

func (r *Registry) RepoHasBranches(repoName string) bool {
	for i := range r.Branches {
		if r.Branches[i].Repo == repoName {
			return true
		}
	}
	return false
}

func (r *Registry) RemoveBranch(repoName, branch string) {
	out := r.Branches[:0]
	for _, b := range r.Branches {
		if b.Repo == repoName && b.Branch == branch {
			continue
		}
		out = append(out, b)
	}
	r.Branches = out
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
// names and on-disk dirs: lowercase, [a-z0-9-] only, trimmed, capped at maxSlugLen.
func AliasToSlug(alias string) string {
	s := slugSafeRE.ReplaceAllString(strings.ToLower(alias), "-")
	s = strings.Trim(s, "-")
	if len(s) > maxSlugLen {
		s = s[:maxSlugLen]
	}
	return s
}

// withSlugSuffix returns "<base>-<n>" capped at maxSlugLen. The suffix is
// preserved verbatim; if base + suffix would overflow, base is trimmed to
// make room (and any trailing `-` is stripped to keep the slug well-formed).
func withSlugSuffix(base string, n int) string {
	suffix := fmt.Sprintf("-%d", n)
	if room := maxSlugLen - len(suffix); len(base) > room {
		base = strings.TrimRight(base[:room], "-")
	}
	return base + suffix
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
		cand := withSlugSuffix(base, i)
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

// MakeBranchAlias builds the canonical branch alias as "<repo-alias>@<branch>".
func MakeBranchAlias(repoAlias, branch string) string {
	return repoAlias + "@" + branch
}

// MakeSlug builds a unique container slug from (repoSlug, branch).
func (r *Registry) MakeSlug(repoSlug, branch string) string {
	base := repoSlug + "-" + slugSafeRE.ReplaceAllString(strings.ToLower(branch), "-")
	base = strings.Trim(base, "-")
	if len(base) > maxSlugLen {
		base = base[:maxSlugLen]
	}
	if !r.slugTaken(base) {
		return base
	}
	for i := 2; i < 1000; i++ {
		cand := withSlugSuffix(base, i)
		if !r.slugTaken(cand) {
			return cand
		}
	}
	return base
}

func (r *Registry) slugTaken(slug string) bool {
	for i := range r.Branches {
		if r.Branches[i].Slug == slug {
			return true
		}
	}
	return false
}
