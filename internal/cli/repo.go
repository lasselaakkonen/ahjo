package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	"github.com/lasselaakkonen/ahjo/internal/repoauth"
	"github.com/lasselaakkonen/ahjo/internal/tokenstore"
)

// featureConsentForNew is the seed FeatureConsent map for a not-yet-
// registered repo. Empty: a brand-new repo has no prior trust
// decisions, so applyRepoFeatures prompts on every non-curated source.
var featureConsentForNew = map[string]bool{}

// dropRepoToken removes the per-repo PAT side-effect. On Linux bare-metal the
// per-repo .env file under SharedDir() is the authoritative store, so it gets
// `os.Remove`d directly. On Mac users' VM the Keychain entry lives on the
// host where the in-VM ahjo can't reach `security`; we drop a marker file
// under <SharedDir>/.keychain-cleanup/<slug> for the Mac shim to sweep after
// it sees the in-VM call return. Either side is best-effort; the registry
// row is the source of truth for "is this repo gone?".
func dropRepoToken(slug string) {
	if _, isMac := paths.MacHostHome(); isMac {
		dir := paths.KeychainCleanupDir()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: mkdir %s: %v\n", dir, err)
			return
		}
		marker := paths.KeychainCleanupMarker(slug)
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: write Keychain cleanup marker %s: %v\n", marker, err)
		}
		return
	}
	if err := os.Remove(paths.SlugEnvPath(slug)); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(cobraOutErr(), "warn: remove %s: %v\n", paths.SlugEnvPath(slug), err)
	}
}

// repoToken centralizes "what's the GH PAT for this repo right now?" across
// the two backends. On Mac users' VM (MacHostHome() truthy) the Mac shim
// reads from Keychain pre-relay and injects GH_TOKEN; the in-VM code never
// touches the disk path so a script grepping ~ for `ghp_*` finds nothing.
// On standalone Linux the per-repo .env file under SharedDir() is the
// canonical store and the env var is unused.
func repoToken(slug string) (string, bool, error) {
	if v := os.Getenv(tokenstore.GHTokenEnv); v != "" {
		return v, true, nil
	}
	if _, isMac := paths.MacHostHome(); isMac {
		return "", false, nil
	}
	return tokenstore.GetAt(paths.SlugEnvPath(slug), tokenstore.GHTokenEnv)
}

func newRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Manage the repo registry",
	}
	cmd.AddCommand(newRepoAddCmd(), newRepoLsCmd(), newRepoRmCmd(), newRepoPullCmd(), newRepoSetTokenCmd())
	return cmd
}

func newRepoLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List registered repos",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			reg, err := registry.Load()
			if err != nil {
				return err
			}
			if len(reg.Repos) == 0 {
				fmt.Println("no repos registered")
				return nil
			}
			for _, r := range reg.Repos {
				fmt.Printf("%-30s  %s  (base: %s)\n",
					strings.Join(r.Aliases, ","), r.Remote, r.DefaultBase)
			}
			return nil
		},
	}
}

// EnsureRepo returns the repo registered under repoAlias. If the repo
// isn't registered and the alias has the canonical "<owner>/<repo>"
// shape, it auto-adds the repo by running the standard `repo add` flow,
// which derives a GitHub URL once the per-repo PAT is resolved (HTTPS
// when a token is available so the clone uses it, else SSH-if-reachable).
// Idempotent: a second call on a registered repo just returns it.
//
// containerConfig is the value of --container-config (a built-in stack
// name, a repo-local .ahjo/<name>.json basename, a host path, or
// "bare"). Forwarded to runRepoAdd for the auto-add path. Pass "" to
// fall back to the standard resolution (in-repo ahjocontainer.json, or
// the interactive picker on a TTY). Ignored when the repo is already
// registered.
func EnsureRepo(ctx context.Context, repoAlias, containerConfig string) (*registry.Repo, error) {
	reg, err := registry.Load()
	if err != nil {
		return nil, err
	}
	if r := reg.FindRepoByAlias(repoAlias); r != nil {
		return r, nil
	}

	if _, _, ok := splitRepoAlias(repoAlias); !ok {
		return nil, fmt.Errorf("no repo with alias %q (try `ahjo repo add` or `ahjo repo ls`)", repoAlias)
	}

	// Pass the bare alias through; runRepoAdd re-parses it via
	// parseRepoSource and repoAddSetup picks the protocol once the PAT is
	// resolved — HTTPS when a token exists, else SSH-then-HTTPS. Deferring
	// here is what stops `ahjo create owner/repo branch` from grabbing the
	// host SSH key when a PAT is available.
	fmt.Printf("repo %q not registered; adding from GitHub...\n", repoAlias)
	if err := runRepoAdd(ctx, repoAlias, "", "", false, containerConfig); err != nil {
		return nil, err
	}

	reg, err = registry.Load()
	if err != nil {
		return nil, err
	}
	if r := reg.FindRepoByAlias(repoAlias); r != nil {
		return r, nil
	}
	if r := reg.FindRepoByAlias(strings.ToLower(repoAlias)); r != nil {
		return r, nil
	}
	return nil, fmt.Errorf("internal: just-added repo %q not in registry", repoAlias)
}

// splitRepoAlias parses "<owner>/<repo>" — exactly two non-empty
// slash-separated segments, no `@`. Branch aliases (which contain `@`)
// and arbitrary user-provided aliases are rejected so we don't try to
// GitHub-clone them.
func splitRepoAlias(alias string) (owner, repo string, ok bool) {
	if strings.Contains(alias, "@") {
		return "", "", false
	}
	parts := strings.Split(alias, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func pickGitHubURL(owner, name string) string {
	sshURL := fmt.Sprintf("git@github.com:%s/%s.git", owner, name)
	if probeSSHReachable(sshURL) {
		return sshURL
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, name)
}

func probeSSHReachable(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--exit-code", url, "HEAD")
	cmd.Env = append(os.Environ(),
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=3 -o StrictHostKeyChecking=accept-new",
		"GIT_TERMINAL_PROMPT=0",
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// promptRepoGHToken interactively asks for a GitHub PAT to forward into
// containers for this repo. Non-fatal in every skip path: --yes, non-TTY
// stdin, an existing per-repo PAT, and an empty paste all return nil.
//
// We prompt at `ahjo repo add` rather than `ahjo init` because least
// privilege wants a fine-grained PAT scoped to *this* repo — a question only
// answerable once the repo identity exists. Fine-grained PATs cannot be
// API-minted; the user creates them through the GitHub UI.
func promptRepoGHToken(slug, primary string, yes bool) error {
	if _, found, err := repoToken(slug); err != nil {
		return err
	} else if found {
		fmt.Fprintln(cobraOut(), "  → GH_TOKEN already set for this repo; skipping prompt.")
		return nil
	}
	// On Mac users' VM the shim is the canonical writer; an unset token here
	// means the user declined or hasn't been prompted yet by the shim. The
	// in-VM prompt path stays disabled — re-prompting would either land the
	// PAT on disk (the whole point of the Keychain move is to avoid that) or
	// silently no-op via saveRepoGHToken's guard.
	if _, isMac := paths.MacHostHome(); isMac {
		return nil
	}
	if yes {
		return nil
	}
	if !isTerminal(os.Stdin) {
		return nil
	}

	owner, name, ok := splitRepoAlias(primary)
	ownerRepo := ""
	if ok {
		ownerRepo = owner + "/" + name
	}

	out := cobraOut()
	repoauth.PrintInstructions(out, ownerRepo)

	tok, err := readSecret(os.Stdin, out, cobraOutErr(), repoauth.PromptText)
	if err != nil {
		return err
	}
	if tok == "" {
		repoauth.PrintSkipHint(out, primary)
		return nil
	}
	return saveRepoGHToken(slug, tok)
}

// saveRepoGHToken validates tok permissively and writes it to the per-repo
// .env file. The non-canonical hint is printed to stderr but doesn't reject.
//
// On Mac users' VM this path is unreachable: the Mac shim intercepts the
// PAT prompt pre-relay and stores in Keychain instead. We still guard here
// so a future caller can't accidentally land a plaintext PAT on disk —
// returns a clear error rather than silently writing.
func saveRepoGHToken(slug, tok string) error {
	if _, isMac := paths.MacHostHome(); isMac {
		return fmt.Errorf("refusing to write per-repo PAT to disk on macOS — the Mac shim is the canonical writer (Keychain)")
	}
	canonical, hint, err := looksLikeGitHubToken(tok)
	if err != nil {
		return fmt.Errorf("token rejected: %w", err)
	}
	if !canonical && hint != "" {
		fmt.Fprintln(cobraOutErr(), "warn: "+hint)
	}
	envPath := paths.SlugEnvPath(slug)
	if err := tokenstore.SetAt(envPath, tokenstore.GHTokenEnv, tok); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	fmt.Fprintf(cobraOut(), "  → saved to %s\n", envPath)
	return nil
}

func newRepoSetTokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-token <alias>",
		Short: "Set or rotate the GitHub PAT forwarded into containers for one repo",
		Long: `Prompts (with hidden input) for a GitHub token and stores it at
~/.ahjo-shared/repo-env/<slug>.env (mode 0600). On macOS the file lives on the
Mac host (virtiofs-shared into the VM), so PATs survive ` + "`limactl delete`" + `.
The token is forwarded into every container for this repo via GH_TOKEN.

ahjo also re-applies environment.GH_TOKEN/GITHUB_TOKEN on each existing
container (default-branch + every branch). Already-running containers will
need a restart for any currently-attached shells to see the new value;
new ` + "`incus exec`" + ` invocations (and therefore new ` + "`ahjo shell`" + ` / ` + "`ahjo claude`" + `
sessions) pick it up immediately.

Prefer fine-grained PATs scoped to a single repo:
  → ` + repoauth.PATSettingsURL,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRepoSetToken(args[0])
		},
	}
}

func runRepoSetToken(alias string) error {
	reg, err := registry.Load()
	if err != nil {
		return err
	}
	repo := reg.FindRepoByAlias(alias)
	if repo == nil {
		return fmt.Errorf("no repo with alias %q (try `ahjo repo ls`)", alias)
	}
	// On Mac users' VM the shim has already prompted, written Keychain, and
	// forwarded the value via GH_TOKEN. Skip the in-VM prompt + disk write,
	// just re-apply environment.GH_TOKEN to existing containers using the env
	// value. If the env is empty here despite being on Mac, the shim refused
	// to relay — surface that as a defensive error rather than re-prompting
	// (which would route through saveRepoGHToken's Mac guard anyway).
	var tok string
	if _, isMac := paths.MacHostHome(); isMac {
		tok = os.Getenv(tokenstore.GHTokenEnv)
		if tok == "" {
			return fmt.Errorf("on macOS the Mac shim is the canonical path; rerun `ahjo repo set-token %s` outside the VM, or unlock your login Keychain", alias)
		}
	} else {
		tok, err = readSecret(os.Stdin, cobraOut(), cobraOutErr(), fmt.Sprintf("Paste GitHub token for %s: ", repo.Aliases[0]))
		if err != nil {
			return err
		}
		if tok == "" {
			return fmt.Errorf("no token entered")
		}
		if err := saveRepoGHToken(repo.Name, tok); err != nil {
			return err
		}
	}

	// Re-apply environment.GH_TOKEN to every container in this repo (the
	// default-branch container plus each branch). Already-running
	// containers won't pick this up in shells that are already attached;
	// new `incus exec` invocations do. The credential helper line in
	// .gitconfig doesn't depend on the token value, so no second
	// `gh auth setup-git` is needed here.
	containers := repoContainerNames(reg, repo.Name)
	updated := 0
	for _, name := range containers {
		exists, err := incus.ContainerExists(name)
		if err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: probe %s: %v\n", name, err)
			continue
		}
		if !exists {
			continue
		}
		if err := installRepoToken(func(k, v string) error { return incus.ConfigSet(name, k, v) }, tok); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: forward GH_TOKEN to %s: %v\n", name, err)
			continue
		}
		updated++
	}
	if updated > 0 {
		fmt.Fprintf(cobraOut(), "  → forwarded to %d container(s); restart any already-attached shells to pick up the new value\n", updated)
	}
	return nil
}

// installRepoToken pushes the per-repo GH PAT onto a container as
// environment.GH_TOKEN and environment.GITHUB_TOKEN via setter. Both names
// are set because tools split on which one they read: gh prefers GH_TOKEN
// but `git` invoked through gh's credential helper falls through to
// whichever the OAuth helper hands it, and some legacy tooling in
// downstream Features still keys off GITHUB_TOKEN. Setting one without the
// other leaves a confusing half-state where some calls auth and others
// don't.
//
// Returned errors carry the underlying setter error verbatim — the caller
// decides whether a single config-set failure is fatal (repo add) or
// best-effort per container (repo set-token).
func installRepoToken(setter func(key, value string) error, tok string) error {
	for _, k := range []string{"environment.GH_TOKEN", "environment.GITHUB_TOKEN"} {
		if err := setter(k, tok); err != nil {
			return err
		}
	}
	return nil
}
