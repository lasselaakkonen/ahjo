package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// TestLockfileTable_BunRemoved guards the canonical mapping: bun.lockb
// was dropped because no bundled stack provides bun. If a `bun` row
// ever lands, it should pair with a real bundled stack — fail loudly
// here so the pairing is intentional.
func TestLockfileTable_BunRemoved(t *testing.T) {
	for _, e := range lockfileTable {
		if e.lockfile == "bun.lockb" {
			t.Fatalf("bun.lockb still present in lockfileTable; only re-add when a bundled `bun` stack exists")
		}
	}
}

// TestLockfileTable_Shape checks every row is well-formed: non-empty
// lockfile, non-empty stack, non-empty command. Cheap insurance
// against typos in the canonical table.
func TestLockfileTable_Shape(t *testing.T) {
	for i, e := range lockfileTable {
		if e.lockfile == "" || e.stack == "" || len(e.cmd) == 0 || e.cmd[0] == "" {
			t.Fatalf("lockfileTable[%d] malformed: %+v", i, e)
		}
	}
}

// TestLockfileTable_Pairings asserts the canonical mapping requested
// in the design table. Locking it in here means a future edit that
// silently re-points pnpm-lock.yaml at, say, the `bare` stack would
// be caught.
func TestLockfileTable_Pairings(t *testing.T) {
	want := map[string]struct {
		stack string
		bin   string
	}{
		"pnpm-lock.yaml":    {"node", "pnpm"},
		"package-lock.json": {"node", "npm"},
		"uv.lock":           {"python", "uv"},
		"Cargo.lock":        {"rust", "cargo"},
	}
	got := map[string]struct {
		stack string
		bin   string
	}{}
	for _, e := range lockfileTable {
		got[e.lockfile] = struct {
			stack string
			bin   string
		}{e.stack, e.cmd[0]}
	}
	if len(got) != len(want) {
		t.Fatalf("lockfileTable size = %d, want %d (rows: %+v)", len(got), len(want), got)
	}
	for lf, w := range want {
		g, ok := got[lf]
		if !ok {
			t.Fatalf("lockfileTable missing %q", lf)
		}
		if g != w {
			t.Fatalf("lockfileTable[%q] = %+v, want %+v", lf, g, w)
		}
	}
}

// pipeIn writes `input` into a fresh os.Pipe and returns the read end
// as the *os.File suitable to pass into promptLockfileStack. The
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

func TestPromptLockfileStack(t *testing.T) {
	node := lockfileEntry{"pnpm-lock.yaml", "node", []string{"pnpm", "install", "--frozen-lockfile"}}
	rust := lockfileEntry{"Cargo.lock", "rust", []string{"cargo", "fetch"}}

	t.Run("no matches returns empty", func(t *testing.T) {
		var out bytes.Buffer
		got, err := promptLockfileStack(nil, nil, &out, false)
		if err != nil || got != "" {
			t.Fatalf("got (%q, %v), want (\"\", nil)", got, err)
		}
		if out.Len() != 0 {
			t.Fatalf("expected silent on empty matches, got %q", out.String())
		}
	})

	t.Run("single match accepted with empty input (default Y)", func(t *testing.T) {
		var out bytes.Buffer
		got, err := promptLockfileStack([]lockfileEntry{node}, pipeIn(t, "\n"), &out, false)
		if err != nil || got != "node" {
			t.Fatalf("got (%q, %v), want (\"node\", nil)", got, err)
		}
		if !strings.Contains(out.String(), "Found pnpm-lock.yaml") {
			t.Fatalf("prompt missing lockfile name: %q", out.String())
		}
	})

	t.Run("single match accepted with y", func(t *testing.T) {
		var out bytes.Buffer
		got, _ := promptLockfileStack([]lockfileEntry{node}, pipeIn(t, "y\n"), &out, false)
		if got != "node" {
			t.Fatalf("got %q, want node", got)
		}
	})

	t.Run("single match declined returns empty (caller falls through)", func(t *testing.T) {
		var out bytes.Buffer
		got, err := promptLockfileStack([]lockfileEntry{node}, pipeIn(t, "n\n"), &out, false)
		if err != nil || got != "" {
			t.Fatalf("got (%q, %v), want (\"\", nil)", got, err)
		}
	})

	t.Run("first declined, second accepted", func(t *testing.T) {
		var out bytes.Buffer
		got, err := promptLockfileStack(
			[]lockfileEntry{node, rust},
			pipeIn(t, "n\ny\n"),
			&out, false,
		)
		if err != nil || got != "rust" {
			t.Fatalf("got (%q, %v), want (\"rust\", nil)", got, err)
		}
		// The accepted match isn't echoed as "also detected"; the
		// declined match shouldn't be either (it was an explicit no).
		if strings.Contains(out.String(), "also detected") {
			t.Fatalf("unexpected `also detected` line when only one match was accepted: %q", out.String())
		}
	})

	t.Run("first accepted prints also-detected for the rest", func(t *testing.T) {
		var out bytes.Buffer
		got, _ := promptLockfileStack(
			[]lockfileEntry{node, rust},
			pipeIn(t, "y\n"),
			&out, false,
		)
		if got != "node" {
			t.Fatalf("got %q, want node", got)
		}
		if !strings.Contains(out.String(), "also detected: Cargo.lock (rust)") {
			t.Fatalf("missing also-detected line: %q", out.String())
		}
	})

	t.Run("auto-yes accepts first match without reading stdin", func(t *testing.T) {
		var out bytes.Buffer
		// Pass nil for in to prove auto-yes never reads.
		got, err := promptLockfileStack(
			[]lockfileEntry{node, rust},
			nil, &out, true,
		)
		if err != nil || got != "node" {
			t.Fatalf("got (%q, %v), want (\"node\", nil)", got, err)
		}
		if !strings.Contains(out.String(), "also detected: Cargo.lock (rust)") {
			t.Fatalf("auto-yes should still surface other matches: %q", out.String())
		}
	})

	t.Run("unrecognized response errors", func(t *testing.T) {
		var out bytes.Buffer
		_, err := promptLockfileStack([]lockfileEntry{node}, pipeIn(t, "maybe\n"), &out, false)
		if err == nil {
			t.Fatalf("expected error on unrecognized response")
		}
	})
}
