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
//
// The same struct is loaded by both the Mac-side shim and the in-VM Linux
// ahjo, but each reads its own physical ~/.ahjo/config.toml. Platform-only
// fields live under a namespaced section ([mac], [linux]) so a reader can
// see at a glance which side a field applies to.
type Config struct {
	Version    int              `toml:"version"`
	ForwardEnv []string         `toml:"forward_env"` // appended to template's defaults
	PortRange  Range            `toml:"port_range"`
	AutoExpose AutoExposeConfig `toml:"auto_expose"`
	Mac        MacConfig        `toml:"mac"` // host-only; ignored in the VM
}

type Range struct {
	Min int `toml:"min"`
	Max int `toml:"max"`
}

// AutoExposeConfig controls automatic exposure of container loopback ports
// to the host. Reconciled from `ss -tlnH` inside the container on `ahjo shell`
// and `ahjo expose --sync`.
//
// Enabled is a *bool so a per-repo `customizations.ahjo.auto_expose` block
// in devcontainer.json can distinguish "unset" from "explicitly disabled"
// when overriding the global default.
type AutoExposeConfig struct {
	Enabled *bool `toml:"enabled"`
	MinPort int   `toml:"min_port"`
}

const DefaultAutoExposeMinPort = 3000

// MacConfig holds settings only consumed by the Mac-side shim.
type MacConfig struct {
	// SSHAuthSock pins which agent socket on the host gets forwarded into
	// the VM. Set by `ahjo init` after detecting the user's real agent
	// (e.g. 1Password). Empty means "fall back to whatever $SSH_AUTH_SOCK
	// is in the shell that invoked ahjo".
	SSHAuthSock string `toml:"ssh_auth_sock"`
}

func defaults() *Config {
	enabled := true
	return &Config{
		Version:    Version,
		ForwardEnv: []string{"CLAUDE_CODE_OAUTH_TOKEN"},
		PortRange:  Range{Min: 10000, Max: 10999},
		AutoExpose: AutoExposeConfig{Enabled: &enabled, MinPort: DefaultAutoExposeMinPort},
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
	if c.AutoExpose.Enabled == nil {
		c.AutoExpose.Enabled = defaults().AutoExpose.Enabled
	}
	if c.AutoExpose.MinPort == 0 {
		c.AutoExpose.MinPort = DefaultAutoExposeMinPort
	}
	return c, nil
}

// Save writes c atomically to ~/.ahjo/config.toml (tempfile + rename).
func (c *Config) Save() error {
	if err := paths.EnsureSkeleton(); err != nil {
		return err
	}
	c.Version = Version
	tmp, err := os.CreateTemp(paths.AhjoDir(), "config-*.toml.tmp")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := toml.NewEncoder(tmp).Encode(c); err != nil {
		tmp.Close()
		return fmt.Errorf("encode config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), paths.ConfigPath())
}

// SaveMacSSHAuthSock loads the current config, sets [mac].ssh_auth_sock,
// and writes the file back. Other sections are preserved.
func SaveMacSSHAuthSock(sock string) error {
	c, err := Load()
	if err != nil {
		return err
	}
	c.Mac.SSHAuthSock = sock
	return c.Save()
}
