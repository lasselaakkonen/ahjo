// Package ahjoconfig loads the optional .ahjoconfig file from a repo worktree.
package ahjoconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

const filename = ".ahjoconfig"

// Config represents the .ahjoconfig file in a repo root.
type Config struct {
	Version    int      `toml:"version"`
	Run        []string `toml:"run"`
	ForwardEnv []string `toml:"forward_env"`
}

// Load reads .ahjoconfig from worktreePath. Returns (nil, false, nil) when
// the file is absent; (nil, false, err) on parse failure.
func Load(worktreePath string) (*Config, bool, error) {
	p := filepath.Join(worktreePath, filename)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", filename, err)
	}
	var c Config
	if err := toml.Unmarshal(b, &c); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", filename, err)
	}
	return &c, true, nil
}
