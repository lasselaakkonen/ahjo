//go:build darwin

package main

import (
	"bytes"
	"os"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/ide"
)

// pipeReader returns the read end of an os.Pipe primed with s, plus a cleanup.
// The read end is a non-terminal *os.File, so it exercises pickMacIDE's
// non-TTY branches (single slug, parse, range) deterministically.
func pipeReader(t *testing.T, s string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString(s); err != nil {
		t.Fatalf("write pipe: %v", err)
	}
	w.Close()
	t.Cleanup(func() { r.Close() })
	return r
}

func TestPickMacIDE_SingleSkipsPrompt(t *testing.T) {
	// A lone detection returns without reading stdin or prompting.
	var out bytes.Buffer
	got, err := pickMacIDE([]string{ide.Cursor}, pipeReader(t, ""), &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != ide.Cursor {
		t.Fatalf("got %q, want %q", got, ide.Cursor)
	}
	if out.Len() != 0 {
		t.Fatalf("expected no prompt for a single IDE, got %q", out.String())
	}
}

func TestPickMacIDE_NonTTYMultipleErrs(t *testing.T) {
	// Several detections on non-TTY stdin must error, not guess. (pickMacIDE
	// checks isTerminal before reading, so the parse/range branches below it
	// are reachable only on a real TTY and aren't exercised here.)
	_, err := pickMacIDE([]string{ide.Cursor, ide.VSCode}, pipeReader(t, ""), &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected an error for multiple IDEs on non-TTY stdin")
	}
}
