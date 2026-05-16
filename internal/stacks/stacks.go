// Package stacks ships ahjo's built-in "tech-stack" presets — curated
// ahjocontainer.json configs (node / python / go / rust) the user can
// apply via --stack against a repo that doesn't carry its own
// .ahjo/ahjocontainer.json.
//
// A stack file IS a regular ahjocontainer.json. Same parser, same schema,
// same Feature-application pipeline. The data lives here so each stack
// can be read, diffed, and copy-pasted as-is into a repo if the user
// wants to commit it.
//
// To add a stack: drop <name>/ahjocontainer.json into this directory.
// The go:embed glob picks it up; no other registration step.
package stacks

import (
	"embed"
	"io/fs"
	"sort"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/ahjocontainer"
)

//go:embed */ahjocontainer.json
var stackFS embed.FS

// Load returns the parsed ahjocontainer.Config for the named stack, or
// (nil, false, nil) when name doesn't match a bundled stack. Errors are
// returned only for malformed bundled JSON, which would be a build-time
// regression caught by TestAllStacksLoad.
func Load(name string) (*ahjocontainer.Config, bool, error) {
	if name == "" {
		return nil, false, nil
	}
	b, err := fs.ReadFile(stackFS, name+"/ahjocontainer.json")
	if err != nil {
		return nil, false, nil
	}
	cfg, err := ahjocontainer.Parse(b, "stacks/"+name+"/ahjocontainer.json")
	if err != nil {
		return nil, true, err
	}
	return cfg, true, nil
}

// List returns the names of all bundled stacks in sorted order. Used by
// CLI flag completion, `ahjo doctor`, and the unknown-stack error path.
func List() []string {
	entries, err := fs.ReadDir(stackFS, ".")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Defensive: only include directories that actually contain the
		// embedded config file, so a stray dir wouldn't show up as a
		// fake stack.
		if _, err := fs.Stat(stackFS, e.Name()+"/ahjocontainer.json"); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// FormatList renders the bundled stacks as a comma-separated string for
// inclusion in help text and error messages.
func FormatList() string {
	return strings.Join(List(), ", ")
}
