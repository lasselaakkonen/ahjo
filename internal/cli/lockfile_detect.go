package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/ahjocontainer"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/stacks"
)

// detectEntry maps one or more on-disk probe files (lockfiles, Dockerfile,
// compose manifests) to the container config ahjo should apply when the
// repo carries no .ahjo/ahjocontainer.json. This table is the single
// source of truth for both the consent prompt in `ahjo repo add` and
// runWarmInstall's dispatch — keeping the two in lockstep so a new row
// enables both sides at once.
//
// An entry resolves to a config one of two ways:
//   - stack != "" → stacks.Load(stack) reads the embedded
//     internal/stacks/<stack>/ahjocontainer.json.
//   - features != nil → the entry produces an in-memory Config with just
//     those Features. Used for Docker, which is a Feature (ahjo/docker)
//     rather than a full language stack.
//
// cmd is the warm-install command run after Features are installed. nil
// means no warm-install step — Docker's entry uses this because the
// Feature install IS the warm-up.
//
// Order matters: detectStacks probes rows in table order, and
// promptStackDetections walks matches in the same order so the prompt
// sequence is stable. pnpm-first beats package-lock so a repo carrying
// both lockfiles defaults to the bundled `node` stack's corepack-pnpm
// postCreate. Within a row, probes are any-match (Docker fires on any
// of Dockerfile / compose.y*ml).
//
// `bun.lockb` intentionally absent: no bundled stack ships bun, so
// detecting it would suggest nothing and warm-installing it would
// fail. Re-add the row when a `bun` stack lands.
type detectEntry struct {
	probes   []string
	name     string
	stack    string
	features map[string]interface{}
	cmd      []string
}

var detectTable = []detectEntry{
	{probes: []string{"pnpm-lock.yaml"}, name: "node", stack: "node", cmd: []string{"pnpm", "install", "--frozen-lockfile"}},
	{probes: []string{"package-lock.json"}, name: "node", stack: "node", cmd: []string{"npm", "ci"}},
	{probes: []string{"uv.lock"}, name: "python", stack: "python", cmd: []string{"uv", "sync", "--frozen"}},
	{probes: []string{"Cargo.lock"}, name: "rust", stack: "rust", cmd: []string{"cargo", "fetch"}},
	// go.sum (not go.mod): a module-only repo without dependencies ships
	// go.mod and `go mod download` would be a no-op. go.sum signals
	// actual deps to fetch. The Go toolchain is provided by features/go:1
	// which runs in the Feature-install phase before warm-install — so
	// unlike python/uv, the warm-install command is guaranteed to find
	// its binary.
	{probes: []string{"go.sum"}, name: "go", stack: "go", cmd: []string{"go", "mod", "download"}},
	// Docker: any of Dockerfile / compose.y*ml triggers one prompt.
	// Applied as the ahjo/docker Feature directly rather than a bundled
	// stack — Docker isn't a language toolchain, so it composes with
	// whatever language stacks the user also accepts. No warm-install
	// command: laying down dockerd binaries is itself the warm-up.
	{
		probes:   []string{"Dockerfile", "compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"},
		name:     "docker",
		features: map[string]interface{}{"ahjo/docker": map[string]interface{}{}},
	},
}

// firstProbeHit returns the first probe file from e that exists in
// containerName's /repo, or "" if none match. Probing is a `test -f` via
// incus.Exec; a non-zero exit (file absent) is treated as a miss, every
// other error is collapsed to a miss too — consistent with how
// runWarmInstall has always interpreted this same probe. The matched
// filename is surfaced in the prompt so the user sees which artifact
// triggered the suggestion (helpful for Docker, where Dockerfile vs.
// compose.yaml leads to different mental models).
func firstProbeHit(containerName string, e detectEntry) string {
	for _, p := range e.probes {
		if _, err := incus.Exec(containerName, "test", "-f", paths.RepoMountPath+"/"+p); err == nil {
			return p
		}
	}
	return ""
}

// detectMatch pairs a detectEntry with the probe filename that fired.
// Carrying the hit filename through to the prompt keeps "Found <file>"
// accurate for multi-probe rows (Docker).
type detectMatch struct {
	entry detectEntry
	hit   string
}

// detectStacks probes /repo inside containerName against every row of
// detectTable, returning matches in table order. Each row contributes
// at most one match (first probe hit).
func detectStacks(containerName string) ([]detectMatch, error) {
	var matches []detectMatch
	for _, e := range detectTable {
		if hit := firstProbeHit(containerName, e); hit != "" {
			matches = append(matches, detectMatch{entry: e, hit: hit})
		}
	}
	return matches, nil
}

// promptStackDetections walks `matches` in order, asking the user
// whether to apply each suggestion. Returns the slice of accepted
// matches in pick order — the caller applies them in series with
// last-write-wins on config conflicts (which is rare since the rows
// cover disjoint Feature sets).
//
// Empty input defaults to yes (the prompt advertises `[Y/n]`).
// autoYes — set on non-TTY stdin or when --yes was passed — accepts
// every match without reading stdin, matching today's "scripted
// invocations never hang" ergonomic. When every match is declined,
// the caller falls through to the generic picker.
func promptStackDetections(matches []detectMatch, in *os.File, out io.Writer, autoYes bool) ([]detectMatch, error) {
	if len(matches) == 0 {
		return nil, nil
	}

	if autoYes {
		accepted := make([]detectMatch, len(matches))
		copy(accepted, matches)
		return accepted, nil
	}

	var accepted []detectMatch
	reader := bufio.NewReader(in)
	for _, m := range matches {
		fmt.Fprint(out, promptLine(m))
		line, err := reader.ReadString('\n')
		// Read failure (closed pipe etc.) is treated as accept-default —
		// mirrors promptContainerConfig's EOF handling so a half-closed
		// stdin doesn't error.
		if err != nil && line == "" {
			accepted = append(accepted, m)
			continue
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			accepted = append(accepted, m)
		case "n", "no":
			continue
		default:
			return nil, fmt.Errorf("unrecognized response %q (expected y/n)", strings.TrimSpace(line))
		}
	}
	return accepted, nil
}

// promptLine renders the per-entry consent prompt. Format diverges by
// shape: stack-backed rows announce both the stack name and the
// warm-install command; Feature-backed rows (Docker) drop the
// warm-install phrase entirely so the prompt doesn't lie about running
// nothing.
func promptLine(m detectMatch) string {
	e := m.entry
	if len(e.cmd) > 0 {
		return fmt.Sprintf("Found %s. Apply ahjo's %q stack and run `%s`? [Y/n]: ",
			m.hit, e.name, strings.Join(e.cmd, " "))
	}
	if e.features != nil {
		feats := make([]string, 0, len(e.features))
		for k := range e.features {
			feats = append(feats, k)
		}
		return fmt.Sprintf("Found %s. Install the %s Feature(s)? [Y/n]: ",
			m.hit, strings.Join(feats, ", "))
	}
	// Stack with no warm-install cmd (no bundled row uses this shape
	// today, but the rendering stays honest if one ever does).
	return fmt.Sprintf("Found %s. Apply ahjo's %q stack? [Y/n]: ", m.hit, e.name)
}

// anyNestedIncus reports whether any config in confs opts into nested
// Incus support. Used to decide whether to wire loop devices once for
// the container — the device wiring is per-container, not per-config,
// so any positive vote is enough.
func anyNestedIncus(confs []*ahjocontainer.Config) bool {
	for _, c := range confs {
		if c != nil && c.Customizations.Ahjo.NestedIncus {
			return true
		}
	}
	return false
}

// mergeFeaturesForApply returns a synthetic *Config whose Features map
// is the union of every input config's Features, with later-config keys
// overriding earlier ones. Other fields are left zero — callers only
// need this for applyRepoFeatures, which inspects Features alone. The
// union keeps trust-prompt / fetch / resolve / install as a single
// ordered pass instead of N partial passes that would prompt
// repeatedly and lose cross-Feature dependency ordering.
func mergeFeaturesForApply(confs []*ahjocontainer.Config) *ahjocontainer.Config {
	merged := map[string]interface{}{}
	for _, c := range confs {
		if c == nil {
			continue
		}
		for k, v := range c.Features {
			merged[k] = v
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return &ahjocontainer.Config{Features: merged}
}

// resolveDetectMatch turns an accepted detectMatch into a *Config ready
// for the apply pipeline. Stack-backed rows go through stacks.Load
// (and inherit its parse / source-path semantics); Feature-backed rows
// build an in-memory Config with just the declared Features. The
// Source string identifies the synthetic config in error messages so a
// later parse failure inside a Feature's options still points at the
// detected probe rather than nothing.
func resolveDetectMatch(m detectMatch) (*ahjocontainer.Config, error) {
	e := m.entry
	if e.features != nil {
		feats := make(map[string]interface{}, len(e.features))
		for k, v := range e.features {
			feats[k] = v
		}
		return &ahjocontainer.Config{
			Source:   fmt.Sprintf("ahjo built-in (detected: %s)", m.hit),
			Features: feats,
		}, nil
	}
	cfg, found, err := stacks.Load(e.stack)
	if err != nil {
		return nil, fmt.Errorf("load bundled stack %q: %w", e.stack, err)
	}
	if !found {
		return nil, fmt.Errorf("bundled stack %q not found (detectTable row out of sync with internal/stacks/)", e.stack)
	}
	return cfg, nil
}
