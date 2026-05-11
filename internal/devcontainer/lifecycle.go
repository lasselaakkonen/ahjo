package devcontainer

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/lasselaakkonen/ahjo/internal/incus"
)

// LifecycleStage names the spec-defined hook a command was declared under.
// Used for log lines and error messages so a failing onCreateCommand can be
// distinguished from a failing postCreateCommand without re-deriving the
// caller.
type LifecycleStage string

const (
	StageOnCreate   LifecycleStage = "onCreateCommand"
	StagePostCreate LifecycleStage = "postCreateCommand"
	StagePostStart  LifecycleStage = "postStartCommand"
	StagePostAttach LifecycleStage = "postAttachCommand"
)

// RunLifecycle renders raw into one or more (label, argv) pairs per the
// devcontainer spec's three forms (string / array / object) and runs each
// inside container as uid in cwd, sequentially. A failed step aborts the
// chain — the caller decides whether to stop the rest of the repo-add /
// shell-prep flow.
//
// Forms:
//   - string  : "echo hi"          → bash -lc "echo hi"
//   - array   : ["echo", "hi"]     → echo hi (no shell)
//   - object  : {"a": "...", ...}  → each entry sequentially in stable key
//     order. The spec calls for parallel execution; ahjo runs sequentially
//     (per the design doc's `waitFor` notes) — predictable, simpler, and
//     no real-world Feature has needed parallelism.
//
// raw of zero length / null is a no-op (the field wasn't set).
func RunLifecycle(container string, stage LifecycleStage, raw json.RawMessage, uid int, env map[string]string, cwd string, out io.Writer) error {
	steps, err := decodeLifecycle(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", stage, err)
	}
	for _, s := range steps {
		fmt.Fprintf(out, "→ %s%s: %s\n", stage, s.label, s.display())
		if err := incus.ExecAs(container, uid, env, cwd, s.argv...); err != nil {
			return fmt.Errorf("%s%s: %w", stage, s.label, err)
		}
	}
	return nil
}

// step is one renderable command derived from a LifecycleCmd. label is
// non-empty only for the object form ("(<key>)"), so the string and array
// forms produce the bare stage name in logs.
type step struct {
	label string
	argv  []string
}

func (s step) display() string {
	if len(s.argv) == 0 {
		return ""
	}
	if len(s.argv) >= 3 && s.argv[0] == "bash" && (s.argv[1] == "-c" || s.argv[1] == "-lc") {
		return s.argv[2]
	}
	out := s.argv[0]
	for _, a := range s.argv[1:] {
		out += " " + a
	}
	return out
}

// decodeLifecycle parses the spec's three forms into (label, argv) steps.
// An absent / null raw produces zero steps.
func decodeLifecycle(raw json.RawMessage) ([]step, error) {
	if !rawJSONHasValue(raw) {
		return nil, nil
	}

	// String form: run via bash -lc so users get shell features (pipes,
	// env-var expansion, &&-chains) without quoting back through ahjo, AND
	// so /etc/profile.d/*.sh load — devcontainer Features (Go, Rust, nvm,
	// …) drop PATH-extending scripts there, and a plain `bash -c` skips
	// them. Matches devcontainers/cli behavior.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []step{{argv: []string{"bash", "-lc", s}}}, nil
	}

	// Array form: argv[0] is the program; ahjo runs it directly through
	// `incus exec` without a shell. Spec convention.
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		if len(arr) == 0 {
			return nil, nil
		}
		return []step{{argv: arr}}, nil
	}

	// Object form: each value is itself a string or array (the spec
	// allows nested shapes). Sequential by sorted key for determinism.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("expected string, array, or object form; got %s", string(raw))
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var steps []step
	for _, k := range keys {
		sub, err := decodeLifecycle(obj[k])
		if err != nil {
			return nil, fmt.Errorf("entry %q: %w", k, err)
		}
		// Lift the entry's first step's argv under its key as the label.
		// Nested objects flatten into multiple labeled steps so a
		// {"a": {"x": "...", "y": "..."}} runs as a/x then a/y.
		for _, ss := range sub {
			label := "(" + k + ")"
			if ss.label != "" {
				label = "(" + k + "/" + trimParens(ss.label) + ")"
			}
			steps = append(steps, step{label: label, argv: ss.argv})
		}
	}
	return steps, nil
}

func trimParens(s string) string {
	if len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		return s[1 : len(s)-1]
	}
	return s
}
