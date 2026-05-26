//go:build darwin

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/ide"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

// runMacIDE is the Mac-side handler for `ahjo ide <alias>`. Like `ahjo top`,
// IDE detection and launching run on the Mac directly — the apps are
// /Applications bundles and the `ssh-remote+<host>` URI resolves through the
// `~/.ahjo-shared/ssh-config` the in-VM ahjo generates. Relaying into the VM
// (the generic path) would probe the VM's empty PATH and always report "no
// IDEs", so `ide` is special-cased here, parallel to `top`'s macIDEs/macTop.
func runMacIDE(alias string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	slug, err := lookupAlias(filepath.Join(home, ".ahjo-shared", "aliases"), alias)
	if err != nil {
		return err
	}

	slugs := ide.DetectInstalled()
	if len(slugs) == 0 {
		return fmt.Errorf("no SSH-capable IDEs found in /Applications or ~/Applications")
	}

	chosen, err := pickMacIDE(slugs, os.Stdin, os.Stdout)
	if err != nil {
		return err
	}

	host := registry.ContainerName(slug)
	path := paths.RepoMountPath
	if err := ide.LaunchOnHost(chosen, host, path); err != nil {
		return fmt.Errorf("open %s: %w", ide.DisplayName(chosen), err)
	}
	fmt.Fprintf(os.Stdout, "opening %s → %s:%s\n", ide.DisplayName(chosen), host, path)
	return nil
}

// pickMacIDE resolves which detected IDE slug to launch. Mirrors the in-VM
// cli.pickIDE: a single detection is returned without prompting; with several
// it prints a numbered menu and reads a 1-based choice (blank = default 1). On
// non-TTY stdin it errors rather than guessing — there's no safe default IDE.
func pickMacIDE(slugs []string, in *os.File, out io.Writer) (string, error) {
	if len(slugs) == 1 {
		return slugs[0], nil
	}
	if !isTerminal(in) {
		return "", fmt.Errorf("multiple IDEs detected; rerun on a terminal to choose")
	}

	fmt.Fprintln(out, "Pick an IDE to open:")
	for i, slug := range slugs {
		fmt.Fprintf(out, "  %d) %s\n", i+1, ide.DisplayName(slug))
	}
	fmt.Fprintf(out, "Choice [1-%d, default 1]: ", len(slugs))

	line, _ := bufio.NewReader(in).ReadString('\n')
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return slugs[0], nil
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return "", fmt.Errorf("unrecognized choice %q (expected a number 1..%d)", trimmed, len(slugs))
	}
	if n < 1 || n > len(slugs) {
		return "", fmt.Errorf("choice %d out of range [1..%d]", n, len(slugs))
	}
	return slugs[n-1], nil
}
