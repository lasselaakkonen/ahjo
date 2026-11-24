// Package tokenstore persists KEY=VALUE pairs in ~/.ahjo/.env (and per-repo
// .env files under ~/.ahjo-shared/repo-env/<slug>.env) so the in-VM ahjo can
// look secrets up on demand (Lookup/Resolver) and forward them into
// containers via the forward_env mechanism, without requiring the user to
// edit shellrc. The files are never exported into ahjo's own process env.
package tokenstore

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

const (
	TokenEnv   = "CLAUDE_CODE_OAUTH_TOKEN"
	GHTokenEnv = "GH_TOKEN"
)

func Path() string { return filepath.Join(paths.AhjoDir(), ".env") }

// Lookup resolves key the way a startup load into the process env would have
// made os.Getenv behave: a non-empty process env value wins, then
// ~/.ahjo/.env, then a set-but-empty process env value. It never mutates the
// process env. A read error on ~/.ahjo/.env is treated as "not in the file".
func Lookup(key string) (string, bool) {
	r, _ := Resolver()
	return r(key)
}

// Resolver returns a lookup func with the same precedence as Lookup that
// reads ~/.ahjo/.env once. A read error is returned alongside a resolver
// that falls back to process env only.
func Resolver() (func(key string) (string, bool), error) { return ResolverAt(Path()) }

// ResolverAt is Resolver against an explicit env file path.
func ResolverAt(path string) (func(key string) (string, bool), error) {
	file, err := ListAt(path)
	if err != nil {
		file = nil
	}
	return func(key string) (string, bool) {
		v, ok := os.LookupEnv(key)
		if ok && v != "" {
			return v, true
		}
		if fv, found := file[key]; found {
			return fv, true
		}
		return v, ok
	}, err
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

// SetAt is the generic write: upserts key=val in path with mode 0600. The
// file is replaced atomically, so a pre-existing wider mode does not survive.
// key must be non-empty and free of '=', whitespace and NUL; val must be free
// of newlines and NUL, so a value cannot inject another line into the file.
// Validation errors name the key, never the value.
func SetAt(path, key, val string) error {
	if err := validate(key, val); err != nil {
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
	return writeAtomic(path, []byte(strings.Join(lines, "\n")+"\n"))
}

// validate rejects a key or value that would corrupt the KEY=VALUE line
// format. The error never contains val.
func validate(key, val string) error {
	if key == "" {
		return fmt.Errorf("tokenstore: empty key")
	}
	if strings.ContainsRune(key, '=') || strings.ContainsRune(key, 0) ||
		strings.IndexFunc(key, unicode.IsSpace) >= 0 {
		return fmt.Errorf("tokenstore: key %q contains '=', whitespace or NUL", key)
	}
	if strings.ContainsAny(val, "\n\r\x00") {
		return fmt.Errorf("tokenstore: value for %s contains a newline, carriage return or NUL", key)
	}
	return nil
}

// writeAtomic replaces path with data via a 0600 temp file in the same
// directory plus rename, so readers never see a half-written file and the
// result is 0600 regardless of the previous file's mode. The directory is
// created 0700 when missing; an existing directory keeps its mode. The temp
// file is removed on any error.
func writeAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".env-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
		return writeAtomic(path, nil)
	}
	return writeAtomic(path, []byte(strings.Join(out, "\n")+"\n"))
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
