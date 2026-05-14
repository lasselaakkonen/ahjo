// Package paths centralizes the on-disk layout of ahjo's state directories.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	AhjoDirName     = ".ahjo"
	SharedDirName   = ".ahjo-shared"
	RegistryFile    = "registry.toml"
	PortsFile       = "ports.json"
	ConfigFile      = "config.toml"
	LockFile        = ".lock"
	SSHConfigFile   = "ssh-config"
	AliasesFile     = "aliases"
	KnownHostsFile  = "known_hosts"
	AhjoBaseProfile = "ahjo-base"

	// RepoMountPath is where each branch container holds its checkout.
	// Containers no longer bind-mount a host worktree — `git clone` runs
	// inside the container at this path during `ahjo repo add`, and
	// `incus copy` reflinks /repo into branch containers.
	RepoMountPath = "/repo"
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
func RegistryPath() string   { return filepath.Join(AhjoDir(), RegistryFile) }
func PortsPath() string      { return filepath.Join(AhjoDir(), PortsFile) }
func ConfigPath() string     { return filepath.Join(AhjoDir(), ConfigFile) }
func LockPath() string       { return filepath.Join(AhjoDir(), LockFile) }
func SSHConfigPath() string  { return filepath.Join(SharedDir(), SSHConfigFile) }
func AliasesPath() string    { return filepath.Join(SharedDir(), AliasesFile) }
func KnownHostsPath() string { return filepath.Join(SharedDir(), KnownHostsFile) }
func HostKeysDir() string    { return filepath.Join(AhjoDir(), "host-keys") }

func SlugHostKeysDir(slug string) string { return filepath.Join(HostKeysDir(), slug) }

// RepoEnvDir holds per-repo .env files (one per slug, mode 0600). Each file
// is layered over ~/.ahjo/.env by branchEnv so PATs scoped to one repo never
// leak into containers for another repo.
//
// The dir lives under SharedDir() — i.e. on the user's actual host filesystem
// (Mac home via virtiofs on macOS, the user's home on standalone Linux) —
// rather than inside the Lima VM disk image. This keeps per-repo PATs with
// the user across `limactl delete` / VM re-creation and matches the principle
// that user-owned state belongs on the host the user is sitting at.
func RepoEnvDir() string { return filepath.Join(SharedDir(), "repo-env") }

// SlugEnvPath is the per-repo .env file for slug.
func SlugEnvPath(slug string) string { return filepath.Join(RepoEnvDir(), slug+".env") }

// EnsureSkeleton creates the ~/.ahjo/ directory tree (idempotent).
func EnsureSkeleton() error {
	for _, d := range []string{
		AhjoDir(), HostKeysDir(), RepoEnvDir(),
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
