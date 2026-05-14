// Package repotoken stores per-repo GitHub fine-grained PATs and forwards
// them into the matching Incus container as `GH_TOKEN`.
//
// One token per repo, scoped to that repo on GitHub via the PAT's "Only
// select repositories" setting. The token covers both git transport
// (via `gh auth setup-git` configuring the credential helper) and the
// GitHub API surface (`gh pr create` etc.). Compromise of one container
// can't reach a different repo: the PAT's resource list is a hard fence.
//
// Files live at `~/.ahjo/repo-tokens/<slug>.env`, mode 0600, one
// `GH_TOKEN=<value>` line. `ahjo repo add` writes; `ahjo repo rm` deletes.
package repotoken

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// Token is a GitHub PAT. Stored separately from a plain string so callers
// can't accidentally log/print one — the type still has a String() method,
// but greppable use cases (e.g. `repotoken.Token`-typed fields) make leaks
// easier to audit.
type Token string

// Save writes the per-slug token file. Mode 0600, atomic via rename so a
// crash mid-write can't truncate the previous token to half a line.
func Save(slug string, t Token) error {
	if t == "" {
		return fmt.Errorf("save: empty token")
	}
	if err := os.MkdirAll(paths.RepoTokensDir(), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", paths.RepoTokensDir(), err)
	}
	final := paths.RepoTokenFile(slug)
	tmp, err := os.CreateTemp(paths.RepoTokensDir(), slug+".env.tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := fmt.Fprintf(tmp, "GH_TOKEN=%s\n", string(t)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), final)
}

// Load returns the per-slug token, or ("", false, nil) if no token file
// exists. Missing-file is distinct from other errors so callers can treat it
// as "no PAT configured" without an error.
func Load(slug string) (Token, bool, error) {
	b, err := os.ReadFile(paths.RepoTokenFile(slug))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == "GH_TOKEN" {
			return Token(strings.TrimSpace(v)), true, nil
		}
	}
	return "", false, fmt.Errorf("%s: no GH_TOKEN= line", paths.RepoTokenFile(slug))
}

// Delete removes the per-slug token file. Missing-file is not an error so
// `ahjo repo rm` is idempotent against a user who manually cleaned up.
func Delete(slug string) error {
	err := os.Remove(paths.RepoTokenFile(slug))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LoadFromFile reads a token from path. Accepts either a raw token (one
// line) or an env-file with `GH_TOKEN=<value>` / `GITHUB_TOKEN=<value>`.
// Used by `ahjo repo add --token-file <path>` for non-interactive setup.
func LoadFromFile(path string) (Token, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s: empty", path)
	}
	// Env-file form: look for GH_TOKEN= or GITHUB_TOKEN=.
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"GH_TOKEN=", "GITHUB_TOKEN="} {
			if strings.HasPrefix(line, prefix) {
				return Token(strings.TrimSpace(strings.TrimPrefix(line, prefix))), nil
			}
		}
	}
	// Single-line raw token.
	if !strings.ContainsAny(s, "\n\r") {
		return Token(s), nil
	}
	return "", fmt.Errorf("%s: looks like a multi-line file but has no GH_TOKEN= line", path)
}

// Prompt prints PAT-creation instructions for ownerRepo and reads one line
// from in. The instructions include the exact GitHub permission grants
// that match the in-container scope ahjo expects.
func Prompt(out io.Writer, in io.Reader, ownerRepo string) (Token, error) {
	fmt.Fprintf(out, `
Create a fine-grained GitHub PAT for %s:

  https://github.com/settings/personal-access-tokens/new

  Repository access:  Only select repositories → %s
  Permissions:        Contents              read/write
                      Pull requests         read/write
                      Issues                read/write
                      Metadata              read (granted automatically)
  Expiration:         your call (max 1 year)

Paste the token (begins with github_pat_…):
> `, ownerRepo, ownerRepo)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	tok := strings.TrimSpace(line)
	if tok == "" {
		return "", fmt.Errorf("no token entered")
	}
	if !strings.HasPrefix(tok, "github_pat_") && !strings.HasPrefix(tok, "ghp_") && !strings.HasPrefix(tok, "ghs_") {
		// Don't hard-fail — GitHub could add new prefixes. Just flag it.
		fmt.Fprintf(out, "warn: token doesn't start with github_pat_/ghp_/ghs_ — using anyway\n")
	}
	return Token(tok), nil
}
