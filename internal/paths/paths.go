// Package paths centralizes the on-disk layout of ahjo's state directories.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	AhjoDirName       = ".ahjo"
	SharedDirName     = ".ahjo-shared"
	RegistryFile      = "registry.toml"
	PortsFile         = "ports.json"
	ConfigFile        = "config.toml"
	LockFile          = ".lock"
	SSHConfigFile     = "ssh-config"
	AliasesFile       = "aliases"
	KnownHostsFile    = "known_hosts"
	AhjoBaseProfile   = "ahjo-base"
	CoiProfilesSubdir = "profiles"
)

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		panic(fmt.Errorf("user home not resolvable: %w", err))
	}
	return h
}

// Expand resolves a leading ~ to the user's home directory.
func Expand(p string) string {
	if p == "" {
		return p
	}
	if p == "~" {
		return home()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home(), p[2:])
	}
	return p
}

func AhjoDir() string { return filepath.Join(home(), AhjoDirName) }

// SharedDir is the directory both Mac-side and in-VM ahjo read/write so that
// ssh-config, aliases, and known_hosts are visible from both sides. On Mac
// and in the Lima VM it resolves to <mac-home>/.ahjo-shared (same physical
// path via virtiofs); on standalone Linux it falls back to ~/.ahjo-shared.
func SharedDir() string {
	if mac, ok := MacHostHome(); ok {
		return filepath.Join(mac, SharedDirName)
	}
	return filepath.Join(home(), SharedDirName)
}
func RegistryPath() string    { return filepath.Join(AhjoDir(), RegistryFile) }
func PortsPath() string       { return filepath.Join(AhjoDir(), PortsFile) }
func ConfigPath() string      { return filepath.Join(AhjoDir(), ConfigFile) }
func LockPath() string        { return filepath.Join(AhjoDir(), LockFile) }
func SSHConfigPath() string   { return filepath.Join(SharedDir(), SSHConfigFile) }
func AliasesPath() string     { return filepath.Join(SharedDir(), AliasesFile) }
func KnownHostsPath() string  { return filepath.Join(SharedDir(), KnownHostsFile) }
func ReposDir() string        { return filepath.Join(AhjoDir(), "repos") }
func WorktreesDir() string    { return filepath.Join(AhjoDir(), "worktrees") }
func HostKeysDir() string     { return filepath.Join(AhjoDir(), "host-keys") }
func ProfilesDir() string     { return filepath.Join(AhjoDir(), "profiles") }
func CoiProfilesDir() string  { return filepath.Join(home(), ".coi", CoiProfilesSubdir) }

func RepoBarePath(repo string) string  { return filepath.Join(ReposDir(), repo+".git") }
func WorktreePath(repo, branch string) string {
	return filepath.Join(WorktreesDir(), repo, branch)
}
func SlugHostKeysDir(slug string) string { return filepath.Join(HostKeysDir(), slug) }
func ProfilePath(name string) string     { return filepath.Join(ProfilesDir(), name) }
func CoiProfilePath(name string) string  { return filepath.Join(CoiProfilesDir(), name) }

// EnsureSkeleton creates the ~/.ahjo/ directory tree (idempotent).
func EnsureSkeleton() error {
	for _, d := range []string{
		AhjoDir(), ReposDir(), WorktreesDir(), HostKeysDir(), ProfilesDir(),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return ensureShared()
}

func ensureShared() error {
	if err := os.MkdirAll(SharedDir(), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", SharedDir(), err)
	}
	return nil
}
