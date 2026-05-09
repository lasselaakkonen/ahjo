package devcontainer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/ahjoruntime"
	"github.com/lasselaakkonen/ahjo/internal/incus"
)

// Image aliases used by the build pipeline. ahjo-osbase is the local copy
// of upstream `images:ubuntu/24.04` (pulled once per ahjo version bump);
// ahjo-base is the published result of applying the ahjo-runtime Feature
// on top.
const (
	UpstreamRemote = "images:ubuntu/24.04"
	OSBaseAlias    = "ahjo-osbase"
	AhjoBaseAlias  = "ahjo-base"

	// remoteUser is the canonical Ubuntu cloud-image user. The Feature
	// runner passes this through as `_REMOTE_USER`; the script reads it
	// rather than hardcoding `ubuntu` so future renames touch only this
	// constant + the runtime callers (raw.idmap stays at UID 1000).
	remoteUser     = "ubuntu"
	remoteUserHome = "/home/ubuntu"
)

// BuildAhjoBase pulls upstream Ubuntu (idempotently), launches a transient
// container, applies the embedded ahjo-runtime Feature, publishes the
// result as `ahjo-base`, and deletes the transient container. Replaces
// the prior `coi build --profile ahjo-base` invocation.
//
// Force-rebuilds when force is true: deletes the ahjo-base alias before
// publishing so a stale image doesn't shadow the new one. The osbase
// alias stays — it's just a local mirror of upstream and re-pulling it
// every update would slow `ahjo update` for no gain.
func BuildAhjoBase(out io.Writer, force bool) error {
	if force {
		if err := incus.DeleteImageAlias(AhjoBaseAlias); err != nil {
			return fmt.Errorf("clear %s alias: %w", AhjoBaseAlias, err)
		}
		fmt.Fprintln(out, "  → cleared ahjo-base alias for force-rebuild")
	}

	fmt.Fprintf(out, "  → ensuring %s is in local image store as alias %s\n", UpstreamRemote, OSBaseAlias)
	if err := incus.ImageCopyRemote(UpstreamRemote, OSBaseAlias); err != nil {
		return err
	}

	suffix, err := randSuffix()
	if err != nil {
		return fmt.Errorf("mint build container name: %w", err)
	}
	buildName := "ahjo-build-" + suffix

	// Always tear down the transient container — both on success (cleanup)
	// and on failure (don't leak orphaned containers). ContainerDeleteForce
	// already tolerates "not found".
	defer func() {
		if err := incus.ContainerDeleteForce(buildName); err != nil {
			fmt.Fprintf(out, "  warn: cleanup %s: %v\n", buildName, err)
		}
	}()

	fmt.Fprintf(out, "  → incus launch %s %s\n", OSBaseAlias, buildName)
	if err := incus.Launch(OSBaseAlias, buildName); err != nil {
		return err
	}

	fmt.Fprintln(out, "  → waiting for systemd to come up")
	if err := incus.WaitReady(buildName, 60*time.Second); err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp("", "ahjo-runtime-feature-")
	if err != nil {
		return fmt.Errorf("mktemp for embedded feature: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if err := ahjoruntime.Materialize(tmpDir); err != nil {
		return fmt.Errorf("materialize ahjo-runtime feature: %w", err)
	}

	fmt.Fprintf(out, "  → applying %s Feature\n", ahjoruntime.FeatureID)
	if err := Apply(buildName, Feature{
		ID:  ahjoruntime.FeatureID,
		Dir: tmpDir,
	}, runtimeEnv(), out); err != nil {
		return err
	}

	fmt.Fprintf(out, "  → incus stop %s\n", buildName)
	if err := incus.Stop(buildName); err != nil {
		return err
	}

	fmt.Fprintf(out, "  → incus publish %s --alias %s\n", buildName, AhjoBaseAlias)
	return incus.Publish(buildName, AhjoBaseAlias)
}

// runtimeEnv is the user-identity envelope every Feature gets. Kept in one
// place so a future user rename (Phase 4) is a single edit to remoteUser /
// remoteUserHome, not a per-Feature audit.
func runtimeEnv() map[string]string {
	return map[string]string{
		"_REMOTE_USER":         remoteUser,
		"_REMOTE_USER_HOME":    remoteUserHome,
		"_CONTAINER_USER":      remoteUser,
		"_CONTAINER_USER_HOME": remoteUserHome,
	}
}

func randSuffix() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
