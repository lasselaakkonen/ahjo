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

	"github.com/lasselaakkonen/ahjo/internal/ahjocontainer"
	"github.com/lasselaakkonen/ahjo/internal/ahjofeatures"
	"github.com/lasselaakkonen/ahjo/internal/devcontainer"
)

// featuresRequestNesting reports whether any built-in Feature in ffs
// requests security.nesting on the container. Only `ahjo/*` Features
// (Registry=="ahjo") may request Incus instance-config changes; the same
// field in an OCI Feature is silently ignored. This is the allowlist
// mechanism: OCI/third-party Features cannot escalate container privileges
// by setting customizations.ahjo.nesting in their devcontainer-feature.json.
//
// Callers use this after Resolve to decide whether to enable nesting and
// restart the container before warm-install/lifecycle hooks run, so that
// dockerd (or any tool that needs userns nesting) is available to
// postCreateCommand.
func featuresRequestNesting(ffs []devcontainer.FetchedFeature) bool {
	for _, ff := range ffs {
		if ff.Ref.Registry == "ahjo" && ff.Metadata != nil && ff.Metadata.Customizations.Ahjo.Nesting {
			return true
		}
	}
	return false
}

// applyRepoFeatures runs the trust-prompt → fetch → resolve → apply
// pipeline for cfg.Features against containerName. Returns whether any
// resolved built-in Feature requested security.nesting on the container
// (so the caller can enable nesting and restart before lifecycle hooks
// run), plus the per-glob consent map captured during the prompt (callers
// persist it onto the Repo row in registry.toml at the end of repo-add).
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
	cfg *ahjocontainer.Config,
	existingConsent map[string]bool,
	in io.Reader,
	out io.Writer,
) (requestsNesting bool, newConsent map[string]bool, err error) {
	if cfg == nil || len(cfg.Features) == 0 {
		return false, nil, nil
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
				return false, nil, fmt.Errorf("trust declined for %s; remove the matching `features:` entries or re-run after reviewing the source", glob)
			}
			newConsent[glob] = true
		}
	}

	tmpRoot, err := os.MkdirTemp("", "ahjo-features-")
	if err != nil {
		return false, nil, fmt.Errorf("mktemp for features: %w", err)
	}
	defer os.RemoveAll(tmpRoot)
	fetcher := &devcontainer.Fetcher{}

	fetch := func(ctx context.Context, ref devcontainer.FeatureRef, opts map[string]any) (devcontainer.FetchedFeature, error) {
		dir := filepath.Join(tmpRoot, devcontainer.SafeRefDir(ref.String()))
		// Built-in Features (`ahjo/<name>`) materialize from the binary's
		// embed.FS instead of OCI. The Resolve loop sets Registry="ahjo"
		// for these via its parseRef helper. After materialization the
		// shape is identical to the OCI path — ReadMetadata still gates
		// `mounts`/`privileged`, ApplyOptionDefaults + NormalizeOptions
		// still run, and downstream Apply doesn't know the difference.
		if ref.Registry == "ahjo" {
			materialize, ok := ahjofeatures.Lookup(ref.Repository)
			if !ok {
				return devcontainer.FetchedFeature{}, fmt.Errorf("unknown built-in feature %q (known: %s)", ref.String(), ahjofeatures.List())
			}
			if err := materialize(dir); err != nil {
				return devcontainer.FetchedFeature{}, fmt.Errorf("materialize %s: %w", ref.String(), err)
			}
			meta, err := devcontainer.ReadMetadata(dir, ref.String())
			if err != nil {
				return devcontainer.FetchedFeature{}, err
			}
			mergedOpts := devcontainer.ApplyOptionDefaults(opts, meta)
			stringOpts, err := devcontainer.NormalizeOptions(mergedOpts)
			if err != nil {
				return devcontainer.FetchedFeature{}, fmt.Errorf("feature %s: %w", ref, err)
			}
			return devcontainer.FetchedFeature{
				Ref: ref,
				Feature: devcontainer.Feature{
					ID:      ref.String(),
					Dir:     dir,
					Options: stringOpts,
				},
				Metadata: meta,
			}, nil
		}
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
		return false, nil, err
	}

	runtimeEnv := devcontainer.RuntimeEnv()
	for _, ff := range ordered {
		fmt.Fprintf(out, "→ feature %s (apply)\n", ff.Ref)
		if err := devcontainer.Apply(ctx, containerName, ff.Feature, runtimeEnv, out); err != nil {
			return false, nil, fmt.Errorf("feature %s: %w", ff.Ref, err)
		}
		// Apply persists the Feature's containerEnv onto the container
		// as Incus environment.* keys (with ${VAR} expanded against
		// the current login env), so every subsequent `incus exec` —
		// the next Feature's install.sh, warm install, lifecycle
		// commands, the user's shell — inherits the new PATH/GOROOT/…
		// Features compose: Feature N's expanded values are visible
		// when Feature N+1's containerEnv is expanded.
	}
	return featuresRequestNesting(ordered), newConsent, nil
}
