//go:build darwin

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/lasselaakkonen/ahjo/internal/macsecret"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

// ghTokenKey is the canonical key for per-repo GitHub PATs in the Keychain.
// Mirrors tokenstore.GHTokenEnv on the Linux side; duplicated here so the
// shim doesn't drag the linux-build tokenstore into the darwin binary.
const ghTokenKey = "GH_TOKEN"

// interceptRepoSubcommand inspects args and, when the subcommand is one that
// reads or writes a per-repo PAT on macOS, performs the Mac-side half:
//
//   - repo add <url>:        prompt + Keychain store, inject GH_TOKEN on relay
//   - repo set-token <alias>: prompt + Keychain store, inject GH_TOKEN on relay
//   - shell <alias>:         Keychain read, inject GH_TOKEN on relay (silent miss)
//   - claude <alias>:        same as shell
//   - rm <alias>:            no upfront action (in-VM ahjo writes a cleanup marker;
//     sweepKeychainCleanup runs post-relay)
//   - repo rm <alias>:       same as rm
//
// Returns the args to forward to the VM (unchanged for now), the extra env
// pairs to inject on the limactl-shell command line, and any error. Errors
// returned here are fatal to the shim — they happen before relay and indicate
// either a refused PAT (locked Keychain, broken `security`) or a missing
// repo-aliases entry, both of which the user must fix before continuing.
func interceptRepoSubcommand(args []string) (newArgs []string, env []string, err error) {
	if len(args) == 0 {
		return args, nil, nil
	}
	switch args[0] {
	case "repo":
		if len(args) >= 2 {
			switch args[1] {
			case "add":
				return interceptRepoAdd(args)
			case "set-token":
				return interceptRepoSetToken(args)
			}
		}
	case "shell", "claude":
		return interceptShellLike(args)
	}
	return args, nil, nil
}

// interceptRepoAdd parses the URL out of `repo add [flags] <url>` and, when
// the user is on a TTY and the Keychain has no row for the URL-derived slug,
// prompts for a PAT and stores it. On any skip path (--yes, non-TTY, empty
// paste, already-stored, --help) returns args unchanged with no env injection
// — the in-VM ahjo runs its normal flow.
func interceptRepoAdd(args []string) ([]string, []string, error) {
	yes := hasFlag(args[2:], "-y", "--yes")
	if hasFlag(args[2:], "-h", "--help") {
		return args, nil, nil
	}
	url := findRepoAddURL(args[2:])
	if url == "" {
		// No URL on the line; let the in-VM ahjo emit the usage error.
		return args, nil, nil
	}
	slug, err := slugFromRepoURL(url)
	if err != nil {
		// Bad URL: defer to in-VM for the canonical error.
		return args, nil, nil
	}
	tok, found, err := macsecret.Get(slug, ghTokenKey)
	if err != nil {
		return nil, nil, fmt.Errorf("read Keychain for %s: %w", slug, err)
	}
	if found && tok != "" {
		return args, []string{"GH_TOKEN=" + tok}, nil
	}
	if yes || !isTerminal(os.Stdin) {
		return args, nil, nil
	}
	tok, err = promptStorePAT(slug, fmt.Sprintf("Paste a GitHub token for %s, or press Enter to skip: ", slug))
	if err != nil {
		return nil, nil, err
	}
	if tok == "" {
		return args, nil, nil
	}
	return args, []string{"GH_TOKEN=" + tok}, nil
}

// interceptRepoSetToken resolves the alias, prompts on the Mac, and stores in
// Keychain. Errors when the alias has no repo-aliases entry — set-token only
// makes sense on a registered repo.
func interceptRepoSetToken(args []string) ([]string, []string, error) {
	alias := firstNonFlag(args[2:])
	if alias == "" {
		return args, nil, nil
	}
	slug, err := lookupRepoSlug(alias)
	if err != nil {
		// Alias unknown to the Mac side. Let the in-VM ahjo emit the canonical
		// error rather than guessing wrong here.
		return args, nil, nil
	}
	if !isTerminal(os.Stdin) {
		return nil, nil, fmt.Errorf("set-token requires a TTY (paste a token interactively); refusing to fall back to in-VM .env path on macOS")
	}
	tok, err := promptStorePAT(slug, fmt.Sprintf("Paste GitHub token for %s: ", alias))
	if err != nil {
		return nil, nil, err
	}
	if tok == "" {
		return nil, nil, fmt.Errorf("no token entered")
	}
	return args, []string{"GH_TOKEN=" + tok}, nil
}

// interceptShellLike fetches an existing Keychain entry for `shell <alias>` /
// `claude <alias>` and forwards it as GH_TOKEN. A miss is silent so the
// existing public-repo / ssh-agent flow keeps working when the user never
// stored a PAT.
func interceptShellLike(args []string) ([]string, []string, error) {
	alias := firstNonFlag(args[1:])
	if alias == "" {
		return args, nil, nil
	}
	slug, err := lookupRepoSlug(alias)
	if err != nil {
		return args, nil, nil
	}
	tok, found, err := macsecret.Get(slug, ghTokenKey)
	if err != nil {
		// Locked Keychain or a `security` failure — surface to the user
		// rather than silently relaying without a token they expect.
		return nil, nil, fmt.Errorf("read Keychain for %s: %w", slug, err)
	}
	if !found || tok == "" {
		return args, nil, nil
	}
	return args, []string{"GH_TOKEN=" + tok}, nil
}

// promptStorePAT reads a PAT with hidden input, writes it into the login
// Keychain at (slug, GH_TOKEN), and returns the value so callers can also
// forward it to the in-VM ahjo. Empty paste returns "" with no Keychain
// write.
func promptStorePAT(slug, prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	tok, err := readHiddenLine(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stdout)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", nil
	}
	if err := macsecret.Set(slug, ghTokenKey, tok); err != nil {
		return "", fmt.Errorf("store in Keychain: %w", err)
	}
	fmt.Fprintf(os.Stdout, "  → saved to macOS Keychain (service=ahjo.%s, account=%s)\n", ghTokenKey, slug)
	fmt.Fprintln(os.Stdout, "    Keychain may prompt the first time ahjo reads this PAT — click \"Always Allow\" for ahjo.")
	return tok, nil
}

func readHiddenLine(fd int) (string, error) {
	if !term.IsTerminal(fd) {
		// Defensive: callers already gate on isTerminal, but if a future
		// caller forgets, fall back to a plain bufio read so we don't echo.
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() {
			return "", sc.Err()
		}
		return sc.Text(), nil
	}
	b, err := term.ReadPassword(fd)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// sweepKeychainCleanup processes every marker file at
// <SharedDir>/.keychain-cleanup/<slug>. For each, it deletes the matching
// Keychain row and removes the marker. Errors are logged but non-fatal —
// the in-VM ahjo's lifecycle (rm / repo rm) is the canonical source of
// truth; Keychain leakage at most leaves a stale row visible in Keychain
// Access.app.
func sweepKeychainCleanup() {
	dir := paths.KeychainCleanupDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warn: read %s: %v\n", dir, err)
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		slug := e.Name()
		if strings.HasPrefix(slug, ".") {
			continue
		}
		if err := macsecret.Delete(slug, ghTokenKey); err != nil {
			fmt.Fprintf(os.Stderr, "warn: delete Keychain for %s: %v\n", slug, err)
			continue
		}
		if err := os.Remove(filepath.Join(dir, slug)); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warn: remove marker %s: %v\n", slug, err)
		}
	}
}

// findRepoAddURL returns the first non-flag arg from `repo add` args (post
// `add`), skipping flag values for known string flags. Returns "" when no
// URL is present (user typed `ahjo repo add --help`, e.g.).
func findRepoAddURL(rest []string) string {
	stringFlags := map[string]bool{"--default-base": true, "--as": true}
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "--" {
			if i+1 < len(rest) {
				return rest[i+1]
			}
			return ""
		}
		if stringFlags[a] {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

// firstNonFlag returns the first positional argument, skipping flag values
// for the small set of string flags shared across repo subcommands.
func firstNonFlag(rest []string) string {
	stringFlags := map[string]bool{"--default-base": true, "--as": true}
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "--" {
			if i+1 < len(rest) {
				return rest[i+1]
			}
			return ""
		}
		if stringFlags[a] {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

// slugFromRepoURL mirrors the in-VM allocation: derive owner/repo from a git
// URL, normalize via AliasToSlug (lowercase + [a-z0-9-]). Returns an error
// when the URL doesn't yield a usable owner/repo pair so the caller can
// defer to the in-VM ahjo for the canonical error message.
//
// Collisions (registry already has the base slug, so the in-VM ahjo allocates
// -2/-3) aren't visible from here. Documented limitation: on collision the
// user's first-run Keychain row will key off the URL-derived slug while the
// in-VM ahjo allocates a suffix, and the in-VM read falls through to the
// public-clone path. The user can rotate with `ahjo repo set-token <alias>`
// once they see the correct alias post-add.
func slugFromRepoURL(url string) (string, error) {
	alias, err := registry.DeriveRepoAlias(url)
	if err != nil {
		return "", err
	}
	slug := registry.AliasToSlug(alias)
	if slug == "" {
		return "", fmt.Errorf("empty slug for %q", url)
	}
	return slug, nil
}

// lookupRepoSlug resolves a user-typed alias (repo or branch) to the parent
// repo's slug by reading the in-VM-written <SharedDir>/repo-aliases file.
// Returns an error when the file is missing or the alias is unknown.
func lookupRepoSlug(alias string) (string, error) {
	path := paths.RepoAliasesPath()
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		if parts[0] == alias {
			return parts[1], nil
		}
	}
	return "", fmt.Errorf("alias %q not in %s", alias, path)
}

// isTerminal — local-to-darwin variant; the linux-build cli.isTerminal lives
// in a package the shim doesn't link.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}
