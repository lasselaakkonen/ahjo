package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/ahjocontainer"
	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	"github.com/lasselaakkonen/ahjo/internal/tokenstore"
)

func newRepoPullCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pull <repo-alias>",
		Short: "git pull --ff-only in the repo's default-branch container",
		Long: `Updates the default-branch container (the COW source for every branch
container in this repo) against origin. Starts the container if it was
stopped, pulls fast-forward only, and leaves it running. Failures surface
verbatim from git — no silent recovery.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoPull(cmd.Context(), args[0])
		},
	}
}

// newRefreshBaseCmd is the hidden subcommand `ahjo rm` spawns detached after a
// non-default branch is removed: it starts the repo's base container if
// stopped, and runs `git pull --ff-only` when /repo is clean so the next
// `ahjo create` COWs from a base that's already in sync with origin. Held
// under the ahjo lockfile, so a follow-up create waits for it to finish
// instead of snapshotting a half-pulled tree.
//
// Takes the repo *slug* (registry Name), not an alias — rm has the slug on
// hand and a slug→repo lookup is unambiguous.
func newRefreshBaseCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_refresh-base <repo-name>",
		Short:  "internal: pre-warm a repo's base container with `git pull` after rm",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return refreshRepoBase(args[0])
		},
	}
}

// Per-step ceilings for the background refresh. Tuned for the worst-case path:
// a cold-VM start on macOS + a real cross-network git pull + a cold-cache
// `npm ci`/`cargo fetch`, with headroom. The hard outer ceiling caps the
// *whole* run so a wedged step (incus daemon hung, git or npm stalled on a
// credential prompt that never fires) can't hold the ahjo lockfile
// indefinitely and block the user's next command.
const (
	refreshLockTimeout   = 2 * time.Minute
	refreshReadyTimeout  = 90 * time.Second
	refreshPullTimeout   = 3 * time.Minute
	refreshWarmTimeout   = 10 * time.Minute
	refreshOverallBudget = 20 * time.Minute
)

func refreshRepoBase(repoName string) (retErr error) {
	// Overall ceiling + SIGINT/SIGTERM cancellation in one context. When this
	// fires (timeout or signal), the git pull subprocess gets killed by
	// CommandContext, the function unwinds normally, and the deferred lock
	// release runs — no flock leak even on a forceful shutdown. (And if the
	// kernel SIGKILLs us anyway, flock auto-releases on process exit.)
	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, refreshOverallBudget)
	defer cancel()

	start := time.Now()
	logStep("refresh-base start repo=%s", repoName)
	defer func() {
		switch {
		case retErr != nil:
			logStep("refresh-base FAIL repo=%s elapsed=%s err=%v",
				repoName, time.Since(start).Round(time.Millisecond), retErr)
		default:
			logStep("refresh-base OK repo=%s elapsed=%s",
				repoName, time.Since(start).Round(time.Millisecond))
		}
	}()

	// Bounded lock wait — the parent rm holds the lock through `incus delete
	// --force`, and other ahjo commands may queue ahead of us, so we tolerate
	// up to refreshLockTimeout before giving up. Releasing is unconditional
	// via defer — covers normal return, error return, and panic.
	release, err := lockfile.AcquireWithTimeout(refreshLockTimeout)
	if err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer release()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cancelled before work: %w", err)
	}

	reg, err := registry.Load()
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	repo := reg.FindRepo(repoName)
	if repo == nil {
		return fmt.Errorf("no repo with name %q (gone since rm?)", repoName)
	}
	if repo.BaseContainerName == "" {
		return fmt.Errorf("repo %q has no base container", repo.Aliases[0])
	}

	status, err := incus.ContainerStatus(repo.BaseContainerName)
	if err != nil {
		return fmt.Errorf("status %s: %w", repo.BaseContainerName, err)
	}
	if status == "" {
		return fmt.Errorf("base container %s not found (deleted out-of-band?)", repo.BaseContainerName)
	}
	if !strings.EqualFold(status, "Running") {
		// Strip before start so the base never boots with a stored ssh-agent
		// device, then re-apply policy: an SSH-origin pull needs the agent.
		if err := stripSSHAgent(repo.BaseContainerName); err != nil {
			return err
		}
		logStep("→ incus start %s (was %s)", repo.BaseContainerName, status)
		if err := startContainer(repo.BaseContainerName); err != nil {
			return fmt.Errorf("start %s: %w", repo.BaseContainerName, err)
		}
		if err := incus.WaitReady(ctx, repo.BaseContainerName, refreshReadyTimeout); err != nil {
			return fmt.Errorf("wait %s ready: %w", repo.BaseContainerName, err)
		}
		if err := applyBaseSSHAgentPolicy(ctx, repo); err != nil {
			if errors.Is(err, errAgentStrip) {
				return err
			}
			logStep("warn: %s: %v (git pull may fail to authenticate)", repo.BaseContainerName, err)
		}
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cancelled before clean-check: %w", err)
	}

	summary, err := repoDirtySummary(repo.BaseContainerName)
	if err != nil {
		return fmt.Errorf("inspect /repo in %s: %w", repo.BaseContainerName, err)
	}
	if summary != "" {
		logStep("skipping git pull in %s: %s", repo.BaseContainerName, summary)
		return nil
	}

	tok, _, _ := repoToken(repo.Name)
	logStep("→ git pull --ff-only (in %s)", repo.BaseContainerName)
	if err := execGitPullFFOnly(ctx, repo.BaseContainerName, tok); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cancelled before warm-install: %w", err)
	}

	// Warm the dep cache too — same installer table `repo add` uses, so
	// branch containers COW'd from this base inherit hot node_modules / uv
	// cache / cargo registry, not just current commits. Best-effort: a
	// failing `npm ci` (lockfile/registry-version drift, private-registry
	// auth missing in the subprocess env) shouldn't roll back the pull or
	// fail the overall refresh — log it and let the next `ahjo create`
	// surface the real installer error in the user's foreground.
	hostEnv := withRepoToken(refreshHostEnv(repo), tok)
	warmCtx, cancelWarm := context.WithTimeout(ctx, refreshWarmTimeout)
	defer cancelWarm()
	if err := execWarmInstallCtx(warmCtx, repo.BaseContainerName, hostEnv); err != nil {
		logStep("warm-install in %s failed (best-effort): %v", repo.BaseContainerName, err)
	}
	return nil
}

// refreshHostEnv reconstructs the env map runWarmInstall expects, mirroring
// the resolution done at `repo add` time so the same secrets (NPM_TOKEN, …)
// reach the same installers. Three sources, in precedence order:
//
//  1. Global config.ForwardEnv keys.
//  2. dcConf.Customizations.Ahjo.ForwardEnv keys, but only those also in
//     repo.ApprovedForwardEnv (registry, trusted). The in-container
//     ahjocontainer.json is untrusted: it can request names, not grant them.
//     Unapproved / invalid names are logged with logStep and skipped — this
//     runs detached, so it never prompts.
//  3. dcConf.ContainerEnv literal values, but only when the key wasn't
//     already populated by (1) or (2) — host env wins over hard-coded values,
//     matching repo.go's merge.
//
// (1) and (2) resolve through tokenstore.Resolver: the subprocess's own
// environment (inherited from the user's shell through `ahjo rm`, so a PAT
// exported in the shell flows through unchanged), then ~/.ahjo/.env.
//
// dcConf load failures are non-fatal: we fall back to global ForwardEnv only,
// log a note, and let warm-install run with whatever env we have. The
// container's own `environment.*` config keys (set at `repo add` time) still
// apply to every incus exec regardless. The repo PAT is not one of them: the
// caller adds it with withRepoToken.
func refreshHostEnv(repo *registry.Repo) map[string]string {
	var global []string
	if cfg, err := config.Load(); err != nil {
		logStep("refresh-hostenv: load global config: %v (continuing with empty ForwardEnv)", err)
	} else {
		global = cfg.ForwardEnv
	}

	dcConf, _, dcErr := ahjocontainer.LoadFromContainer(repo.BaseContainerName)
	if dcErr != nil {
		logStep("refresh-hostenv: load devcontainer config: %v (continuing without dcConf-specific env)", dcErr)
		dcConf = nil
	}

	allowed, pending, invalid := partitionForwardEnv(global, repo.ApprovedForwardEnv, requestedForwardEnv(dcConf))
	for _, k := range invalid {
		logStep("refresh-hostenv: ignoring invalid forward_env name %q", k)
	}
	if len(pending) > 0 {
		alias := repo.Name
		if len(repo.Aliases) > 0 {
			alias = repo.Aliases[0]
		}
		logStep("refresh-hostenv: not forwarding unapproved forward_env names %s (approve with: ahjo repo approve-env %s %s)",
			strings.Join(pending, ", "), alias, strings.Join(pending, " "))
	}

	lookup, lerr := tokenstore.Resolver()
	if lerr != nil {
		logStep("refresh-hostenv: read %s: %v (continuing with process env only)", tokenstore.Path(), lerr)
	}
	hostEnv := resolveForwardEnv(forwardEnvKeys(global, allowed), nil, lookup)
	if dcConf != nil {
		for k, v := range dcConf.ContainerEnv {
			if _, set := hostEnv[k]; set {
				continue
			}
			hostEnv[k] = v
		}
	}
	return hostEnv
}

// execWarmInstallCtx wraps runWarmInstall with context cancellation: a
// hanging `npm ci` (private registry timing out, lockfile asking for a
// removed package version, etc.) gets killed when ctx expires instead of
// holding the ahjo lockfile past the warm budget.
//
// runWarmInstall now threads ctx into incus.ExecAsContext, so a canceled ctx
// kills the in-flight `incus exec` directly. We still run it in a goroutine and
// race ctx.Done() so this returns promptly (releasing the ahjo lockfile)
// without waiting for the killed child to be reaped.
func execWarmInstallCtx(ctx context.Context, containerName string, hostEnv map[string]string) error {
	done := make(chan error, 1)
	go func() {
		done <- runWarmInstall(ctx, containerName, hostEnv)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("warm-install cancelled (%s budget / signal): %w", refreshWarmTimeout, ctx.Err())
	}
}

// execGitPullFFOnly runs `git pull --ff-only` inside container as uid 1000,
// killed by the parent ctx (overall timeout or SIGINT/SIGTERM) so a stalled
// pull can't outlive the budget. Stdout/stderr are inherited so the spawned-
// detached log file captures git's progress and any error text verbatim.
//
// Mirrors incus.ExecAs's argv layout (--user, --cwd, --) but uses
// exec.CommandContext for cancellation; the per-pull deadline is layered
// onto ctx so the rest of the function can still observe a separate parent
// cancel.
//
// tok, when non-empty, is passed as --env GH_TOKEN/GITHUB_TOKEN for this exec
// only. The returned errors never format the argv, so the token cannot leak
// into the refresh-base log.
func execGitPullFFOnly(parentCtx context.Context, container, tok string) error {
	ctx, cancel := context.WithTimeout(parentCtx, refreshPullTimeout)
	defer cancel()

	args := []string{"exec", container, "--user", "1000", "--cwd", paths.RepoMountPath}
	args = append(args, repoTokenEnvArgs(tok)...)
	args = append(args, "--", "git", "pull", "--ff-only")
	cmd := exec.CommandContext(ctx, "incus", args...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("git pull cancelled (%s budget / signal): %w", refreshPullTimeout, ctxErr)
	}
	return fmt.Errorf("git pull --ff-only in %s: %w", container, err)
}

// applyBaseSSHAgentPolicy runs applySSHAgentPolicy on repo's (running) base
// container for `repo pull` and the background refresh. A config.toml that
// fails to load means the auto policy. The caller already holds the
// lockfile, so this never backfills has_token (syncRepoHasToken would
// deadlock on the lock).
func applyBaseSSHAgentPolicy(ctx context.Context, repo *registry.Repo) error {
	cfg, err := config.Load()
	if err != nil {
		cfg = nil
	}
	return applySSHAgentPolicy(ctx, repo.BaseContainerName, repo, cfg)
}

// logStep prints one timestamped line to whatever stdout the detached
// subprocess was wired to (the refresh-base log file under ~/.ahjo/). Kept
// flat (no level/structured fields) because the consumer is `tail -f`.
func logStep(format string, args ...any) {
	fmt.Printf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"),
		fmt.Sprintf(format, args...))
}

func runRepoPull(ctx context.Context, repoAlias string) error {
	// Serialize against the detached `_refresh-base` that `ahjo rm <branch>`
	// spawns: it holds the lockfile while running its own `git pull --ff-only`
	// (and warm-install) in this same base container's /repo. Without the lock,
	// two concurrent pulls race on FETCH_HEAD/index.lock — the foreground one
	// then dies with "Cannot fast-forward to multiple branches" or a stale
	// index.lock. Acquiring here makes the user's pull wait for the background
	// refresh to finish instead of corrupting its tree.
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return err
	}
	repo := reg.FindRepoByAlias(repoAlias)
	if repo == nil {
		return fmt.Errorf("no repo with alias %q (try `ahjo repo ls`)", repoAlias)
	}
	if repo.BaseContainerName == "" {
		return fmt.Errorf("repo %q has no base container; re-add it with `ahjo repo add`", repo.Aliases[0])
	}

	status, err := incus.ContainerStatus(repo.BaseContainerName)
	if err != nil {
		return err
	}
	if !strings.EqualFold(status, "Running") {
		// Strip before start so the base never boots with a stored ssh-agent
		// device, then re-apply policy: an SSH-origin pull needs the agent.
		// An already-running base keeps its device as is.
		if err := stripSSHAgent(repo.BaseContainerName); err != nil {
			return err
		}
		fmt.Printf("→ incus start %s\n", repo.BaseContainerName)
		if err := startContainer(repo.BaseContainerName); err != nil {
			return err
		}
		if err := incus.WaitReady(ctx, repo.BaseContainerName, 30*time.Second); err != nil {
			return err
		}
		if err := applyBaseSSHAgentPolicy(ctx, repo); err != nil {
			if errors.Is(err, errAgentStrip) {
				return err
			}
			fmt.Fprintf(cobraOutErr(), "warn: %s: %v (git pull may fail to authenticate)\n", repo.BaseContainerName, err)
		}
	}

	fmt.Printf("→ git pull --ff-only (in %s)\n", repo.BaseContainerName)
	tok, _, _ := repoToken(repo.Name)
	return incus.ExecAsContext(
		ctx, repo.BaseContainerName, 1000, withRepoToken(nil, tok), paths.RepoMountPath,
		"git", "pull", "--ff-only",
	)
}
