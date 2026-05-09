// Package ahjoconfig loads the optional .ahjoconfig file from a repo's
// /repo checkout (read via `incus exec ... cat /repo/.ahjoconfig`).
package ahjoconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/lasselaakkonen/ahjo/internal/incus"
)

const filename = ".ahjoconfig"
const containerPath = "/repo/" + filename

// Config represents the .ahjoconfig file in a repo root.
type Config struct {
	Version    int              `toml:"version"`
	Run        []string         `toml:"run"`
	ForwardEnv []string         `toml:"forward_env"`
	AutoExpose AutoExposeConfig `toml:"auto_expose"`
}

// AutoExposeConfig overrides the global ~/.ahjo/config.toml [auto_expose]
// section for this repo. Each field is a pointer so "unset in .ahjoconfig"
// is distinguishable from "explicitly set to zero", and the global value
// is used for any field this repo doesn't override.
type AutoExposeConfig struct {
	Enabled *bool `toml:"enabled"`
	MinPort *int  `toml:"min_port"`
}

// Load reads .ahjoconfig from a host directory. Used by tests and any
// host-side context where the file is on the local filesystem; production
// readers should prefer LoadFromContainer.
func Load(dir string) (*Config, bool, error) {
	p := filepath.Join(dir, filename)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", filename, err)
	}
	return parse(b)
}

// LoadFromContainer reads /repo/.ahjoconfig from inside the named container
// via `incus exec ... cat`. Returns (nil, false, nil) when the file is
// absent. Used by every runtime caller now that there is no host-side
// worktree directory.
func LoadFromContainer(name string) (*Config, bool, error) {
	out, err := incus.Exec(name, "test", "-f", containerPath)
	if err != nil {
		// `test -f` exit 1 = missing file; surface as "absent", not an error.
		// incus.Exec wraps the exit code into the error message; sniff for it.
		if strings.Contains(err.Error(), "exit 1") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("probe %s in %s: %w", containerPath, name, err)
	}
	_ = out
	out, err = incus.Exec(name, "cat", containerPath)
	if err != nil {
		return nil, false, fmt.Errorf("read %s in %s: %w", containerPath, name, err)
	}
	return parse(out)
}

func parse(b []byte) (*Config, bool, error) {
	var c Config
	if err := toml.Unmarshal(b, &c); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", filename, err)
	}
	return &c, true, nil
}
