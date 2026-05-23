package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/ide"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	"github.com/lasselaakkonen/ahjo/internal/tui/top"
)

func newIDECmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ide <alias>",
		Short: "Open an SSH-capable IDE on the branch's container",
		Long: `Detect the SSH-capable IDEs installed on the host (Cursor, VS Code,
VS Code Insiders, Windsurf, Zed) and open the chosen one against the branch's
container over ssh-remote — the same detection + launch the ` + "`i`" + ` picker in
` + "`ahjo top`" + ` uses.

A lone detected IDE opens directly; with several, ahjo prompts for a choice on
a terminal. The container must be running for the IDE's SSH connection to land
(` + "`ahjo shell <alias>`" + ` starts it).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runIDE(args[0])
		},
	}
}

func runIDE(alias string) error {
	reg, err := registry.Load()
	if err != nil {
		return err
	}
	br := reg.FindBranchByAlias(alias)
	if br == nil {
		return fmt.Errorf("no branch with alias %q", alias)
	}
	if br.Slug == "" {
		return fmt.Errorf("registry row for %q has no slug; recreate with `ahjo rm %s && ahjo create`", alias, alias)
	}

	ides := idesForTop()
	if len(ides) == 0 {
		return fmt.Errorf("no SSH-capable IDEs found on PATH")
	}

	chosen, err := pickIDE(ides, os.Stdin, cobraOut())
	if err != nil {
		return err
	}
	if chosen.Open == nil {
		return fmt.Errorf("ide %s: no launcher", chosen.Name)
	}

	host := registry.ContainerName(br.Slug)
	path := paths.RepoMountPath
	if err := chosen.Open(host, path); err != nil {
		return fmt.Errorf("open %s: %w", chosen.Name, err)
	}
	fmt.Fprintf(cobraOut(), "opening %s → %s:%s\n", chosen.Name, host, path)
	return nil
}

// pickIDE resolves which detected IDE to launch. A single detection is
// returned without prompting; with several it prints a numbered menu and
// reads a 1-based choice (blank line = default 1). On non-TTY stdin it errors
// instead of guessing — unlike the container-config picker there's no safe
// default IDE to fall back to.
func pickIDE(ides []top.IDE, in *os.File, out io.Writer) (top.IDE, error) {
	if len(ides) == 1 {
		return ides[0], nil
	}
	if !isTerminal(in) {
		return top.IDE{}, fmt.Errorf("multiple IDEs detected; rerun on a terminal to choose")
	}

	fmt.Fprintln(out, "Pick an IDE to open:")
	for i, e := range ides {
		fmt.Fprintf(out, "  %d) %s\n", i+1, e.Name)
	}
	fmt.Fprintf(out, "Choice [1-%d, default 1]: ", len(ides))

	line, _ := bufio.NewReader(in).ReadString('\n')
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ides[0], nil
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return top.IDE{}, fmt.Errorf("unrecognized choice %q (expected a number 1..%d)", trimmed, len(ides))
	}
	if n < 1 || n > len(ides) {
		return top.IDE{}, fmt.Errorf("choice %d out of range [1..%d]", n, len(ides))
	}
	return ides[n-1], nil
}

// idesForTop is the Deps hook that powers the `i` picker in `ahjo top` and
// backs the `ahjo ide` command. Bare-Linux only: probes PATH via internal/ide
// and returns picker entries whose Open invokes the local CLI shim. On the
// Mac, the shim hands the TUI its own platform-specific Deps and this function
// is never called.
func idesForTop() []top.IDE {
	slugs := ide.DetectInstalled()
	out := make([]top.IDE, 0, len(slugs))
	for _, slug := range slugs {
		slug := slug
		out = append(out, top.IDE{
			Name: ide.DisplayName(slug),
			Open: func(host, path string) error {
				return ide.LaunchOnHost(slug, host, path)
			},
		})
	}
	return out
}
