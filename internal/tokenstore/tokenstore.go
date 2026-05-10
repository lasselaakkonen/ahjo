// Package tokenstore persists CLAUDE_CODE_OAUTH_TOKEN in ~/.ahjo/.env so the
// in-VM ahjo can pick it up automatically (and forward it into containers via
// the forward_env mechanism) without requiring the user to edit shellrc.
package tokenstore

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

const TokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

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
// other keys already in the file. The file is created with mode 0600.
func SetToken(tok string) error {
	return upsert(TokenEnv, tok)
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

func upsert(key, val string) error {
	if err := os.MkdirAll(paths.AhjoDir(), 0o755); err != nil {
		return err
	}
	p := Path()
	var lines []string
	found := false
	if b, err := os.ReadFile(p); err == nil {
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
	return os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}
