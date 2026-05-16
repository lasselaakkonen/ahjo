package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/ahjocontainer"
	"github.com/lasselaakkonen/ahjo/internal/stacks"
)

// containerConfigFlagShort is the one-line help shown next to --container-config
// in `--help` output. The long-form `containerConfigHelpBlock` (below) carries
// the resolution rules; this stays terse so the flag list reads cleanly.
const containerConfigFlagShort = "container config to apply when the repo has no .ahjo/ahjocontainer.json " +
	"(or to override it): a bundled stack name, a repo-local .ahjo/<name>.json basename, " +
	"a host path to a .json file, or \"bare\" for no toolchain"

// containerConfigHelpBlock is appended to the Long descriptions of `repo add`
// and `claude` so users see the full resolution rules in one place. Kept
// here so the two commands stay in sync.
var containerConfigHelpBlock = "Container config resolution (in order):\n" +
	"  1. --container-config <value> if set — overrides the in-repo file.\n" +
	"  2. Otherwise, .ahjo/ahjocontainer.json in the repo if present.\n" +
	"  3. Otherwise, interactive picker on a TTY (bare + repo .ahjo/*.json +\n" +
	"     bundled stacks: " + stacks.FormatList() + ").\n" +
	"  4. Otherwise (non-TTY), bare — no toolchain beyond ahjo-base.\n\n" +
	"--container-config <value> accepts:\n" +
	"  • a bundled stack name (" + stacks.FormatList() + ")\n" +
	"  • a repo-local basename, resolved against .ahjo/<value>.json in the repo\n" +
	"  • an absolute or relative path to a .json file on the host\n" +
	"  • the literal \"bare\" for no toolchain (same as the picker's bare option)"

// bareConfigName is the reserved keyword for "apply no container config".
// Same outcome as today's flagless no-in-repo-config path; named so users
// can request it explicitly from CLI or pick it interactively.
const bareConfigName = "bare"

// configIdentifierPattern is the shape accepted as a "name" (built-in
// stack or repo-local basename). Anything outside this pattern is treated
// as a host path. Keeps the resolver from interpreting e.g. "./foo" or
// "node-22" inconsistently.
var configIdentifierPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// resolveContainerConfig turns the user's --container-config value into a
// concrete *ahjocontainer.Config. Resolution order, first match wins:
//
//  1. ""        → caller decides (in-repo canonical / picker / bare).
//  2. "bare"    → no config (nil).
//  3. path-like → read as a JSON file on the host (LoadFromHost).
//  4. identifier (`[A-Za-z0-9_-]+`):
//     a. /repo/.ahjo/<value>.json inside containerName
//     b. bundled stack with that name
//  5. otherwise → error.
//
// The "" return signal is reserved for ref == "" only; every other input
// either resolves to a *Config or returns an error.
func resolveContainerConfig(containerName, ref string) (*ahjocontainer.Config, bool, error) {
	if ref == "" {
		return nil, false, nil
	}
	if ref == bareConfigName {
		// Explicit bare. Treated as "resolved to nothing" — caller skips
		// feature/lifecycle application just like with no in-repo config.
		return nil, true, nil
	}

	if isPathLike(ref) {
		abs, err := filepath.Abs(ref)
		if err != nil {
			return nil, false, fmt.Errorf("--container-config %q: %w", ref, err)
		}
		dir, file := filepath.Split(abs)
		// LoadFromHost expects the repo-dir + ConfigPath shape. We're not
		// using ConfigPath here, so call the file reader directly via a
		// minimal re-parse — LoadFromHost is shaped for the canonical
		// .ahjo/ahjocontainer.json layout.
		b, err := os.ReadFile(abs)
		if err != nil {
			return nil, false, fmt.Errorf("--container-config %q: %w", ref, err)
		}
		cfg, err := ahjocontainer.Parse(b, filepath.Join(filepath.Base(strings.TrimSuffix(dir, string(filepath.Separator))), file))
		if err != nil {
			return nil, false, err
		}
		return cfg, true, nil
	}

	if !configIdentifierPattern.MatchString(ref) {
		return nil, false, fmt.Errorf("--container-config %q: not a valid identifier (use [A-Za-z0-9_-]+) and not a path to an existing .json file", ref)
	}

	// Identifier: try repo-local .ahjo/<ref>.json first (repos can override
	// a built-in stack name by shipping their own file with the same name).
	if cfg, found, err := ahjocontainer.LoadAlternateFromContainer(containerName, ref); err != nil {
		return nil, false, err
	} else if found {
		fmt.Printf("→ applying repo's .ahjo/%s.json\n", ref)
		return cfg, true, nil
	}

	if cfg, found, err := stacks.Load(ref); err != nil {
		return nil, false, fmt.Errorf("load bundled stack %q: %w", ref, err)
	} else if found {
		fmt.Printf("→ applying ahjo built-in %q stack\n", ref)
		return cfg, true, nil
	}

	return nil, false, fmt.Errorf("--container-config %q: not a path, not .ahjo/%s.json in the repo, and not a bundled stack (available: %s)",
		ref, ref, stacks.FormatList())
}

// isPathLike returns true when ref clearly points at a filesystem path
// rather than a bare identifier — host-path heuristic before the
// identifier-vs-stack lookup. Kept conservative: anything with a
// separator, a leading "./" / "../" / "~/", an absolute prefix, or a
// ".json" suffix is treated as a path.
func isPathLike(ref string) bool {
	switch {
	case strings.ContainsRune(ref, filepath.Separator):
		return true
	case strings.HasPrefix(ref, "./"), strings.HasPrefix(ref, "../"):
		return true
	case strings.HasPrefix(ref, "~/"):
		return true
	case filepath.IsAbs(ref):
		return true
	case strings.HasSuffix(ref, ".json"):
		return true
	}
	return false
}

// promptContainerConfig is the TTY picker shown when no --container-config
// flag was passed and the repo carries no .ahjo/ahjocontainer.json.
// Returns the user's chosen identifier (a repo alternate name, a bundled
// stack name, or "bare"). On non-TTY stdin it returns ("bare", nil) so
// scripted invocations keep working without prompting.
func promptContainerConfig(containerName string, in *os.File, out io.Writer) (string, error) {
	if !isTerminal(in) {
		return bareConfigName, nil
	}

	alts, err := ahjocontainer.ListAlternatesInContainer(containerName)
	if err != nil {
		return "", err
	}
	sort.Strings(alts)
	bundled := stacks.List()

	type choice struct {
		name string
		hint string
	}
	choices := make([]choice, 0, 1+len(alts)+len(bundled))
	choices = append(choices, choice{bareConfigName, "no toolchain (ahjo base image only)"})
	for _, a := range alts {
		choices = append(choices, choice{a, "repo-local .ahjo/" + a + ".json"})
	}
	for _, s := range bundled {
		choices = append(choices, choice{s, "ahjo built-in stack"})
	}

	fmt.Fprintln(out, "No .ahjo/ahjocontainer.json in this repo. Pick a base container config:")
	for i, c := range choices {
		fmt.Fprintf(out, "  %d) %-12s — %s\n", i+1, c.name, c.hint)
	}
	fmt.Fprintf(out, "Choice [1-%d, default 1]: ", len(choices))

	// EOF on stdin (e.g. closed pipe) is treated as "accept default" —
	// matches promptStartingModel's read-failure semantics so piped
	// invocations never hang or error from the picker.
	line, _ := bufio.NewReader(in).ReadString('\n')
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return choices[0].name, nil
	}

	// Accept either a number or the choice name verbatim.
	if n, convErr := strconv.Atoi(trimmed); convErr == nil {
		if n < 1 || n > len(choices) {
			return "", fmt.Errorf("choice %d out of range [1..%d]", n, len(choices))
		}
		return choices[n-1].name, nil
	}
	for _, c := range choices {
		if c.name == trimmed {
			return c.name, nil
		}
	}
	return "", fmt.Errorf("unrecognized choice %q (expected a number 1..%d or one of the listed names)", trimmed, len(choices))
}
