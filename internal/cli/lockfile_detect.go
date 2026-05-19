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
// sequence is stable. The dedupe-by-name pass (see detectMatches) then
// keeps at most one match per `name`, so when several rows share a
// name (node has pnpm/yarn/npm; python has uv/poetry/pipenv/
// requirements; go has go.work/go.sum), the first match in table order
// wins and the rest are suppressed. Order rows in priority order:
//
//   - pnpm > yarn > npm (modern manager first)
//   - uv > poetry > pipenv > requirements.txt (modernity)
//   - go.work > go.sum (workspace mode supersedes single-module)
//
// A repo with both pnpm-lock and package-lock is prompted ONCE, for
// pnpm — declining at that point falls through to the generic picker
// rather than re-prompting for npm (today's behavior; the dedupe
// pass enforces it consistently). Within a row, probes are any-match
// (Docker fires on any of Dockerfile / compose.y*ml).
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
	// yarn.lock: APT-installed yarn from the node Feature
	// (installYarnUsingApt:true) is the classic v1 line, so the lock
	// guard is `--frozen-lockfile` (yarn 1.x). Berry's `--immutable`
	// would not parse against APT's binary.
	{probes: []string{"yarn.lock"}, name: "node", stack: "node", cmd: []string{"yarn", "install", "--frozen-lockfile"}},
	{probes: []string{"package-lock.json"}, name: "node", stack: "node", cmd: []string{"npm", "ci"}},
	{probes: []string{"uv.lock"}, name: "python", stack: "python", cmd: []string{"uv", "sync", "--frozen"}},
	// poetry.lock: pipx run ephemerally fetches poetry into ~/.local/pipx,
	// then forwards remaining argv to `poetry install`. Cached on disk so
	// subsequent invocations from the user's shell hit the same install.
	// Lives ahead of Pipfile/requirements so a repo carrying multiple
	// python manifests defaults to the highest-fidelity manager.
	{probes: []string{"poetry.lock"}, name: "python", stack: "python", cmd: []string{"pipx", "run", "poetry", "install", "--no-root", "--no-interaction"}},
	// Pipfile.lock: pipenv ships in the Python Feature's default
	// installTools set, so no extra Feature options needed — the binary
	// is on PATH after the Feature install phase.
	{probes: []string{"Pipfile.lock"}, name: "python", stack: "python", cmd: []string{"pipenv", "install", "--deploy"}},
	// requirements.txt: the long tail of Python projects. --user keeps
	// the install in ~/.local for the ubuntu account, matching the
	// stack's postCreateCommand convention. Runs from /repo so the
	// relative path resolves.
	{probes: []string{"requirements.txt"}, name: "python", stack: "python", cmd: []string{"pip", "install", "--user", "-r", "requirements.txt"}},
	{probes: []string{"Cargo.lock"}, name: "rust", stack: "rust", cmd: []string{"cargo", "fetch"}},
	// Gemfile.lock: the upstream Ruby Feature ships bundler with the
	// RVM install, so `bundle install` is available without a
	// postCreate prerequisite. RVM also honors .ruby-version
	// automatically, so no host-side version-file parsing is needed.
	{probes: []string{"Gemfile.lock"}, name: "ruby", stack: "ruby", cmd: []string{"bundle", "install"}},
	// composer.lock: the upstream PHP Feature with installComposer:true
	// stages composer alongside the interpreter. --no-interaction stops
	// composer from prompting on plugin trust; --no-progress keeps the
	// warm-install log uncluttered for non-TTY repo adds.
	{probes: []string{"composer.lock"}, name: "php", stack: "php", cmd: []string{"composer", "install", "--no-interaction", "--no-progress"}},
	// go.work: workspace mode. `go work sync` is the canonical
	// workspace operation — it resolves the workspace build list,
	// which downloads go.mod files for transitive deps into the
	// module cache. Not as aggressive as iterating `go mod download`
	// per `use` module, but it's the verb a workspace user reaches
	// for and it doesn't fail on a repo whose root `use` paths point
	// at packages with incomplete cgo deps.
	//
	// Placed above the go.sum row so the dedupe-by-name logic in
	// detectMatches picks workspace mode when both files are present.
	{probes: []string{"go.work"}, name: "go", stack: "go", cmd: []string{"go", "work", "sync"}},
	// go.sum (not go.mod): a module-only repo without dependencies ships
	// go.mod and `go mod download` would be a no-op. go.sum signals
	// actual deps to fetch. The Go toolchain is provided by features/go:1
	// which runs in the Feature-install phase before warm-install — so
	// unlike python/uv, the warm-install command is guaranteed to find
	// its binary.
	{probes: []string{"go.sum"}, name: "go", stack: "go", cmd: []string{"go", "mod", "download"}},
	// .pre-commit-config.yaml: applied as the ahjo/pre-commit Feature
	// directly. The Feature stages its own python+pipx surface and runs
	// `pre-commit install-hooks` during Feature install, so the warm-up
	// is the Feature install itself — no warm-install cmd on the row.
	// install-hooks (not install) is deliberate: it warms the hook
	// cache without writing to .git/hooks.
	{
		probes:   []string{".pre-commit-config.yaml"},
		name:     "pre-commit",
		features: map[string]interface{}{"ahjo/pre-commit": map[string]interface{}{}},
	},
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

// detectMatches walks detectTable in order, calling probe per row, and
// returns matches with at most one entry per `name` — the first row to
// hit for a given name wins, later rows with the same name are
// suppressed. Higher-priority rows (placed first in the table) that
// don't hit don't block lower-priority same-name rows from being
// probed. Extracted from detectStacks so the dedupe semantics can be
// exercised without an Incus container.
func detectMatches(probe func(detectEntry) string) []detectMatch {
	var matches []detectMatch
	seen := map[string]bool{}
	for _, e := range detectTable {
		if seen[e.name] {
			continue
		}
		if hit := probe(e); hit != "" {
			matches = append(matches, detectMatch{entry: e, hit: hit})
			seen[e.name] = true
		}
	}
	return matches
}

// detectStacks probes /repo inside containerName against every row of
// detectTable, returning matches in table order with the dedupe-by-name
// pass applied (see detectMatches).
func detectStacks(containerName string) ([]detectMatch, error) {
	return detectMatches(func(e detectEntry) string {
		return firstProbeHit(containerName, e)
	}), nil
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
