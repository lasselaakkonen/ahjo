package devcontainer

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeLifecycle_StringForm(t *testing.T) {
	steps, err := decodeLifecycle(json.RawMessage(`"echo hi"`))
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("want 1 step; got %d", len(steps))
	}
	got := strings.Join(steps[0].argv, " ")
	if got != "bash -c echo hi" {
		t.Fatalf("argv = %q, want %q", got, "bash -c echo hi")
	}
	if steps[0].label != "" {
		t.Fatalf("string form should have empty label; got %q", steps[0].label)
	}
}

func TestDecodeLifecycle_ArrayForm(t *testing.T) {
	steps, err := decodeLifecycle(json.RawMessage(`["echo","hi"]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("want 1 step; got %d", len(steps))
	}
	if got := strings.Join(steps[0].argv, " "); got != "echo hi" {
		t.Fatalf("argv = %q, want %q", got, "echo hi")
	}
}

func TestDecodeLifecycle_ObjectForm_StableOrder(t *testing.T) {
	body := `{
		"first": "echo one",
		"second": ["echo", "two"]
	}`
	steps, err := decodeLifecycle(json.RawMessage(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("want 2 steps; got %d (%+v)", len(steps), steps)
	}
	if steps[0].label != "(first)" {
		t.Fatalf("first label = %q, want (first)", steps[0].label)
	}
	if got := strings.Join(steps[0].argv, " "); got != "bash -c echo one" {
		t.Fatalf("first argv = %q", got)
	}
	if steps[1].label != "(second)" {
		t.Fatalf("second label = %q, want (second)", steps[1].label)
	}
	if got := strings.Join(steps[1].argv, " "); got != "echo two" {
		t.Fatalf("second argv = %q", got)
	}
}

func TestDecodeLifecycle_NestedObject(t *testing.T) {
	body := `{"outer": {"inner": "echo nested"}}`
	steps, err := decodeLifecycle(json.RawMessage(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("want 1 step; got %d", len(steps))
	}
	if steps[0].label != "(outer/inner)" {
		t.Fatalf("nested label = %q", steps[0].label)
	}
}

func TestDecodeLifecycle_AbsentRawIsNoop(t *testing.T) {
	for _, raw := range []string{"", "null", `""`, "[]", "{}"} {
		t.Run(raw, func(t *testing.T) {
			steps, err := decodeLifecycle(json.RawMessage(raw))
			if err != nil {
				t.Fatalf("absent raw should be no-op; got %v", err)
			}
			if len(steps) != 0 {
				t.Fatalf("absent raw should produce no steps; got %d", len(steps))
			}
		})
	}
}

func TestDecodeLifecycle_RejectsBogusForm(t *testing.T) {
	steps, err := decodeLifecycle(json.RawMessage(`42`))
	if err == nil {
		t.Fatalf("expected error rejecting numeric form; got %d steps", len(steps))
	}
}
