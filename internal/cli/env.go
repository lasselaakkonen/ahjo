package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/lasselaakkonen/ahjo/internal/tokenstore"
)

var envKeyRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func newEnvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Read/write KEY=VALUE pairs in ~/.ahjo/.env (forwarded into containers via forward_env)",
		Long: `Manage values stored in ~/.ahjo/.env (mode 0600). Keys listed in
config.forward_env (default: CLAUDE_CODE_OAUTH_TOKEN, GH_TOKEN) are forwarded
into every container at ` + "`ahjo shell` / `ahjo claude`" + ` time.

For per-repo overrides (typical for GH_TOKEN), use ` + "`ahjo repo set-token <slug>`" + `
instead; that writes to ~/.ahjo-shared/repo-env/<slug>.env and only forwards
into containers for that repo.`,
	}
	cmd.AddCommand(newEnvSetCmd(), newEnvGetCmd(), newEnvUnsetCmd(), newEnvListCmd())
	return cmd
}

func newEnvSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set KEY [VALUE]",
		Short: "Set KEY=VALUE in ~/.ahjo/.env. Omit VALUE to prompt with hidden input.",
		Long: `Set KEY=VALUE in ~/.ahjo/.env (mode 0600), preserving other keys.

  ahjo env set FOO bar          # one-shot
  ahjo env set FOO              # prompt with hidden input (no echo, never in history)
  echo "$VAL" | ahjo env set FOO  # pipeline; stderr notes the non-TTY read

Tip: if you have ` + "`gh`" + ` on your host,
  ahjo env set GH_TOKEN "$(gh auth token)"
reuses your existing login. WARNING: this exposes every repo your account can
see to the autonomous agents running inside containers. Prefer per-repo
fine-grained PATs (set during ` + "`ahjo repo add`" + ` or via ` + "`ahjo repo set-token`" + `).`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			key := args[0]
			if err := validateEnvKey(key); err != nil {
				return err
			}
			var val string
			if len(args) == 2 {
				val = args[1]
			} else {
				v, err := readSecret(os.Stdin, cobraOut(), cobraOutErr(), fmt.Sprintf("Value for %s: ", key))
				if err != nil {
					return err
				}
				val = v
			}
			if val == "" {
				return fmt.Errorf("empty value for %s", key)
			}
			if err := tokenstore.Set(key, val); err != nil {
				return err
			}
			fmt.Fprintf(cobraOut(), "  → saved to %s\n", tokenstore.Path())
			return nil
		},
	}
}

func newEnvGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get KEY",
		Short: "Print the value of KEY from ~/.ahjo/.env (exit 1 if missing)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			key := args[0]
			if err := validateEnvKey(key); err != nil {
				return err
			}
			v, ok, err := tokenstore.Get(key)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%s not set in %s", key, tokenstore.Path())
			}
			fmt.Fprintln(cobraOut(), v)
			return nil
		},
	}
}

func newEnvUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset KEY",
		Short: "Remove KEY from ~/.ahjo/.env (no-op if missing)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			key := args[0]
			if err := validateEnvKey(key); err != nil {
				return err
			}
			return tokenstore.Unset(key)
		},
	}
}

func newEnvListCmd() *cobra.Command {
	var show bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List keys in ~/.ahjo/.env (values masked by default)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			m, err := tokenstore.List()
			if err != nil {
				return err
			}
			if len(m) == 0 {
				fmt.Fprintln(cobraOut(), "(empty)")
				return nil
			}
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			// stable output
			sortStrings(keys)
			for _, k := range keys {
				v := m[k]
				if !show {
					v = maskSecret(v)
				}
				fmt.Fprintf(cobraOut(), "%s=%s\n", k, v)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&show, "show", false, "print raw values instead of masking")
	return cmd
}

func validateEnvKey(key string) error {
	if !envKeyRE.MatchString(key) {
		return fmt.Errorf("invalid key %q: must match [A-Z_][A-Z0-9_]* so the .env file is shell-sourceable", key)
	}
	return nil
}

// readSecret reads a value with `*` echo per byte when stdin is a TTY, else
// reads one line plain with a stderr note. Returns the trimmed value.
//
// term.ReadPassword echoes nothing, which makes long pastes feel like a
// hang. Putting the TTY in raw mode and echoing `*` per input byte gives
// visible feedback without revealing the value or letting it land in
// terminal scrollback.
func readSecret(in *os.File, out, errw io.Writer, prompt string) (string, error) {
	if isTerminal(in) {
		fmt.Fprint(out, prompt)
		s, err := readSecretEcho(in, out)
		fmt.Fprintln(out)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		return strings.TrimSpace(s), nil
	}
	fmt.Fprintln(errw, "note: stdin is not a TTY; reading value without masking")
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", errors.New("no value entered")
	}
	return strings.TrimSpace(sc.Text()), nil
}

// readSecretEcho puts the TTY in raw mode and reads bytes until CR/LF/EOF,
// echoing `*` for each accepted byte and handling Backspace/DEL. Ctrl-C and
// Ctrl-D abort; Ctrl-U clears the current input.
func readSecretEcho(in *os.File, out io.Writer) (string, error) {
	fd := int(in.Fd())
	prev, err := term.MakeRaw(fd)
	if err != nil {
		// Raw mode unavailable (e.g. unusual TTY) — fall back to silent
		// read so we never accidentally echo the secret in cooked mode.
		b, perr := term.ReadPassword(fd)
		if perr != nil {
			return "", perr
		}
		return string(b), nil
	}
	defer func() { _ = term.Restore(fd, prev) }()

	var buf []byte
	one := make([]byte, 1)
	for {
		n, err := in.Read(one)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
		if n == 0 {
			continue
		}
		c := one[0]
		switch c {
		case '\r', '\n':
			return string(buf), nil
		case 0x03: // Ctrl-C
			return "", errors.New("interrupted")
		case 0x04: // Ctrl-D
			if len(buf) == 0 {
				return "", io.EOF
			}
			return string(buf), nil
		case 0x15: // Ctrl-U: clear line
			for range buf {
				fmt.Fprint(out, "\b \b")
			}
			buf = buf[:0]
		case 0x7f, 0x08: // DEL / Backspace
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				fmt.Fprint(out, "\b \b")
			}
		default:
			if c < 0x20 { // ignore other control bytes
				continue
			}
			buf = append(buf, c)
			fmt.Fprint(out, "*")
		}
	}
	return string(buf), nil
}

// maskSecret replaces all but the last 4 chars with an ellipsis. Empty and
// very short values are fully masked.
func maskSecret(s string) string {
	if len(s) <= 4 {
		return "…"
	}
	return "…" + s[len(s)-4:]
}

func sortStrings(s []string) {
	// tiny inline sort to avoid importing sort just for List output
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// looksLikeGitHubToken classifies a pasted token. Permissive on purpose:
// enterprise installations have unpredictable token shapes, and the Claude
// path's hard prefix check (init.go's sk-ant-oat01- assertion) doesn't
// translate. canonical=true means the prefix matches a known github.com
// shape; canonical=false returns a non-empty hint to print before accepting.
func looksLikeGitHubToken(s string) (canonical bool, hint string, err error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return false, "", errors.New("empty token")
	}
	if strings.ContainsAny(t, " \t\r\n") {
		return false, "", errors.New("token contains whitespace")
	}
	for _, r := range t {
		if r < 0x20 || r == 0x7f {
			return false, "", errors.New("token contains control characters")
		}
	}
	if len(t) < 20 {
		return false, "", errors.New("token is shorter than 20 characters; likely truncated")
	}
	prefixes := []string{"ghp_", "github_pat_", "gho_", "ghs_", "ghu_", "ghr_"}
	for _, p := range prefixes {
		if strings.HasPrefix(t, p) {
			return true, "", nil
		}
	}
	return false, "token doesn't match common GitHub prefixes; accepting anyway — assuming enterprise host", nil
}
