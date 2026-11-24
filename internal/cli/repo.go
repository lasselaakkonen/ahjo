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

	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
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
// the two backends. Precedence:
//
//  1. Standalone Linux (MacHostHome() false): the per-repo .env file under
//     SharedDir(), when it holds a non-empty GH_TOKEN. A repo-scoped PAT
//     beats a global one, matching branchEnv's precedence.
//  2. GH_TOKEN from the process env or ~/.ahjo/.env (tokenstore.Lookup).
//     On Mac users' VM the process env is the only source:
//     the Mac shim reads Keychain pre-relay and hands the value to the
//     in-VM process, which never touches a disk path, so a script grepping
//     ~ for `ghp_*` finds nothing. On Linux it is the global fallback
//     (`ahjo env set GH_TOKEN`).
//  3. Otherwise not found.
//
// The token is never stored on a container; callers inject it per exec via
// withRepoToken.
func repoToken(slug string) (string, bool, error) {
	if _, isMac := paths.MacHostHome(); !isMac && slug != "" {
		v, found, err := tokenstore.GetAt(paths.SlugEnvPath(slug), tokenstore.GHTokenEnv)
		if err != nil {
			return "", false, err
		}
		if found && v != "" {
			return v, true, nil
		}
	}
	if v, _ := tokenstore.Lookup(tokenstore.GHTokenEnv); v != "" {
		return v, true, nil
	}
	return "", false, nil
}

// withRepoToken returns env with the repo PAT added for one `incus exec`.
// tok == "" returns env unchanged. An existing non-empty GH_TOKEN in env is
// kept; GITHUB_TOKEN is set to the resulting GH_TOKEN unless env already has
// a non-empty GITHUB_TOKEN. Allocates a map when env is nil. Both names are
// set because gh reads GH_TOKEN while other tooling reads GITHUB_TOKEN.
func withRepoToken(env map[string]string, tok string) map[string]string {
	if tok == "" {
		return env
	}
	if env == nil {
		env = map[string]string{}
	}
	if env[tokenstore.GHTokenEnv] == "" {
		env[tokenstore.GHTokenEnv] = tok
	}
	if env["GITHUB_TOKEN"] == "" {
		env["GITHUB_TOKEN"] = env[tokenstore.GHTokenEnv]
	}
	return env
}

// repoTokenEnvArgs renders withRepoToken(nil, tok) as `incus exec` argv
// (`--env GH_TOKEN=… --env GITHUB_TOKEN=…`) for the call sites that build
// their own `incus exec` command line instead of going through
// incus.ExecAs. Returns nil when tok is empty. Callers must never format
// the resulting argv into an error or log line.
func repoTokenEnvArgs(tok string) []string {
	if tok == "" {
		return nil
	}
	env := withRepoToken(nil, tok)
	return []string{
		"--env", tokenstore.GHTokenEnv + "=" + env[tokenstore.GHTokenEnv],
		"--env", "GITHUB_TOKEN=" + env["GITHUB_TOKEN"],
	}
}

// persistedTokenKeys are the Incus config keys older ahjo versions wrote the
// repo PAT to. ahjo no longer sets them; they are only ever read and unset.
var persistedTokenKeys = []string{"environment.GH_TOKEN", "environment.GITHUB_TOKEN"}

// scrubPersistedToken removes PAT keys older ahjo versions stored on the
// container. Returns true when it removed anything. get and unset are the
// container-bound accessors (incus.ConfigGet / incus.ConfigUnset in
// production), injectable so the decision logic is unit-testable.
func scrubPersistedToken(get func(key string) (string, error), unset func(key string) error) (bool, error) {
	removed := false
	for _, k := range persistedTokenKeys {
		v, err := get(k)
		if err != nil {
			return removed, err
		}
		if v == "" {
			continue
		}
		if err := unset(k); err != nil {
			return removed, err
		}
		removed = true
	}
	return removed, nil
}

// scrubPersistedRepoToken runs scrubPersistedToken against each named
// container that exists. Best-effort: failures are warnings on
// cobraOutErr(). Prints one line per container it changed. Empty and
// duplicate names are skipped; a missing container is skipped silently so
// `incus config get` never prints "not found" noise.
func scrubPersistedRepoToken(names ...string) {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		exists, err := incus.ContainerExists(name)
		if err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: probe %s: %v\n", name, err)
			continue
		}
		if !exists {
			continue
		}
		removed, err := scrubPersistedToken(
			func(k string) (string, error) { return incus.ConfigGet(name, k) },
			func(k string) error { return incus.ConfigUnset(name, k) },
		)
		if err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: remove persisted GH_TOKEN from %s: %v\n", name, err)
		}
		if removed {
			fmt.Fprintf(cobraOut(), "→ removed persisted GH_TOKEN from %s config (now injected per session)\n", name)
		}
	}
}

// scrubLegacyRepoToken is scrubPersistedRepoToken for call sites that go on
// to apply the ssh-agent policy. The persisted key is one of repoHasToken's
// inputs (it identifies PAT repos registered before has_token existed), so
// record has_token, in the registry and on repo, before removing it. Must be
// called without the lockfile held.
func scrubLegacyRepoToken(repo *registry.Repo, names ...string) {
	if repo != nil && !repo.HasToken && repoHasToken(repo) {
		syncRepoHasToken(repo.Name)
		repo.HasToken = true
	}
	scrubPersistedRepoToken(names...)
}

// syncRepoHasToken backfills the registry's HasToken flag for repoName when a
// PAT is visible now (repoHasToken) but the row predates the flag or was
// added without one. Recording it lets the generated ssh-config, which only
// trusts the registry, suppress ForwardAgent for the repo too.
//
// Best-effort: failures warn and return. Takes the lockfile itself, so it
// must only be called from code that does not already hold it.
func syncRepoHasToken(repoName string) {
	reg, err := registry.Load()
	if err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: record has_token for %s: %v\n", repoName, err)
		return
	}
	repo := reg.FindRepo(repoName)
	if repo == nil || repo.HasToken || !repoHasToken(repo) {
		return
	}
	release, err := lockfile.Acquire()
	if err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: record has_token for %s: %v\n", repoName, err)
		return
	}
	defer release()
	if _, err := setRepoHasTokenLocked(repoName); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: record has_token for %s: %v\n", repoName, err)
	}
}

// setRepoHasTokenLocked reloads the registry, sets HasToken on repoName,
// saves, and regenerates the ssh-config. The caller holds the lockfile.
// Returns the reloaded registry. A repo that vanished since the caller looked
// is not an error (the caller checks the returned registry if it cares); one
// already flagged is left untouched.
func setRepoHasTokenLocked(repoName string) (*registry.Registry, error) {
	reg, err := registry.Load()
	if err != nil {
		return nil, err
	}
	repo := reg.FindRepo(repoName)
	if repo == nil || repo.HasToken {
		return reg, nil
	}
	repo.HasToken = true
	if err := reg.Save(); err != nil {
		return nil, err
	}
	return reg, regenerateSSHConfig(reg)
}

func newRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Manage the repo registry",
	}
	cmd.AddCommand(newRepoAddCmd(), newRepoLsCmd(), newRepoRmCmd(), newRepoPullCmd(), newRepoSetTokenCmd(), newRepoApproveEnvCmd())
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
	// Zero elevationFlags: on a TTY the user gets the nesting /
	// nested_incus prompts; off a TTY the gate's error tells them to run
	// `ahjo repo add ... --allow-...` first. `create` deliberately has no
	// --allow-* flags of its own.
	if err := runRepoAdd(ctx, repoAlias, "", "", false, containerConfig, elevationFlags{}); err != nil {
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
The token is injected as GH_TOKEN / GITHUB_TOKEN into each ahjo-started
process for this repo (` + "`ahjo shell`" + `, ` + "`ahjo claude`" + `, lifecycle hooks, and
ahjo's own git fetch/pull) for that exec only. It is never stored in a
container's Incus config. ahjo also removes the token from the config of any
container an older ahjo version stored it on.

The new value is used from the next ` + "`ahjo shell`" + ` / ` + "`ahjo claude`" + ` / ` + "`ahjo create`" + ` /
` + "`ahjo repo pull`" + `. Already-attached shells keep the old value.

For an HTTPS GitHub origin the token replaces the host ssh-agent: ahjo
records that the repo has a PAT, removes the ssh-agent device from every
existing container of the repo, and regenerates the ssh-config so
` + "`ahjo ssh`" + ` / ` + "`ahjo ide`" + ` stop forwarding the agent too. Override with
` + "`forward_ssh_agent`" + ` in config.toml.

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
	// relayed the value into this process's env. Skip the in-VM prompt + disk
	// write. If the env is empty here despite being on Mac, the shim refused
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

	// Record the PAT in the registry (the container-proof input to the
	// ssh-agent policy) and regenerate the ssh-config so ahjo ssh / ahjo ide
	// stop forwarding the agent when policy now suppresses it. Lock released
	// before the slow per-container Incus loop.
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	reg, err = setRepoHasTokenLocked(repo.Name)
	release()
	if err != nil {
		return fmt.Errorf("record has_token: %w", err)
	}
	repo = reg.FindRepo(repo.Name)
	if repo == nil {
		return fmt.Errorf("repo %q was removed while setting its token", alias)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: load config (using auto ssh-agent policy): %v\n", err)
		cfg = nil
	}
	suppress := !shouldForwardAgent(repo, true, cfg)

	// Nothing to push onto containers: the token is injected per exec. Only
	// remove the copy older ahjo versions persisted in Incus config (after
	// has_token is recorded above, since that copy was one of its inputs).
	//
	// When the policy now suppresses the agent, also strip the ssh-agent
	// device from each container, running or stopped, so neither an attached
	// session nor the next start keeps it. A container whose strip could not
	// be confirmed fails the command (fail closed).
	containers := repoContainerNames(reg, repo.Name)
	stripped := 0
	var stripFailed []string
	for _, name := range containers {
		exists, err := incus.ContainerExists(name)
		if err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: probe %s: %v\n", name, err)
			if suppress {
				stripFailed = append(stripFailed, name)
			}
			continue
		}
		if !exists {
			continue
		}
		if suppress {
			if err := stripSSHAgent(name); err != nil {
				fmt.Fprintf(cobraOutErr(), "warn: %v\n", err)
				stripFailed = append(stripFailed, name)
			} else {
				stripped++
			}
		}
	}
	scrubPersistedRepoToken(containers...)
	fmt.Fprintln(cobraOut(), "  → token saved; used from the next ahjo shell / claude / create / repo pull. Already-attached shells keep the old value.")
	if stripped > 0 {
		fmt.Fprintf(cobraOut(), "  → ssh-agent removed from %d container(s) (HTTPS+PAT origin)\n", stripped)
	}
	if len(stripFailed) > 0 {
		return fmt.Errorf("could not remove the ssh-agent device from %s; the agent may still be reachable there — fix Incus and rerun `ahjo repo set-token %s`",
			strings.Join(stripFailed, ", "), alias)
	}
	return nil
}
