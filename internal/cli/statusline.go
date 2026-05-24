package cli

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"

	"github.com/lasselaakkonen/ahjo/internal/incus"
)

// statuslineScript is the bash+jq statusline ahjo installs into the container.
// It renders git-dir state / branch / active bridges, reading live bridge state
// from ~/.ahjo/ahjo-state.json.
//
//go:embed statusline.sh
var statuslineScript string

const (
	// The script lives in ~/.claude (Claude's own dir, wired from settings.json),
	// not under ~/.ahjo with the state snapshots it reads.
	ahjoStatuslinePath = claudeDirPath + "/ahjo-statusline.sh"
	claudeSettingsPath = claudeDirPath + "/settings.json"
)

// installClaudeStatusline drops the statusline script into the container and,
// unless the user already configured a statusLine of their own, points Claude's
// settings.json at it. Called at base-container creation from pushClaudeConfig;
// COW branch clones inherit ~/.claude, so this runs once. Best-effort by
// contract — the caller only warns on failure, never fails container creation.
func installClaudeStatusline(containerName, hostHome string) error {
	if err := pushContainerFile(containerName, statuslineScript, ahjoStatuslinePath); err != nil {
		return fmt.Errorf("push statusline script: %w", err)
	}
	if err := incus.ExecAs(containerName, 0, nil, "/", "chmod", "0755", ahjoStatuslinePath); err != nil {
		return fmt.Errorf("chmod statusline script: %w", err)
	}

	// A missing/unreadable host settings.json reads as empty; mergeStatusLineSetting
	// then produces a minimal file carrying just our statusLine.
	raw, _ := os.ReadFile(hostHome + "/.claude/settings.json")
	merged, changed, err := mergeStatusLineSetting(raw)
	if err != nil {
		return fmt.Errorf("merge statusLine into settings.json: %w", err)
	}
	if !changed {
		return nil // user has their own statusLine; leave the verbatim copy untouched
	}
	return pushContainerFile(containerName, string(merged), claudeSettingsPath)
}

// mergeStatusLineSetting adds ahjo's statusLine to a settings.json body unless one
// is already present. Pure (no I/O) so it stays unit-testable, mirroring the
// read-modify-write in init.go's mergeClaudeOnboardingMarker. Empty input is
// treated as an empty object so a container whose user has no settings.json still
// gets a minimal one. Returns changed=false when the user already has a statusLine
// so the caller leaves their config alone; malformed JSON errors rather than
// clobbering a file that's merely broken.
func mergeStatusLineSetting(raw []byte) (merged []byte, changed bool, err error) {
	d := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, false, err
		}
	}
	if _, exists := d["statusLine"]; exists {
		return nil, false, nil
	}
	d["statusLine"] = map[string]any{
		"type":    "command",
		"command": ahjoStatuslinePath,
		"padding": 0,
	}
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(out, '\n'), true, nil
}
