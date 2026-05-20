package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// TestDetectTable_BunRemoved guards the canonical mapping: bun.lockb
// was dropped because no bundled stack provides bun. If a `bun` row
// ever lands, it should pair with a real bundled stack — fail loudly
// here so the pairing is intentional.
func TestDetectTable_BunRemoved(t *testing.T) {
	for _, e := range detectTable {
		for _, p := range e.probes {
			if p == "bun.lockb" {
				t.Fatalf("bun.lockb still present in detectTable; only re-add when a bundled `bun` stack exists")
			}
		}
	}
}

// TestDetectTable_Shape checks every row is well-formed: non-empty
// name, non-empty probes, and one of stack / features set (mutually
// exclusive — a row either maps to a bundled stack or to an in-memory
// Feature set, never both). Stack-backed rows that ship a warm-install
// command must give it a non-empty argv. Cheap insurance against
// typos in the canonical table.
func TestDetectTable_Shape(t *testing.T) {
	for i, e := range detectTable {
		if e.name == "" || len(e.probes) == 0 {
			t.Fatalf("detectTable[%d] malformed: %+v", i, e)
		}
		for _, p := range e.probes {
			if p == "" {
				t.Fatalf("detectTable[%d] has empty probe: %+v", i, e)
			}
		}
		hasStack := e.stack != ""
		hasFeatures := e.features != nil
		if hasStack == hasFeatures {
			t.Fatalf("detectTable[%d] must set exactly one of stack/features (got stack=%q features=%v)", i, e.stack, e.features)
		}
		if len(e.cmd) > 0 && e.cmd[0] == "" {
			t.Fatalf("detectTable[%d] has empty cmd[0]: %+v", i, e)
		}
	}
}

// TestDetectTable_Pairings asserts the canonical mapping requested in
// the design table. Locking it in here means a future edit that
// silently re-points pnpm-lock.yaml at, say, the `bare` stack would
// be caught.
func TestDetectTable_Pairings(t *testing.T) {
	type want struct {
		stack    string
		bin      string // empty when row has no warm-install cmd
		features []string
	}
	wanted := map[string]want{
		"pnpm-lock.yaml":          {stack: "node", bin: "pnpm"},
		"yarn.lock":               {stack: "node", bin: "yarn"},
		"package-lock.json":       {stack: "node", bin: "npm"},
		"uv.lock":                 {stack: "python", bin: "uv"},
		"poetry.lock":             {stack: "python", bin: "pipx"},
		"Pipfile.lock":            {stack: "python", bin: "pipenv"},
		"requirements.txt":        {stack: "python", bin: "pip"},
		"Cargo.lock":              {stack: "rust", bin: "cargo"},
		"Gemfile.lock":            {stack: "ruby", bin: "bundle"},
		"composer.lock":           {stack: "php", bin: "composer"},
		"go.work":                 {stack: "go", bin: "go"},
		"go.sum":                  {stack: "go", bin: "go"},
		".pre-commit-config.yaml": {features: []string{"ahjo/prek"}},
		"Dockerfile":              {features: []string{"ahjo/docker"}},
		"compose.yaml":            {features: []string{"ahjo/docker"}},
		"compose.yml":             {features: []string{"ahjo/docker"}},
		"docker-compose.yaml":     {features: []string{"ahjo/docker"}},
		"docker-compose.yml":      {features: []string{"ahjo/docker"}},
	}
	seen := map[string]bool{}
	for _, e := range detectTable {
		for _, p := range e.probes {
			w, ok := wanted[p]
			if !ok {
				t.Fatalf("detectTable carries unexpected probe %q (row %q)", p, e.name)
			}
			seen[p] = true
			if e.stack != w.stack {
				t.Fatalf("probe %q stack = %q, want %q", p, e.stack, w.stack)
			}
			if w.bin != "" {
				if len(e.cmd) == 0 || e.cmd[0] != w.bin {
					t.Fatalf("probe %q cmd = %v, want first arg %q", p, e.cmd, w.bin)
				}
			} else if len(e.cmd) != 0 {
				t.Fatalf("probe %q expected no warm-install cmd, got %v", p, e.cmd)
			}
			for _, f := range w.features {
				if _, ok := e.features[f]; !ok {
					t.Fatalf("probe %q missing expected feature %q (features=%v)", p, f, e.features)
				}
			}
		}
	}
	for p := range wanted {
		if !seen[p] {
			t.Fatalf("detectTable missing probe %q", p)
		}
	}
}

// pipeIn writes `input` into a fresh os.Pipe and returns the read end
// as the *os.File suitable to pass into promptStackDetections. The
// writer is closed after the write so a reader that drains past the
// last newline sees EOF rather than blocking.
func pipeIn(t *testing.T, input string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	go func() {
		defer w.Close()
		_, _ = io.WriteString(w, input)
	}()
	t.Cleanup(func() { r.Close() })
	return r
}

// node / rust / docker fixtures used across multi-accept cases. Kept
// here (not in detectTable lookups) so tests don't break when the
// canonical table reorders.
func nodeMatch() detectMatch {
	return detectMatch{
		entry: detectEntry{
			probes: []string{"pnpm-lock.yaml"},
			name:   "node",
			stack:  "node",
			cmd:    []string{"pnpm", "install", "--frozen-lockfile"},
		},
		hit: "pnpm-lock.yaml",
	}
}

func rustMatch() detectMatch {
	return detectMatch{
		entry: detectEntry{
			probes: []string{"Cargo.lock"},
			name:   "rust",
			stack:  "rust",
			cmd:    []string{"cargo", "fetch"},
		},
		hit: "Cargo.lock",
	}
}

func dockerMatch() detectMatch {
	return detectMatch{
		entry: detectEntry{
			probes:   []string{"Dockerfile"},
			name:     "docker",
			features: map[string]interface{}{"ahjo/docker": map[string]interface{}{}},
		},
		hit: "Dockerfile",
	}
}

func names(ms []detectMatch) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.entry.name
	}
	return out
}

func TestPromptStackDetections(t *testing.T) {
	t.Run("no matches returns empty", func(t *testing.T) {
		var out bytes.Buffer
		got, err := promptStackDetections(nil, nil, &out, false)
		if err != nil || len(got) != 0 {
			t.Fatalf("got (%v, %v), want (nil, nil)", names(got), err)
		}
		if out.Len() != 0 {
			t.Fatalf("expected silent on empty matches, got %q", out.String())
		}
	})

	t.Run("single match accepted with empty input (default Y)", func(t *testing.T) {
		var out bytes.Buffer
		got, err := promptStackDetections([]detectMatch{nodeMatch()}, pipeIn(t, "\n"), &out, false)
		if err != nil || len(got) != 1 || got[0].entry.name != "node" {
			t.Fatalf("got (%v, %v), want ([node], nil)", names(got), err)
		}
		if !strings.Contains(out.String(), "Found pnpm-lock.yaml") {
			t.Fatalf("prompt missing probe filename: %q", out.String())
		}
	})

	t.Run("single match declined returns empty", func(t *testing.T) {
		var out bytes.Buffer
		got, err := promptStackDetections([]detectMatch{nodeMatch()}, pipeIn(t, "n\n"), &out, false)
		if err != nil || len(got) != 0 {
			t.Fatalf("got (%v, %v), want (nil, nil)", names(got), err)
		}
	})

	t.Run("multi-accept: both accepted yields both in pick order", func(t *testing.T) {
		var out bytes.Buffer
		got, err := promptStackDetections(
			[]detectMatch{nodeMatch(), rustMatch()},
			pipeIn(t, "y\ny\n"),
			&out, false,
		)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if want := []string{"node", "rust"}; !equalStrings(names(got), want) {
			t.Fatalf("got %v, want %v", names(got), want)
		}
	})

	t.Run("multi-accept: first declined, second accepted", func(t *testing.T) {
		var out bytes.Buffer
		got, err := promptStackDetections(
			[]detectMatch{nodeMatch(), rustMatch()},
			pipeIn(t, "n\ny\n"),
			&out, false,
		)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if want := []string{"rust"}; !equalStrings(names(got), want) {
			t.Fatalf("got %v, want %v", names(got), want)
		}
	})

	t.Run("polyglot: node + docker both accepted", func(t *testing.T) {
		var out bytes.Buffer
		got, err := promptStackDetections(
			[]detectMatch{nodeMatch(), dockerMatch()},
			pipeIn(t, "y\ny\n"),
			&out, false,
		)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if want := []string{"node", "docker"}; !equalStrings(names(got), want) {
			t.Fatalf("got %v, want %v", names(got), want)
		}
		// Docker prompt must mention the Feature, not a phantom warm-install cmd.
		if !strings.Contains(out.String(), "ahjo/docker") {
			t.Fatalf("docker prompt missing feature key: %q", out.String())
		}
		// Docker prompt must NOT advertise a warm-install cmd. Prompts
		// share lines (no trailing newline) so split on the prompt
		// terminator rather than '\n'.
		segments := strings.Split(out.String(), "[Y/n]: ")
		for _, seg := range segments {
			if strings.Contains(seg, "Found Dockerfile") && strings.Contains(seg, "run `") {
				t.Fatalf("docker prompt should not advertise a warm-install cmd: %q", seg)
			}
		}
	})

	t.Run("auto-yes accepts every match without reading stdin", func(t *testing.T) {
		var out bytes.Buffer
		got, err := promptStackDetections(
			[]detectMatch{nodeMatch(), rustMatch()},
			nil, &out, true,
		)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if want := []string{"node", "rust"}; !equalStrings(names(got), want) {
			t.Fatalf("got %v, want %v", names(got), want)
		}
	})

	t.Run("unrecognized response errors", func(t *testing.T) {
		var out bytes.Buffer
		_, err := promptStackDetections([]detectMatch{nodeMatch()}, pipeIn(t, "maybe\n"), &out, false)
		if err == nil {
			t.Fatalf("expected error on unrecognized response")
		}
	})
}

// TestDetectMatches_DedupeByName covers the workspace-supersedes-module
// case end-to-end: both go.work and go.sum probes claim a hit, but only
// the higher-priority go.work row should fire. Same shape would cover
// pnpm-lock superseding package-lock, but go is the row that motivated
// the dedupe (a workspace repo running `go mod download` after
// `go work sync` fails with "go mod download requires a main module").
func TestDetectMatches_DedupeByName(t *testing.T) {
	probe := func(e detectEntry) string {
		for _, p := range e.probes {
			if p == "go.work" || p == "go.sum" {
				return p
			}
		}
		return ""
	}
	got := detectMatches(probe)
	if len(got) != 1 {
		t.Fatalf("want 1 match, got %d: %v", len(got), got)
	}
	if got[0].hit != "go.work" {
		t.Fatalf("want go.work to win, got hit=%q", got[0].hit)
	}
	if len(got[0].entry.cmd) == 0 || got[0].entry.cmd[1] != "work" {
		t.Fatalf("want `go work sync` cmd, got %v", got[0].entry.cmd)
	}
}

// TestDetectMatches_FallthroughOnMiss confirms a higher-priority row's
// non-hit doesn't block lower-priority rows with the same name from
// being probed. Workspace absent, plain module present → go.sum row
// matches and `go mod download` runs.
func TestDetectMatches_FallthroughOnMiss(t *testing.T) {
	probe := func(e detectEntry) string {
		for _, p := range e.probes {
			if p == "go.sum" {
				return p
			}
		}
		return ""
	}
	got := detectMatches(probe)
	if len(got) != 1 || got[0].hit != "go.sum" {
		t.Fatalf("want single go.sum match, got %v", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
