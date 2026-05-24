package top

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// persistedSelection is the slice of TUI state we carry across runs so
// reopening `ahjo top` lands on the same panel and row the user left. Keyed
// by stable identifiers (repo name, branch slug) rather than list indices,
// since the rows themselves shift as repos and branches come and go.
type persistedSelection struct {
	Focus  string `json:"focus"`            // "repos" | "containers" | "details"
	Repo   string `json:"repo,omitempty"`   // registry.Repo.Name of the highlighted repo
	Branch string `json:"branch,omitempty"` // registry.Branch.Slug of the highlighted container
}

// loadSelection reads the persisted selection. A missing file is the first-run
// case, not an error: it returns (nil, nil). Any other read/parse failure is
// returned so the caller can fall back to defaults.
func loadSelection() (*persistedSelection, error) {
	b, err := os.ReadFile(paths.TopSelectionPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sel persistedSelection
	if err := json.Unmarshal(b, &sel); err != nil {
		return nil, err
	}
	return &sel, nil
}

// save writes the selection atomically (tempfile + rename) under ~/.ahjo,
// mirroring config.Save. Persisting the selection is a convenience, never
// load-bearing, so callers ignore the returned error.
func (s persistedSelection) save() error {
	dir := paths.AhjoDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "top-selection-*.json.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), paths.TopSelectionPath())
}
