package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// elevationFlags carries the dedicated opt-in flags from the command line.
// --yes is deliberately not one of them: skipping the PAT prompt must never
// also approve a container privilege bump.
type elevationFlags struct {
	AllowNesting     bool // --allow-nesting
	AllowNestedIncus bool // --allow-nested-incus (implies nesting)
}

// allowsNesting reports whether the flags pre-approve security.nesting.
// --allow-nested-incus implies --allow-nesting: nested Incus is useless
// without nesting.
func (f elevationFlags) allowsNesting() bool {
	return f.AllowNesting || f.AllowNestedIncus
}

// elevationGate decides, once per repo add, whether the repo may get
// security.nesting and host loop devices. Repo content (including
// .ahjo/ahjocontainer.json) is untrusted, so neither elevation may be
// granted on the repo author's say-so alone: it needs a dedicated flag or
// an explicit "yes" on a terminal.
//
// Rules, identical for both confirm methods:
//  1. Matching flag set → approved; one line names the flag.
//  2. Else on a TTY → explain what is granted, then `Allow? [y/N] `. Only
//     y / yes (case-insensitive, trimmed) approves; anything else,
//     including EOF, is a decline and returns an error.
//  3. Else (non-TTY) → error naming the flag to pass.
type elevationGate struct {
	flags       elevationFlags
	interactive bool          // stdin is a TTY
	in          *bufio.Reader // shared with the Feature trust prompt
	out         io.Writer
	nestingOK   bool // memo: nesting already approved
}

// confirmNestedIncus gates customizations.ahjo.nested_incus (host loop
// devices + security.nesting). Approval also marks nesting as approved, so
// a following confirmNesting does not prompt a second time.
func (g *elevationGate) confirmNestedIncus() error {
	if g.flags.AllowNestedIncus {
		fmt.Fprintln(g.out, "→ nested_incus approved by --allow-nested-incus")
		g.nestingOK = true
		return nil
	}
	if !g.interactive {
		return errors.New("customizations.ahjo.nested_incus requires confirmation; re-run on a terminal or pass --allow-nested-incus")
	}
	fmt.Fprintln(g.out, "This repo sets customizations.ahjo.nested_incus. ahjo will pass host loop "+
		"devices (/dev/loop-control and the currently unused /dev/loopN in 0..7) into the "+
		"container and enable security.nesting. Code in the container can then mount "+
		"arbitrary block filesystems with the host kernel, and can read and write anything "+
		"the host later attaches to one of those loop devices while the container runs.")
	if !g.ask() {
		return errors.New("nested_incus declined; remove it from .ahjo/ahjocontainer.json or re-run and approve")
	}
	g.nestingOK = true
	return nil
}

// confirmNesting gates security.nesting requested by built-in Features.
// requestedBy lists the `ahjo/<name>` refs asking for it; those names come
// from the embedded built-in registry, so they are safe to print. Returns
// nil without prompting when nesting was already approved in this run.
func (g *elevationGate) confirmNesting(requestedBy []string) error {
	if g.nestingOK {
		return nil
	}
	who := strings.Join(requestedBy, ", ")
	if g.flags.allowsNesting() {
		flag := "--allow-nesting"
		if !g.flags.AllowNesting {
			flag = "--allow-nested-incus"
		}
		fmt.Fprintf(g.out, "→ security.nesting (needed by: %s) approved by %s\n", who, flag)
		g.nestingOK = true
		return nil
	}
	if !g.interactive {
		return fmt.Errorf("security.nesting (needed by: %s) requires confirmation; re-run on a terminal or pass --allow-nesting", who)
	}
	fmt.Fprintf(g.out, "This repo requests security.nesting (needed by: %s). It lets code in the "+
		"container create nested user namespaces and mount overlayfs, which widens the "+
		"kernel attack surface of the Incus host.\n", who)
	if !g.ask() {
		return fmt.Errorf("security.nesting declined (needed by: %s); remove %s from the container config or re-run and approve", who, who)
	}
	g.nestingOK = true
	return nil
}

// ask prints the y/N prompt and reads one line. Only y / yes approves; a
// read error (EOF, closed stdin) or a nil reader is a decline.
func (g *elevationGate) ask() bool {
	fmt.Fprint(g.out, "Allow? [y/N] ")
	if g.in == nil {
		fmt.Fprintln(g.out)
		return false
	}
	line, err := g.in.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(g.out)
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}
