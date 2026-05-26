package top

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// newTestModel returns a concrete *model with empty Deps. The paste tests
// only exercise input routing, which needs no live host/registry wiring.
func newTestModel(t *testing.T) *model {
	t.Helper()
	m, ok := New(Deps{}).(*model)
	if !ok {
		t.Fatal("New did not return *model")
	}
	return m
}

// Bracketed paste arrives as a tea.PasteMsg, not a run of KeyPressMsgs, so it
// must be routed into the focused text prompt or the content is silently
// dropped. This is the bug behind "can't paste into the mirror-target prompt".
func TestPasteRoutedToTextPrompt(t *testing.T) {
	m := newTestModel(t)
	m.startInput(inputAddRepo) // no precondition, drives the real input flow

	updated, _ := m.Update(tea.PasteMsg{Content: "owner/repo"})
	got := updated.(*model).input.Value()

	if got != "owner/repo" {
		t.Fatalf("pasted text not inserted: input value = %q, want %q", got, "owner/repo")
	}
}

// Paste should append at the cursor, not replace, so a paste following typed
// text composes the way a user expects.
func TestPasteAppendsAtCursor(t *testing.T) {
	m := newTestModel(t)
	m.startInput(inputAddRepo)
	m.input.SetValue("owner/")
	m.input.CursorEnd()

	updated, _ := m.Update(tea.PasteMsg{Content: "repo"})
	if got := updated.(*model).input.Value(); got != "owner/repo" {
		t.Fatalf("paste did not append at cursor: input value = %q, want %q", got, "owner/repo")
	}
}

// Outside a text prompt there's nowhere for pasted text to land (the list
// panes have filtering disabled), so paste is dropped without touching the
// hidden input buffer or panicking.
func TestPasteDroppedOutsideTextPrompt(t *testing.T) {
	m := newTestModel(t)
	// inputNone by default; also covers the list-style picker modes below.
	updated, cmd := m.Update(tea.PasteMsg{Content: "ignored"})
	if v := updated.(*model).input.Value(); v != "" {
		t.Fatalf("paste leaked into input outside a prompt: %q", v)
	}
	if cmd != nil {
		t.Fatalf("paste outside a prompt should issue no command, got %v", cmd)
	}
}

// textInputActive must be true exactly for the m.input-backed prompts and
// false for inputNone and the list-style pickers, since that predicate gates
// paste routing.
func TestTextInputActive(t *testing.T) {
	m := newTestModel(t)
	cases := map[inputMode]bool{
		inputNone:         false,
		inputAddRepo:      true,
		inputNewContainer: true,
		inputMirrorTarget: true,
		inputForward:      true,
		inputIDE:          false,
		inputRunTarget:    false,
	}
	for mode, want := range cases {
		m.inputMode = mode
		if got := m.textInputActive(); got != want {
			t.Errorf("textInputActive() for mode %d = %v, want %v", mode, got, want)
		}
	}
}
