package devcontainer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tailscale/hujson"

	"github.com/lasselaakkonen/ahjo/internal/incus"
)

// configCandidates lists the per-repo devcontainer.json paths in spec
// precedence: .devcontainer/devcontainer.json wins, then the legacy flat
// path. Named variants under .devcontainer/<name>/ aren't honored — ahjo
// has no surface for picking one and the spec only requires the first two.
var configCandidates = []string{
	".devcontainer/devcontainer.json",
	".devcontainer.json",
}

// LegacyAhjoconfigName is the retired per-repo TOML config. Its presence in
// /repo aborts repo-add with a migration error pointing at this doc; the
// parser is gone entirely.
const LegacyAhjoconfigName = ".ahjoconfig"

// Config is the parsed honored subset of a per-repo devcontainer.json.
// Docker-flavored fields surface here only so we can detect and reject them
// — they're never honored. Lifecycle commands stay as raw JSON until
// rendered by lifecycle.go (the spec allows three forms per command).
type Config struct {
	// Source is the in-repo path the config was read from
	// (.devcontainer/devcontainer.json or .devcontainer.json), used for
	// error messages.
	Source string

	// Honored fields.
	RemoteUser    string                 `json:"remoteUser"`
	ContainerUser string                 `json:"containerUser"`
	ContainerEnv  map[string]string      `json:"containerEnv"`
	ForwardPorts  []int                  `json:"forwardPorts"`
	Features      map[string]interface{} `json:"features"`

	OnCreateCommand   json.RawMessage `json:"onCreateCommand"`
	PostCreateCommand json.RawMessage `json:"postCreateCommand"`
	PostStartCommand  json.RawMessage `json:"postStartCommand"`
	PostAttachCommand json.RawMessage `json:"postAttachCommand"`

	Customizations Customizations `json:"customizations"`

	// Rejected on parse — kept here as raw so detection sees presence
	// regardless of nested shape. Any non-zero value triggers an error.
	Image             json.RawMessage `json:"image"`
	Build             json.RawMessage `json:"build"`
	DockerComposeFile json.RawMessage `json:"dockerComposeFile"`
	Mounts            json.RawMessage `json:"mounts"`
	RunArgs           json.RawMessage `json:"runArgs"`
	Secrets           json.RawMessage `json:"secrets"`
}

// Customizations is the spec-defined extension point. Only the `ahjo` block
// is read — vscode/codespaces/etc. are kept as raw so they can be ignored
// without warning rather than failing the parse.
type Customizations struct {
	Ahjo AhjoCustomizations `json:"ahjo"`
}

// AhjoCustomizations is ahjo's per-repo extension namespace, replacing the
// retired .ahjoconfig schema. forward_env feeds into the env envelope at
// container-attach time; auto_expose overrides the global ~/.ahjo/config.toml
// [auto_expose] block.
type AhjoCustomizations struct {
	ForwardEnv []string             `json:"forward_env"`
	AutoExpose AhjoAutoExposeConfig `json:"auto_expose"`
}

// AhjoAutoExposeConfig overrides the global auto-expose settings. Pointer
// fields preserve the "unset" vs "explicitly false / zero" distinction, so
// a per-repo `enabled: false` actually disables when the global default is
// true.
type AhjoAutoExposeConfig struct {
	Enabled *bool `json:"enabled"`
	MinPort *int  `json:"min_port"`
}

// LoadFromHost reads devcontainer.json from a host directory (the repo
// root). Used by tests; production callers prefer LoadFromContainer because
// /repo lives only inside the container after Phase 1 of no-more-worktrees.
func LoadFromHost(repoDir string) (*Config, bool, error) {
	for _, rel := range configCandidates {
		p := filepath.Join(repoDir, rel)
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("read %s: %w", rel, err)
		}
		cfg, err := Parse(b, rel)
		if err != nil {
			return nil, true, err
		}
		return cfg, true, nil
	}
	return nil, false, nil
}

// LoadFromContainer reads /repo/<candidate>/devcontainer.json from inside
// the named container via `incus exec ... cat`. Returns (nil, false, nil)
// when no candidate file is present.
func LoadFromContainer(container string) (*Config, bool, error) {
	for _, rel := range configCandidates {
		p := "/repo/" + rel
		// `test -f` exit 1 means missing. incus.Exec wraps the exit code
		// into the error string; sniff for "exit 1" so we can treat
		// absent as "try the next candidate".
		if _, err := incus.Exec(container, "test", "-f", p); err != nil {
			if strings.Contains(err.Error(), "exit 1") {
				continue
			}
			return nil, false, fmt.Errorf("probe %s in %s: %w", p, container, err)
		}
		b, err := incus.Exec(container, "cat", p)
		if err != nil {
			return nil, false, fmt.Errorf("read %s in %s: %w", p, container, err)
		}
		cfg, err := Parse(b, rel)
		if err != nil {
			return nil, true, err
		}
		return cfg, true, nil
	}
	return nil, false, nil
}

// HasLegacyAhjoconfig reports whether /repo/.ahjoconfig is present in the
// container. Used by repo-add to abort with a migration error rather than
// silently ignoring a file the user thinks is being honored.
func HasLegacyAhjoconfig(container string) (bool, error) {
	_, err := incus.Exec(container, "test", "-f", "/repo/"+LegacyAhjoconfigName)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "exit 1") {
		return false, nil
	}
	return false, fmt.Errorf("probe /repo/%s in %s: %w", LegacyAhjoconfigName, container, err)
}

// Parse strips JSONC comments + trailing commas from b, json.Unmarshals
// into Config, and rejects Docker-flavored fields. The lax JSONC dialect
// is an intentional deviation from spec for compatibility with the ~10%
// of in-the-wild devcontainer.json files (especially Codespaces-targeting)
// that use trailing commas.
func Parse(b []byte, source string) (*Config, error) {
	std, err := hujson.Standardize(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", source, err)
	}
	// Unknown fields are accepted: the devcontainer spec is open by design,
	// and many in-the-wild files carry tool-specific extensions ahjo
	// doesn't read (vscode customizations, Codespaces hints, etc.).
	var c Config
	if err := json.Unmarshal(std, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", source, err)
	}
	c.Source = source
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// validate rejects Docker-flavored fields and `features:` (deferred to
// Phase 2b). Each error names the offending field plus the source path so
// the user knows exactly where to look.
//
// Empty / null raw JSON counts as absent; only a non-trivial value
// triggers the rejection.
func (c *Config) validate() error {
	type rule struct {
		field string
		raw   json.RawMessage
		hint  string
	}
	rules := []rule{
		{"image", c.Image, "Docker-flavored — ahjo runs Incus system containers, not Docker images. See designdocs/adopt-devcontainer-spec.md (Phase 4)."},
		{"build", c.Build, "Docker-flavored — ahjo doesn't build images from a Dockerfile. See designdocs/adopt-devcontainer-spec.md (Phase 4)."},
		{"dockerComposeFile", c.DockerComposeFile, "multi-container repos require Docker; not supported."},
		{"mounts", c.Mounts, "Docker-flavored — ahjo's mount story goes through the Incus device API, not the spec's mounts field."},
		{"runArgs", c.RunArgs, "Docker-flavored — Incus has no equivalent passthrough."},
		{"secrets", c.Secrets, "security-sensitive; needs separate design before any honoring."},
	}
	for _, r := range rules {
		if !rawJSONHasValue(r.raw) {
			continue
		}
		return fmt.Errorf("%s declares `%s` — %s", c.Source, r.field, r.hint)
	}
	if len(c.Features) > 0 {
		return fmt.Errorf("%s declares `features` — user-supplied Features need OCI fetch, deferred to Phase 2b of designdocs/adopt-devcontainer-spec.md. Remove the block to proceed", c.Source)
	}
	return nil
}

// rawJSONHasValue reports whether m is non-empty and non-null after
// trimming whitespace. `null`, `""`, `[]`, `{}` all count as "no value".
func rawJSONHasValue(m json.RawMessage) bool {
	s := strings.TrimSpace(string(m))
	switch s {
	case "", "null", "\"\"", "[]", "{}":
		return false
	}
	return true
}

// ApplyContainerEnv writes each entry in cfg.ContainerEnv as
// `environment.<KEY>=<VALUE>` on container so the env is visible to every
// in-container process at start. Literal pass-through — the spec's
// `${localEnv:...}` / `${containerEnv:...}` interpolations are not honored
// in Phase 2a (a literal `${...}` reaches the container as-is, which is
// almost never useful but at least preserves the user's intent).
//
// No-op for an empty / nil ContainerEnv.
func (c *Config) ApplyContainerEnv(setter func(key, value string) error) error {
	if c == nil {
		return nil
	}
	keys := make([]string, 0, len(c.ContainerEnv))
	for k := range c.ContainerEnv {
		keys = append(keys, k)
	}
	// Sort for deterministic apply order in tests + logs.
	sort.Strings(keys)
	for _, k := range keys {
		if err := setter("environment."+k, c.ContainerEnv[k]); err != nil {
			return fmt.Errorf("set containerEnv %s: %w", k, err)
		}
	}
	return nil
}

// CheckRemoteUser warns when a repo declares remoteUser/containerUser as
// anything other than ahjo's `ubuntu`. Returns the message rather than
// printing so the caller can route it to the right output stream; an empty
// string means no mismatch.
//
// Silently switching users would break git config / SSH keys that ahjo
// pre-stages at /home/ubuntu, so we never honor the override — but we
// warn loudly so the user knows their declaration was ignored.
func (c *Config) CheckRemoteUser(expected string) string {
	for _, field := range []struct{ name, val string }{
		{"remoteUser", c.RemoteUser},
		{"containerUser", c.ContainerUser},
	} {
		if field.val == "" || field.val == expected {
			continue
		}
		return fmt.Sprintf("warn: %s declares `%s: %q` — ahjo runs as %q regardless; declaration ignored", c.Source, field.name, field.val, expected)
	}
	return ""
}
