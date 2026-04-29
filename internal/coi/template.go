package coi

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

//go:embed all:assets
var assets embed.FS

// TemplateData feeds the .coi/config.toml template per worktree.
type TemplateData struct {
	Image       string
	Slug        string
	HostKeysDir string
	ForwardEnv  []string
}

// RenderConfig renders .coi/config.toml into worktreeDir/.coi/config.toml.
func RenderConfig(worktreeDir string, data TemplateData) error {
	tmplBytes, err := assets.ReadFile("assets/templates/coi-config-template.toml")
	if err != nil {
		return fmt.Errorf("read embedded template: %w", err)
	}
	t, err := template.New("coi-config").Parse(string(tmplBytes))
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	coiDir := filepath.Join(worktreeDir, ".coi")
	if err := os.MkdirAll(coiDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", coiDir, err)
	}
	dst := filepath.Join(coiDir, "config.toml")
	tmp, err := os.CreateTemp(coiDir, ".config.toml.tmp.*")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := t.Execute(tmp, data); err != nil {
		tmp.Close()
		return fmt.Errorf("execute template: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// AhjoBaseAssets returns the embedded ahjo-base profile files (config.toml, build.sh).
func AhjoBaseAssets() (configTOML, buildSh []byte, err error) {
	configTOML, err = assets.ReadFile("assets/profiles/ahjo-base/config.toml")
	if err != nil {
		return nil, nil, err
	}
	buildSh, err = assets.ReadFile("assets/profiles/ahjo-base/build.sh")
	if err != nil {
		return nil, nil, err
	}
	return configTOML, buildSh, nil
}
