package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/paths"
)

func newClaudeCmd() *cobra.Command {
	var update bool
	var force bool
	cmd := &cobra.Command{
		Use:   "claude <alias>",
		Short: "Start (if needed) and launch `claude` inside the branch's container",
		Long: `Start the container if needed, wire SSH proxy + sshd, then ` +
			"`incus exec --force-interactive`" + ` directly into ` + "`claude`" + ` as the
in-container ` + "`ubuntu`" + ` user in /repo. Use ` + "`ahjo shell`" + ` for an interactive
shell instead.

Pass --update to discard the existing container before attaching: ahjo stops
it, deletes it, and recreates it from the repo's default-branch container.

Before recreating with --update, ahjo inspects /repo for uncommitted/unpushed
work (starting a stopped container for the check, after prompting). If /repo
is dirty — or the user declines the start prompt — the command refuses to
proceed; pass --force to skip the check and recreate anyway.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runClaude(args[0], update, force)
		},
	}
	cmd.Flags().BoolVar(&update, "update", false, "destroy the existing container before attaching so it picks up the current ahjo-base image")
	cmd.Flags().BoolVar(&force, "force", false, "with --update, skip the /repo cleanliness check and recreate even when uncommitted/unpushed work is present")
	return cmd
}

func runClaude(alias string, update, force bool) error {
	br, containerName, err := prepareBranchContainer(alias, update, force)
	if err != nil {
		return err
	}
	dcConf, err := loadDevcontainerSafe(containerName)
	if err != nil {
		return err
	}
	env, err := branchEnv(containerName, dcConf)
	if err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: collect forward env: %v\n", err)
	}
	if err := runPostAttach(containerName, dcConf, env); err != nil {
		return err
	}
	_ = br
	model := promptStartingModel(cobraOut(), os.Stdin)
	// Launch through `bash -lc 'exec claude …'` so ~/.profile fires and
	// ~/.local/bin lands on PATH — otherwise claude's self-check ("native
	// install exists but ~/.local/bin not in PATH") prints on every start.
	// `exec` replaces bash with claude so signals + exit codes still pass
	// through unchanged.
	return incus.ExecAttach(containerName, 1000, env, paths.RepoMountPath,
		"bash", "-lc", `exec claude --dangerously-skip-permissions --model "$1"`, "bash", model)
}

// promptStartingModel asks which alias to pass to `claude` via --model on
// launch. ahjo's auth path (CLAUDE_CODE_OAUTH_TOKEN, minted by `claude
// setup-token` and forwarded via `forward_env`) doesn't propagate Max-tier
// metadata into claude's /model picker, so the 1M Opus entry is hidden even
// on a Max plan — see https://github.com/anthropics/claude-code/issues/5625.
// Asking here is a per-session pick without touching settings.json. Returns
// "opus[1m]" on non-TTY stdin or any read failure so piped invocations don't
// hang.
func promptStartingModel(out io.Writer, in *os.File) string {
	if !isTerminal(in) {
		return "opus[1m]"
	}
	fmt.Fprintln(out, "ahjo asks because anthropics/claude-code#5625: long-lived OAuth tokens")
	fmt.Fprintln(out, "don't surface the Max-tier 1M Opus entry in claude's /model picker.")
	fmt.Fprintln(out, "  https://github.com/anthropics/claude-code/issues/5625")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Pick a starting model:")
	fmt.Fprintln(out, "  1) opus[1m]   — Opus 4.7 with 1M context window")
	fmt.Fprintln(out, "  2) opusplan   — Opus for plan, Sonnet for execution (200K)")
	fmt.Fprint(out, "Choice [1/2, default 1]: ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.TrimSpace(line) {
	case "2", "opusplan":
		return "opusplan"
	}
	return "opus[1m]"
}

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
