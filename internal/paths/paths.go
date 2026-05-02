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

func AhjoDir() string         { return filepath.Join(home(), AhjoDirName) }
func SharedDir() string       { return filepath.Join(home(), SharedDirName) }
func RegistryPath() string    { return filepath.Join(AhjoDir(), RegistryFile) }
func PortsPath() string       { return filepath.Join(AhjoDir(), PortsFile) }
func ConfigPath() string      { return filepath.Join(AhjoDir(), ConfigFile) }
func LockPath() string        { return filepath.Join(AhjoDir(), LockFile) }
func SSHConfigPath() string   { return filepath.Join(SharedDir(), SSHConfigFile) }
func AliasesPath() string     { return filepath.Join(SharedDir(), AliasesFile) }
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

// ensureShared creates the SharedDir; on Linux it's a symlink target under AhjoDir,
// on Mac it's a Lima 9p mount the user must set up — we just ensure it exists or
// is reachable, but never silently move things.
func ensureShared() error {
	if _, err := os.Stat(SharedDir()); err == nil {
		return nil
	}
	// On Linux we can helpfully create it as a real dir under ~/.ahjo/shared and
	// symlink ~/.ahjo-shared -> ~/.ahjo/shared. On Mac, the Lima mount must
	// already be in place; if it isn't, ahjo doctor reports it.
	if isLinux() {
		real := filepath.Join(AhjoDir(), "shared")
		if err := os.MkdirAll(real, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", real, err)
		}
		return os.Symlink(real, SharedDir())
	}
	return nil
}
