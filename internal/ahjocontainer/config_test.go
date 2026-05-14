package ahjocontainer

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParse_HonorsSubsetAndCustomizations(t *testing.T) {
	src := []byte(`{
		// JSONC: inline comment
		"remoteUser": "ubuntu",
		"containerEnv": { "FOO": "bar" },
		"forwardPorts": [3000, 5173],
		"postCreateCommand": "pnpm install",
		"customizations": {
			"vscode": { "settings": {} },
			"ahjo": {
				"forward_env": ["MY_TOKEN"],
				"auto_expose": { "enabled": false, "min_port": 4000 }
			}
		},
	}`)
	cfg, err := Parse(src, ConfigPath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.RemoteUser != "ubuntu" {
		t.Errorf("RemoteUser = %q, want %q", cfg.RemoteUser, "ubuntu")
	}
	if cfg.ContainerEnv["FOO"] != "bar" {
		t.Errorf("ContainerEnv[FOO] = %q, want %q", cfg.ContainerEnv["FOO"], "bar")
	}
	if len(cfg.ForwardPorts) != 2 || cfg.ForwardPorts[0] != 3000 {
		t.Errorf("ForwardPorts = %v, want [3000 5173]", cfg.ForwardPorts)
	}
	if string(cfg.PostCreateCommand) != `"pnpm install"` {
		t.Errorf("PostCreateCommand raw = %s, want \"pnpm install\"", cfg.PostCreateCommand)
	}
	ahjo := cfg.Customizations.Ahjo
	if len(ahjo.ForwardEnv) != 1 || ahjo.ForwardEnv[0] != "MY_TOKEN" {
		t.Errorf("Customizations.Ahjo.ForwardEnv = %v", ahjo.ForwardEnv)
	}
	if ahjo.AutoExpose.Enabled == nil || *ahjo.AutoExpose.Enabled {
		t.Errorf("Customizations.Ahjo.AutoExpose.Enabled = %v, want false", ahjo.AutoExpose.Enabled)
	}
	if ahjo.AutoExpose.MinPort == nil || *ahjo.AutoExpose.MinPort != 4000 {
		t.Errorf("Customizations.Ahjo.AutoExpose.MinPort = %v, want 4000", ahjo.AutoExpose.MinPort)
	}
}

func TestParse_RejectsDockerFlavoredFields(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"image", `{"image":"ubuntu:24.04"}`, "image"},
		{"build", `{"build":{"dockerfile":"Dockerfile"}}`, "build"},
		{"dockerComposeFile", `{"dockerComposeFile":"compose.yml"}`, "dockerComposeFile"},
		{"mounts", `{"mounts":[{"source":"x","target":"y"}]}`, "mounts"},
		{"runArgs", `{"runArgs":["--privileged"]}`, "runArgs"},
		{"secrets", `{"secrets":{"FOO":{"description":"x"}}}`, "secrets"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.body), ConfigPath)
			if err == nil {
				t.Fatalf("expected error rejecting %q, got nil", c.name)
			}
			if !strings.Contains(err.Error(), "`"+c.want+"`") {
				t.Fatalf("error %q should name field %q", err.Error(), c.want)
			}
		})
	}
}

func TestParse_NullAndEmptyValuesAreNotRejected(t *testing.T) {
	// Some authors include "image": null or "mounts": [] as a placeholder
	// for "tool fills this in"; treat absent shape as absent.
	cases := []string{
		`{"image": null, "remoteUser": "ubuntu"}`,
		`{"mounts": [], "remoteUser": "ubuntu"}`,
		`{"runArgs": [], "remoteUser": "ubuntu"}`,
		`{"build": null, "remoteUser": "ubuntu"}`,
		`{"image": "", "remoteUser": "ubuntu"}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			if _, err := Parse([]byte(body), ConfigPath); err != nil {
				t.Fatalf("parse should accept %s; got %v", body, err)
			}
		})
	}
}

func TestParse_AcceptsFeatures_Phase2b(t *testing.T) {
	body := `{"features": {
		"ghcr.io/devcontainers/features/node:1": {"version": "20"},
		"ghcr.io/devcontainers/features/common-utils:2": {}
	}}`
	cfg, err := Parse([]byte(body), ConfigPath)
	if err != nil {
		t.Fatalf("Phase 2b should accept features: %v", err)
	}
	if len(cfg.Features) != 2 {
		t.Fatalf("Features = %v, want 2 entries", cfg.Features)
	}
}

func TestParse_TrailingCommasAccepted(t *testing.T) {
	// hujson lax dialect: trailing commas common in Codespaces-targeting
	// devcontainer.json files.
	body := `{
		"remoteUser": "ubuntu",
		"forwardPorts": [3000,],
	}`
	cfg, err := Parse([]byte(body), ConfigPath)
	if err != nil {
		t.Fatalf("trailing-comma parse: %v", err)
	}
	if cfg.RemoteUser != "ubuntu" || len(cfg.ForwardPorts) != 1 || cfg.ForwardPorts[0] != 3000 {
		t.Fatalf("trailing-comma parse dropped fields: %+v", cfg)
	}
}

func TestParse_BlockComment(t *testing.T) {
	body := `{
		/* block comment with "quoted" content */
		"remoteUser": "ubuntu"
	}`
	cfg, err := Parse([]byte(body), ConfigPath)
	if err != nil {
		t.Fatalf("block-comment parse: %v", err)
	}
	if cfg.RemoteUser != "ubuntu" {
		t.Fatalf("RemoteUser dropped after block comment: %+v", cfg)
	}
}

func TestCheckRemoteUser_WarnsOnMismatchOnly(t *testing.T) {
	cases := []struct {
		name   string
		c      Config
		want   string
		wantOK bool
	}{
		{
			name: "ok-empty",
			c:    Config{Source: "x"},
		},
		{
			name: "ok-matches",
			c:    Config{Source: "x", RemoteUser: "ubuntu", ContainerUser: "ubuntu"},
		},
		{
			name:   "mismatch-remoteUser",
			c:      Config{Source: ConfigPath, RemoteUser: "vscode"},
			want:   "remoteUser",
			wantOK: true,
		},
		{
			name:   "mismatch-containerUser",
			c:      Config{Source: ConfigPath, ContainerUser: "node"},
			want:   "containerUser",
			wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.c.CheckRemoteUser("ubuntu")
			if tc.wantOK {
				if !strings.Contains(msg, tc.want) {
					t.Fatalf("want msg containing %q; got %q", tc.want, msg)
				}
			} else if msg != "" {
				t.Fatalf("want empty msg; got %q", msg)
			}
		})
	}
}

func TestLoadFromHost_ReadsCanonicalPath(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, ConfigPath)
	if err := mkdirAll(filepath.Dir(full)); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(full, `{"remoteUser": "ubuntu"}`); err != nil {
		t.Fatal(err)
	}

	cfg, ok, err := LoadFromHost(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !ok {
		t.Fatal("expected found")
	}
	if cfg.RemoteUser != "ubuntu" {
		t.Fatalf("RemoteUser = %q, want ubuntu", cfg.RemoteUser)
	}
	if !strings.HasSuffix(cfg.Source, ConfigPath) {
		t.Fatalf("Source = %q", cfg.Source)
	}
}

func TestLoadFromHost_AbsentReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	cfg, ok, err := LoadFromHost(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ok || cfg != nil {
		t.Fatalf("want (nil, false, nil); got (%v, %v)", cfg, ok)
	}
}

func TestLegacyDevcontainerPaths_CoverBothShapes(t *testing.T) {
	// Mirrors the LegacyAhjoconfig posture: a guard list, not a fallback
	// loader. Exposed so callers / future tests can iterate either form.
	want := map[string]bool{
		".devcontainer/devcontainer.json": false,
		".devcontainer.json":              false,
	}
	for _, p := range LegacyDevcontainerPaths {
		if _, ok := want[p]; !ok {
			t.Fatalf("unexpected legacy path %q", p)
		}
		want[p] = true
	}
	for p, seen := range want {
		if !seen {
			t.Fatalf("missing legacy path %q", p)
		}
	}
}
