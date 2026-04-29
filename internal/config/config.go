// Package config loads the global ahjo settings from ~/.ahjo/config.toml.
// Defaults are applied for missing fields; the file is optional.
package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

const Version = 1

// Config controls the parts of ahjo behavior the user might tweak globally.
type Config struct {
	Version    int      `toml:"version"`
	ForwardEnv []string `toml:"forward_env"` // appended to template's defaults
	PortRange  Range    `toml:"port_range"`
}

type Range struct {
	Min int `toml:"min"`
	Max int `toml:"max"`
}

func defaults() *Config {
	return &Config{
		Version:    Version,
		ForwardEnv: []string{"CLAUDE_CODE_OAUTH_TOKEN"},
		PortRange:  Range{Min: 10000, Max: 10999},
	}
}

func Load() (*Config, error) {
	c := defaults()
	b, err := os.ReadFile(paths.ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := toml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.Version == 0 {
		c.Version = Version
	}
	if c.Version != Version {
		return nil, fmt.Errorf("config version %d unsupported (binary expects %d)", c.Version, Version)
	}
	if len(c.ForwardEnv) == 0 {
		c.ForwardEnv = defaults().ForwardEnv
	}
	if c.PortRange.Min == 0 && c.PortRange.Max == 0 {
		c.PortRange = defaults().PortRange
	}
	return c, nil
}
