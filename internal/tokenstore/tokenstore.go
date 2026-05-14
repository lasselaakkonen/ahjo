// Package tokenstore persists KEY=VALUE pairs in ~/.ahjo/.env (and per-repo
// .env files under ~/.ahjo-shared/repo-env/<slug>.env) so the in-VM ahjo can
// pick secrets up automatically and forward them into containers via the
// forward_env mechanism, without requiring the user to edit shellrc.
package tokenstore

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

const (
	TokenEnv   = "CLAUDE_CODE_OAUTH_TOKEN"
	GHTokenEnv = "GH_TOKEN"
)

func Path() string { return filepath.Join(paths.AhjoDir(), ".env") }

// Load applies KEY=VALUE pairs from ~/.ahjo/.env to the process env, but only
// for keys that are currently unset. Missing file is not an error.
func Load() error {
	f, err := os.Open(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := parseLine(sc.Text())
		if !ok {
			continue
		}
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

// SetToken writes CLAUDE_CODE_OAUTH_TOKEN into ~/.ahjo/.env, preserving any
// other keys already in the file.
func SetToken(tok string) error { return Set(TokenEnv, tok) }

// Set writes key=val into ~/.ahjo/.env (mode 0600), preserving other keys.
func Set(key, val string) error { return SetAt(Path(), key, val) }

// Get reads key from ~/.ahjo/.env without mutating process env.
func Get(key string) (string, bool, error) { return GetAt(Path(), key) }

// Unset removes key from ~/.ahjo/.env. No-op if missing.
func Unset(key string) error { return UnsetAt(Path(), key) }

// List returns every KEY=VALUE in ~/.ahjo/.env.
func List() (map[string]string, error) { return ListAt(Path()) }

// SetAt is the generic write: upserts key=val in path with mode 0600.
func SetAt(path, key, val string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var lines []string
	found := false
	if b, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), key+"=") {
				lines = append(lines, fmt.Sprintf("%s=%s", key, val))
				found = true
				continue
			}
			lines = append(lines, line)
		}
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines = lines[:n-1]
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if !found {
		lines = append(lines, fmt.Sprintf("%s=%s", key, val))
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// GetAt reads a single key from path.
func GetAt(path, key string) (string, bool, error) {
	m, err := ListAt(path)
	if err != nil {
		return "", false, err
	}
	v, ok := m[key]
	return v, ok, nil
}

// UnsetAt removes key from path. No-op if the file or key is missing.
func UnsetAt(path, key string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var out []string
	removed := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), key+"=") {
			removed = true
			continue
		}
		out = append(out, line)
	}
	if !removed {
		return nil
	}
	if n := len(out); n > 0 && out[n-1] == "" {
		out = out[:n-1]
	}
	if len(out) == 0 {
		return os.WriteFile(path, nil, 0o600)
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

// ListAt returns every KEY=VALUE in path. Missing file → empty map, no error.
func ListAt(path string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := parseLine(sc.Text())
		if !ok {
			continue
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadInto reads KEY=VALUE pairs from path into m without touching process
// env. Existing entries in m are overwritten by values from path. Missing
// file is not an error.
func LoadInto(path string, m map[string]string) error {
	loaded, err := ListAt(path)
	if err != nil {
		return err
	}
	for k, v := range loaded {
		m[k] = v
	}
	return nil
}

func parseLine(line string) (string, string, bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", false
	}
	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return "", "", false
	}
	k := strings.TrimSpace(s[:eq])
	v := strings.TrimSpace(s[eq+1:])
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	return k, v, true
}
