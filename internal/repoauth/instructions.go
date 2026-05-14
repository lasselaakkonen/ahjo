// Package repoauth centralizes the user-facing text shown when ahjo asks
// for a per-repo GitHub PAT. The same block appears on macOS (Mac shim,
// pre-relay, before storing in Keychain) and on Linux (in-VM ahjo, before
// writing the per-repo .env). Keeping it here keeps the two surfaces from
// drifting out of step.
package repoauth

import (
	"fmt"
	"io"
)

// PATSettingsURL is the GitHub page for minting a fine-grained PAT.
const PATSettingsURL = "https://github.com/settings/personal-access-tokens/new"

// PromptText is the single line shown right before stdin is read.
const PromptText = "Paste a token now, or press Enter to skip (gh won't work; git push/fetch works only over SSH): "

// PrintInstructions writes the PAT-setup block to out. ownerRepo is
// "<owner>/<repo>" (e.g. "lasselaakkonen/ahjo"); pass "" to omit the
// "Only select repositories" scoping line when the caller hasn't derived
// owner/repo (non-GitHub remote, parse failure, etc.).
func PrintInstructions(out io.Writer, ownerRepo string) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Set a GitHub token for this repo? gh inside containers can use it.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Fine-grained PAT (recommended) — scope it to JUST this repo:")
	fmt.Fprintln(out, "    → "+PATSettingsURL)
	if ownerRepo != "" {
		fmt.Fprintf(out, "        Repository access:  Only select repositories → %s\n", ownerRepo)
	}
	fmt.Fprintln(out, "        Permissions:")
	fmt.Fprintln(out, "          - Contents       (RW — needed to push commits and merge PRs)")
	fmt.Fprintln(out, "          - Pull requests  (RW)")
	fmt.Fprintln(out, "          - Issues         (RW)")
	fmt.Fprintln(out, "          - Metadata       (read — required)")
	fmt.Fprintln(out, "        Expiration:         your call")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Classic PAT — broader, easier, less safe.")
	fmt.Fprintln(out)
}

// PrintSkipHint writes the "you skipped — add later" hint. primaryAlias is
// the user-facing handle for `ahjo repo set-token <alias>`.
func PrintSkipHint(out io.Writer, primaryAlias string) {
	fmt.Fprintln(out, "  → skipped. `gh` inside containers for this repo will require manual auth.")
	fmt.Fprintln(out, "     Add later:  ahjo repo set-token "+primaryAlias)
	fmt.Fprintln(out, "     Or globally (warning: exposes all your repos):")
	fmt.Fprintln(out, "       ahjo env set GH_TOKEN \"$(gh auth token)\"")
}
