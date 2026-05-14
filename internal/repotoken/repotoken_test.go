package repotoken

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadDelete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Save("foo-bar", "github_pat_TESTING"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	tok, ok, err := Load("foo-bar")
	if err != nil || !ok || tok != "github_pat_TESTING" {
		t.Fatalf("Load returned (%q, %v, %v)", tok, ok, err)
	}
	if err := Delete("foo-bar"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, err = Load("foo-bar")
	if ok || err != nil {
		t.Fatalf("post-Delete Load returned ok=%v err=%v", ok, err)
	}
	// Idempotent delete.
	if err := Delete("foo-bar"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestSavePermissions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Save("perm-check", "tok"); err != nil {
		t.Fatal(err)
	}
	tokenDir := filepath.Join(os.Getenv("HOME"), ".ahjo", "repo-tokens")
	if info, err := os.Stat(tokenDir); err != nil {
		t.Fatal(err)
	} else if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("token dir mode = %o; want 0700", mode)
	}
	info, err := os.Stat(filepath.Join(tokenDir, "perm-check.env"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("token file mode = %o; want 0600", mode)
	}
}

func TestSaveEmptyTokenRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Save("x", ""); err == nil {
		t.Errorf("expected error on empty token, got nil")
	}
}

func TestLoadMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tok, ok, err := Load("never-existed")
	if err != nil || ok || tok != "" {
		t.Fatalf("Load(missing) = (%q, %v, %v); want (\"\", false, nil)", tok, ok, err)
	}
}

func TestLoadFromFile(t *testing.T) {
	tmp := t.TempDir()
	cases := []struct {
		name, body, want string
	}{
		{"raw single-line", "github_pat_RAW\n", "github_pat_RAW"},
		{"env-style GH_TOKEN", "GH_TOKEN=github_pat_ENV\n", "github_pat_ENV"},
		{"env-style GITHUB_TOKEN fallback", "GITHUB_TOKEN=github_pat_GH\n", "github_pat_GH"},
		{"env-style with comments", "# comment\nGH_TOKEN=github_pat_WITH_COMMENT\n", "github_pat_WITH_COMMENT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(tmp, c.name+".env")
			if err := os.WriteFile(p, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadFromFile(p)
			if err != nil {
				t.Fatalf("LoadFromFile: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("got %q; want %q", got, c.want)
			}
		})
	}
}

func TestPrompt(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader("  github_pat_PASTED  \n")
	tok, err := Prompt(&out, in, "owner/repo")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if string(tok) != "github_pat_PASTED" {
		t.Errorf("got %q; want trimmed github_pat_PASTED", tok)
	}
	if !strings.Contains(out.String(), "owner/repo") {
		t.Errorf("prompt didn't include ownerRepo; got: %s", out.String())
	}
}

func TestPromptEmptyRejected(t *testing.T) {
	in := strings.NewReader("\n")
	_, err := Prompt(&bytes.Buffer{}, in, "owner/repo")
	if err == nil {
		t.Error("expected error on empty token")
	}
}
