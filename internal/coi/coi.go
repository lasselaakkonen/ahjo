// Package coi wraps the host `coi` binary. Phase 1 of no-more-worktrees
// reduces this to image-build operations only — runtime container lifecycle
// (creation, attach, exec) moved to direct `incus` calls in
// internal/incus/incus.go and internal/cli/{repo,new,shell,claude}.go.
package coi

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// PinnedVersion is the COI release ahjo targets. ahjo passes VERSION=<this>
// to COI's install.sh, which downloads exactly that release of the coi
// binary. Because COI embeds profiles/default/build.sh into its binary, the
// build script that `coi build` runs is also pinned — so an upstream change
// to build.sh or the binary cannot silently break ahjo's pipeline.
// (install.sh itself stays at master; it just reads $VERSION.)
//
// Bump after testing the new release end-to-end:
//  1. set this to the new tag
//  2. ahjo init        # fresh VM ideally, or `ahjo nuke -y && ahjo init`
//  3. ahjo update      # exercises the in-VM rebuild path on an existing VM
//  4. ahjo new <repo> <branch> && ahjo shell <alias>   # confirm a container
//     builds and `claude` launches inside it
//
// History:
//   - v0.8.0 (2026-04-16): pin established. Embeds the `npm:pnpm@latest`
//     fix for mise's aqua backend, removing the need for ahjo's prior
//     sed-patch on profiles/default/build.sh.
//   - v0.8.1 (2026-05-07): sandbox JSON merge moved from in-container
//     `python3 -c` to pure Go (no python3 dependency in container; fixes
//     intermittent exit-1). tmux env-var forwarding moved off `export …`
//     onto `tmux new-session -e` (no longer leaks via ps). `sg` removed
//     from incus invocations (must be in incus-admin group with active
//     session). /etc/claude-code/managed-settings.json suppresses claude's
//     auto-mode prompt.
const PinnedVersion = "v0.8.1"

// InstalledVersion returns the version reported by `coi --version`, normalized
// to a tag like "v0.8.1". Returns "" when coi isn't on PATH or the output
// can't be parsed; callers should treat that as "unknown" rather than failure.
func InstalledVersion() string {
	out, err := exec.Command("coi", "--version").Output()
	if err != nil {
		return ""
	}
	for _, f := range strings.Fields(strings.TrimSpace(string(out))) {
		if strings.HasPrefix(f, "v") {
			return f
		}
	}
	return ""
}

// Build runs `coi build --profile <name>` (optionally with --force)
// inheriting stdio. The only runtime use of COI in Phase 1+ — image-build
// pipeline only.
func Build(profile string, force bool) error {
	args := []string{"build", "--profile", profile}
	if force {
		args = append(args, "--force")
	}
	cmd := exec.Command("coi", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// AhjoBaseAssets returns the embedded ahjo-base profile files (config.toml, build.sh).
func AhjoBaseAssets() (configTOML, buildSh []byte, err error) {
	configTOML, err = assets.ReadFile("assets/profiles/ahjo-base/config.toml")
	if err != nil {
		return nil, nil, fmt.Errorf("read embedded ahjo-base/config.toml: %w", err)
	}
	buildSh, err = assets.ReadFile("assets/profiles/ahjo-base/build.sh")
	if err != nil {
		return nil, nil, fmt.Errorf("read embedded ahjo-base/build.sh: %w", err)
	}
	return configTOML, buildSh, nil
}
