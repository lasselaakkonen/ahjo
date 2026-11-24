package cli

import (
	"bufio"
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
)

// failReader fails the test if the gate reads input when it must not.
type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Fatal("gate read stdin although no prompt was expected")
	return 0, io.EOF
}

func newGate(flags elevationFlags, interactive bool, in io.Reader, out io.Writer) *elevationGate {
	return &elevationGate{flags: flags, interactive: interactive, in: bufio.NewReader(in), out: out}
}

// confirmers covers both gate methods with the same rule set: each entry
// names the flag that approves it and invokes the method.
var confirmers = []struct {
	name    string
	flags   elevationFlags // the matching flag set
	flagArg string         // the flag name the non-TTY error must cite
	call    func(g *elevationGate) error
}{
	{
		name:    "nesting",
		flags:   elevationFlags{AllowNesting: true},
		flagArg: "--allow-nesting",
		call:    func(g *elevationGate) error { return g.confirmNesting([]string{"ahjo/docker"}) },
	},
	{
		name:    "nested_incus",
		flags:   elevationFlags{AllowNestedIncus: true},
		flagArg: "--allow-nested-incus",
		call:    func(g *elevationGate) error { return g.confirmNestedIncus() },
	},
}

func TestElevationGate_FlagApprovesWithoutReading(t *testing.T) {
	for _, c := range confirmers {
		t.Run(c.name, func(t *testing.T) {
			for _, interactive := range []bool{false, true} {
				var out bytes.Buffer
				g := newGate(c.flags, interactive, failReader{t}, &out)
				if err := c.call(g); err != nil {
					t.Fatalf("interactive=%v: flag should approve: %v", interactive, err)
				}
				if !strings.Contains(out.String(), "approved by "+c.flagArg) {
					t.Fatalf("interactive=%v: output should name the approving flag; got %q", interactive, out.String())
				}
				if strings.Contains(out.String(), "[y/N]") {
					t.Fatalf("interactive=%v: flag approval must not prompt; got %q", interactive, out.String())
				}
			}
		})
	}
}

func TestElevationGate_TTYYesApproves(t *testing.T) {
	for _, c := range confirmers {
		for _, ans := range []string{"y\n", "Y\n", "yes\n", "  YES  \n", "y"} {
			t.Run(c.name+"/"+strings.TrimSpace(ans), func(t *testing.T) {
				var out bytes.Buffer
				g := newGate(elevationFlags{}, true, strings.NewReader(ans), &out)
				if err := c.call(g); err != nil {
					t.Fatalf("answer %q should approve: %v", ans, err)
				}
				if !strings.Contains(out.String(), "Allow? [y/N] ") {
					t.Fatalf("expected prompt; got %q", out.String())
				}
			})
		}
	}
}

func TestElevationGate_TTYDeclines(t *testing.T) {
	for _, c := range confirmers {
		for name, ans := range map[string]string{"n": "n\n", "no": "no\n", "empty": "\n", "EOF": "", "other": "sure\n"} {
			t.Run(c.name+"/"+name, func(t *testing.T) {
				var out bytes.Buffer
				g := newGate(elevationFlags{}, true, strings.NewReader(ans), &out)
				err := c.call(g)
				if err == nil {
					t.Fatalf("answer %q must decline", ans)
				}
				if !strings.Contains(err.Error(), "declined") {
					t.Fatalf("error should say declined; got %v", err)
				}
				if g.nestingOK {
					t.Fatal("decline must not mark nesting approved")
				}
			})
		}
	}
}

func TestElevationGate_NonTTYWithoutFlagErrors(t *testing.T) {
	for _, c := range confirmers {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			g := newGate(elevationFlags{}, false, failReader{t}, &out)
			err := c.call(g)
			if err == nil {
				t.Fatal("non-TTY without flag must error")
			}
			if !strings.Contains(err.Error(), c.flagArg) {
				t.Fatalf("error should name %s; got %v", c.flagArg, err)
			}
			if strings.Contains(out.String(), "[y/N]") {
				t.Fatalf("non-TTY must not prompt; got %q", out.String())
			}
		})
	}
}

func TestElevationGate_NestingFlagDoesNotApproveNestedIncus(t *testing.T) {
	g := newGate(elevationFlags{AllowNesting: true}, false, failReader{t}, &bytes.Buffer{})
	err := g.confirmNestedIncus()
	if err == nil || !strings.Contains(err.Error(), "--allow-nested-incus") {
		t.Fatalf("--allow-nesting must not approve nested_incus; err = %v", err)
	}
}

func TestElevationGate_NestedIncusFlagImpliesNesting(t *testing.T) {
	// --allow-nested-incus alone approves nesting even when confirmNesting
	// runs first (no prior confirmNestedIncus call).
	var out bytes.Buffer
	g := newGate(elevationFlags{AllowNestedIncus: true}, false, failReader{t}, &out)
	if err := g.confirmNesting([]string{"ahjo/docker"}); err != nil {
		t.Fatalf("--allow-nested-incus should imply --allow-nesting: %v", err)
	}
	if !strings.Contains(out.String(), "approved by --allow-nested-incus") {
		t.Fatalf("output should name the approving flag; got %q", out.String())
	}
}

func TestElevationGate_NestedIncusApprovalCoversNesting(t *testing.T) {
	// One "y" for nested_incus; the following confirmNesting must not
	// prompt again.
	var out bytes.Buffer
	g := newGate(elevationFlags{}, true, strings.NewReader("y\n"), &out)
	if err := g.confirmNestedIncus(); err != nil {
		t.Fatalf("confirmNestedIncus: %v", err)
	}
	before := out.Len()
	if err := g.confirmNesting([]string{"ahjo/docker"}); err != nil {
		t.Fatalf("confirmNesting after nested_incus approval: %v", err)
	}
	if out.Len() != before {
		t.Fatalf("confirmNesting must not prompt after nested_incus approval; extra output %q", out.String()[before:])
	}
}

func TestElevationGate_PromptTextNamesRequesters(t *testing.T) {
	var out bytes.Buffer
	g := newGate(elevationFlags{}, true, strings.NewReader("n\n"), &out)
	err := g.confirmNesting([]string{"ahjo/a", "ahjo/b"})
	if err == nil {
		t.Fatal("expected decline")
	}
	if !strings.Contains(out.String(), "needed by: ahjo/a, ahjo/b") {
		t.Fatalf("prompt should list requesting Features; got %q", out.String())
	}
	if !strings.Contains(err.Error(), "ahjo/a, ahjo/b") {
		t.Fatalf("decline error should name the Features; got %v", err)
	}
}

func TestElevationGate_SharedReaderLeavesLaterInput(t *testing.T) {
	// The gate and the Feature trust prompt share one *bufio.Reader; the
	// gate must consume exactly one line so the trust prompt still sees
	// its answer.
	r := bufio.NewReader(strings.NewReader("y\ny\n"))
	g := &elevationGate{interactive: true, in: r, out: &bytes.Buffer{}}
	if err := g.confirmNesting([]string{"ahjo/docker"}); err != nil {
		t.Fatalf("confirmNesting: %v", err)
	}
	if bufio.NewReader(r) != r {
		t.Fatal("bufio.NewReader must return the shared reader unchanged")
	}
	rest, _ := r.ReadString('\n')
	if rest != "y\n" {
		t.Fatalf("remaining input = %q, want %q", rest, "y\n")
	}
}

func TestElevationFlags_YesIsNotAnInput(t *testing.T) {
	// --yes must never approve an elevation. Guard that structurally: the
	// gate's flag set holds exactly the two dedicated opt-ins.
	typ := reflect.TypeOf(elevationFlags{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	if want := []string{"AllowNesting", "AllowNestedIncus"}; !equalStrings(got, want) {
		t.Fatalf("elevationFlags fields = %v, want %v (do not route --yes into the gate)", got, want)
	}
}
