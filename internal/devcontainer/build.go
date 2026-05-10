package devcontainer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/ahjodevtools"
	"github.com/lasselaakkonen/ahjo/internal/ahjoruntime"
	"github.com/lasselaakkonen/ahjo/internal/incus"
)

// Image aliases used by the build pipeline. ahjo-osbase is the local copy
// of upstream `images:ubuntu/24.04` (pulled once per ahjo version bump);
// ahjo-base is the published result of applying the curated upstream
// Features plus ahjo's two embedded Features on top.
const (
	UpstreamRemote = "images:ubuntu/24.04"
	OSBaseAlias    = "ahjo-osbase"
	AhjoBaseAlias  = "ahjo-base"

	// remoteUser is the canonical Ubuntu cloud-image user. The Feature
	// runner passes this through as `_REMOTE_USER`; each install.sh reads
	// it rather than hardcoding `ubuntu` so future renames touch only
	// this constant + the runtime callers (raw.idmap stays at UID 1000).
	remoteUser     = "ubuntu"
	remoteUserHome = "/home/ubuntu"
)

// upstreamBaseFeatures is the curated set of devcontainer Features baked
// into ahjo-base. Order in the resolved apply chain is driven by each
// Feature's own `dependsOn` / `installsAfter` metadata — listing them
// here just declares the install set. Versions are pinned to the major
// only (`:1`, `:2`) so ahjo's rolling-current-toolchains convention
// keeps applying: each `ahjo update` rebuilds ahjo-base with whatever's
// current under that major.
var upstreamBaseFeatures = []string{
	"ghcr.io/devcontainers/features/common-utils:2",
	"ghcr.io/devcontainers/features/git:1",
	"ghcr.io/devcontainers/features/github-cli:1",
}

// embeddedFeature couples a Feature ID with the Materialize fn that
// extracts its embedded files into a tmp dir. The build pipeline applies
// these in the order they appear in embeddedBaseFeatures, after the
// upstream Features above and before `incus publish`.
type embeddedFeature struct {
	id         string
	materialize func(dst string) error
}

// embeddedBaseFeatures is the fixed apply order for ahjo's own embedded
// Features. ahjo-default-dev-tools first (ahjo-runtime doesn't depend on
// it but it's the natural "developer surface" layer); ahjo-runtime last
// because its install.sh wires the systemd unit, sshd, claude-prepare,
// and Node + corepack — the ahjo-shaped layer that should sit on top of
// everything else.
var embeddedBaseFeatures = []embeddedFeature{
	{id: ahjodevtools.FeatureID, materialize: ahjodevtools.Materialize},
	{id: ahjoruntime.FeatureID, materialize: ahjoruntime.Materialize},
}

// BuildAhjoBase pulls upstream Ubuntu (idempotently), launches a transient
// container, applies the curated upstream Features (resolved by their own
// dep metadata) followed by ahjo's embedded Features, publishes the result
// as `ahjo-base`, and deletes the transient container.
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

	env := RuntimeEnv()

	if err := applyUpstreamBaseFeatures(buildName, env, out); err != nil {
		return err
	}

	for _, ef := range embeddedBaseFeatures {
		if err := applyEmbedded(buildName, ef, env, out); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "  → incus stop %s\n", buildName)
	if err := incus.Stop(buildName); err != nil {
		return err
	}

	fmt.Fprintf(out, "  → incus publish %s --alias %s\n", buildName, AhjoBaseAlias)
	return incus.Publish(buildName, AhjoBaseAlias)
}

// applyUpstreamBaseFeatures fetches, resolves, and applies the curated
// upstream Feature set in the order their dep metadata dictates. Mirrors
// the cli/features.go fetch closure used at repo-add time, with two
// differences: there's no trust prompt (every source is auto-trusted
// under CuratedTrustedGlob), and there are no per-Feature options to
// thread through (we pass them as empty maps).
func applyUpstreamBaseFeatures(container string, env map[string]string, out io.Writer) error {
	ctx := context.Background()

	tmpRoot, err := os.MkdirTemp("", "ahjo-base-upstream-")
	if err != nil {
		return fmt.Errorf("mktemp for upstream features: %w", err)
	}
	defer os.RemoveAll(tmpRoot)

	fetcher := &Fetcher{}
	fetch := func(ctx context.Context, ref FeatureRef, opts map[string]any) (FetchedFeature, error) {
		dir := filepath.Join(tmpRoot, safeRefDir(ref.String()))
		if err := fetcher.Fetch(ctx, ref, dir); err != nil {
			return FetchedFeature{}, err
		}
		meta, err := ReadMetadata(dir, ref.String())
		if err != nil {
			return FetchedFeature{}, err
		}
		stringOpts, err := NormalizeOptions(opts)
		if err != nil {
			return FetchedFeature{}, fmt.Errorf("feature %s: %w", ref, err)
		}
		return FetchedFeature{
			Ref: ref,
			Feature: Feature{
				ID:      safeRefDir(ref.String()),
				Dir:     dir,
				Options: stringOpts,
			},
			Metadata: meta,
		}, nil
	}

	topLevel := make(map[string]any, len(upstreamBaseFeatures))
	for _, src := range upstreamBaseFeatures {
		topLevel[src] = map[string]any{}
	}

	ordered, err := Resolve(ctx, topLevel, fetch)
	if err != nil {
		return fmt.Errorf("resolve upstream base features: %w", err)
	}

	for _, ff := range ordered {
		fmt.Fprintf(out, "  → applying upstream feature %s\n", ff.Ref)
		if err := Apply(container, ff.Feature, env, out); err != nil {
			return fmt.Errorf("feature %s: %w", ff.Ref, err)
		}
	}
	return nil
}

// applyEmbedded materializes and applies one of ahjo's embedded Features.
// The tmp dir is per-Feature (rather than one shared root) so a failure
// in materialize / Apply leaves a self-contained tree that's safe to
// `os.RemoveAll` regardless of which Feature blew up.
func applyEmbedded(container string, ef embeddedFeature, env map[string]string, out io.Writer) error {
	tmpDir, err := os.MkdirTemp("", "ahjo-base-embedded-"+ef.id+"-")
	if err != nil {
		return fmt.Errorf("mktemp for %s: %w", ef.id, err)
	}
	defer os.RemoveAll(tmpDir)
	if err := ef.materialize(tmpDir); err != nil {
		return fmt.Errorf("materialize %s: %w", ef.id, err)
	}
	fmt.Fprintf(out, "  → applying embedded feature %s\n", ef.id)
	return Apply(container, Feature{ID: ef.id, Dir: tmpDir}, env, out)
}

// safeRefDir maps an OCI ref to a filesystem-safe basename for the
// per-Feature extraction tmp dir. Mirrors the helper in cli/features.go;
// kept private here rather than shared because the two callers compose
// different surrounding workflows (base-bake vs. repo-add) and the
// helper is too small to justify a third package.
func safeRefDir(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// RuntimeEnv is the user-identity envelope every Feature gets. Kept in
// one place so a future user rename (Phase 4) is a single edit to
// remoteUser / remoteUserHome, not a per-Feature audit. Exported so
// the Phase 2b repo-add path can reuse the same envelope when applying
// user-supplied Features.
func RuntimeEnv() map[string]string {
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
