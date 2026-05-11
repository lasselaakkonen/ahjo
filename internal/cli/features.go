package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/devcontainer"
)

// applyRepoFeatures runs the trust-prompt → fetch → resolve → apply
// pipeline for cfg.Features against containerName. Returns the
// per-glob consent map captured during the prompt; callers persist it
// onto the Repo row in registry.toml at the end of repo-add.
//
// No-op when cfg.Features is empty. Auto-trusts the curated upstream
// (`ghcr.io/devcontainers/features/*`); already-consented globs in
// existingConsent skip the prompt. The user declining a prompt aborts
// repo-add — leaves the half-set-up container behind for inspection,
// per the design doc's no-auto-rollback rule.
//
// install.sh runs with the spec-defined envelope only (`_REMOTE_USER`,
// `_REMOTE_USER_HOME`, `_CONTAINER_USER`, `_CONTAINER_USER_HOME` plus
// options as ALL_CAPS). Host env (forward_env) is intentionally NOT
// passed — Features in the wild assume the spec's envelope and break
// when extra vars are present. Each Feature's `containerEnv` is
// persisted onto the container's Incus environment.* keys after a
// successful install, where downstream lifecycle hooks pick it up
// (literal pass-through; the spec's `${...}` interpolation is not
// honored, matching how cfg.ApplyContainerEnv handles devcontainer.json
// containerEnv).
func applyRepoFeatures(
	ctx context.Context,
	containerName string,
	cfg *devcontainer.Config,
	existingConsent map[string]bool,
	in io.Reader,
	out io.Writer,
) (newConsent map[string]bool, err error) {
	if cfg == nil || len(cfg.Features) == 0 {
		return nil, nil
	}

	// Sort sources for stable output / deterministic prompt order.
	sources := make([]string, 0, len(cfg.Features))
	for s := range cfg.Features {
		sources = append(sources, s)
	}
	sort.Strings(sources)

	consentedGlobs := make([]string, 0, len(existingConsent))
	for g, v := range existingConsent {
		if v {
			consentedGlobs = append(consentedGlobs, g)
		}
	}
	auto, known, prompt := devcontainer.PartitionFeatureSources(sources, consentedGlobs)

	if len(auto) > 0 {
		fmt.Fprintf(out, "→ Features (auto-trusted): %s\n", strings.Join(auto, ", "))
	}
	if len(known) > 0 {
		fmt.Fprintf(out, "→ Features (previously trusted): %s\n", strings.Join(known, ", "))
	}

	newConsent = map[string]bool{}
	if len(prompt) > 0 {
		fmt.Fprintln(out, "Repo declares devcontainer Features from non-curated sources.")
		fmt.Fprintln(out, "Each Feature runs install.sh as root inside the container during repo-add.")
		reader := bufio.NewReader(in)
		for _, glob := range prompt {
			fmt.Fprintf(out, "Trust Features matching %q for this repo? [y/N] ", glob)
			line, _ := reader.ReadString('\n')
			ans := strings.TrimSpace(strings.ToLower(line))
			if ans != "y" && ans != "yes" {
				return nil, fmt.Errorf("trust declined for %s; remove the matching `features:` entries or re-run after reviewing the source", glob)
			}
			newConsent[glob] = true
		}
	}

	tmpRoot, err := os.MkdirTemp("", "ahjo-features-")
	if err != nil {
		return nil, fmt.Errorf("mktemp for features: %w", err)
	}
	defer os.RemoveAll(tmpRoot)
	fetcher := &devcontainer.Fetcher{}

	fetch := func(ctx context.Context, ref devcontainer.FeatureRef, opts map[string]any) (devcontainer.FetchedFeature, error) {
		dir := filepath.Join(tmpRoot, devcontainer.SafeRefDir(ref.String()))
		if err := fetcher.Fetch(ctx, ref, dir); err != nil {
			return devcontainer.FetchedFeature{}, err
		}
		meta, err := devcontainer.ReadMetadata(dir, ref.String())
		if err != nil {
			return devcontainer.FetchedFeature{}, err
		}
		// Merge Feature-declared defaults before normalization so that
		// options the user omitted still arrive in the env envelope with
		// the spec's documented value. Without this, Features like
		// `git:1` that branch on a fixed default keyword (`version:
		// os-provided`) crash because they see VERSION="" instead.
		mergedOpts := devcontainer.ApplyOptionDefaults(opts, meta)
		stringOpts, err := devcontainer.NormalizeOptions(mergedOpts)
		if err != nil {
			return devcontainer.FetchedFeature{}, fmt.Errorf("feature %s: %w", ref, err)
		}
		return devcontainer.FetchedFeature{
			Ref: ref,
			Feature: devcontainer.Feature{
				// ID is the human-readable form (the OCI ref) so log
				// lines and warn: messages reference what the user
				// wrote in devcontainer.json. Apply transforms it via
				// SafeRefDir for the in-container tmp-dir path.
				ID:      ref.String(),
				Dir:     dir,
				Options: stringOpts,
			},
			Metadata: meta,
		}, nil
	}

	topLevel := make(map[string]any, len(cfg.Features))
	for k, v := range cfg.Features {
		topLevel[k] = v
	}
	ordered, err := devcontainer.Resolve(ctx, topLevel, fetch)
	if err != nil {
		return nil, err
	}

	runtimeEnv := devcontainer.RuntimeEnv()
	for _, ff := range ordered {
		fmt.Fprintf(out, "→ feature %s (apply)\n", ff.Ref)
		if err := devcontainer.Apply(containerName, ff.Feature, runtimeEnv, out); err != nil {
			return nil, fmt.Errorf("feature %s: %w", ff.Ref, err)
		}
		// Apply persists the Feature's containerEnv onto the container
		// as Incus environment.* keys (with ${VAR} expanded against
		// the current login env), so every subsequent `incus exec` —
		// the next Feature's install.sh, warm install, lifecycle
		// commands, the user's shell — inherits the new PATH/GOROOT/…
		// Features compose: Feature N's expanded values are visible
		// when Feature N+1's containerEnv is expanded.
	}
	return newConsent, nil
}
