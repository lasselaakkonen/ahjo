// Package repoauth centralizes the user-facing text shown when ahjo asks
// for a per-repo GitHub PAT. The same block appears on macOS (Mac shim,
// pre-relay, before storing in Keychain) and on Linux (in-VM ahjo, before
// writing the per-repo .env). Keeping it here keeps the two surfaces from
// drifting out of step.
package repoauth

import (
	"fmt"
	"io"
	"net/url"
	"strings"
)

// PATSettingsURL is the bare GitHub page for minting a fine-grained PAT.
// Used as a fallback when we have no owner/repo to prefill from.
const PATSettingsURL = "https://github.com/settings/personal-access-tokens/new"

// PromptText is the single line shown right before stdin is read.
const PromptText = "Paste a token now, or press Enter to skip (gh won't work; git push/fetch works only over SSH): "

// BuildPATURL returns a fine-grained-PAT creation URL with name,
// resource target, permissions, and expiry prefilled from owner/repo. If
// owner is empty, returns the bare PATSettingsURL.
//
// GitHub's template-URL feature has no parameter to preselect a specific
// repository within target_name, so the user still has to toggle
// "Only select repositories" and pick the repo by hand.
// https://docs.github.com/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens#pre-filling-fine-grained-personal-access-token-details-using-url-parameters
func BuildPATURL(owner, repo string) string {
	if owner == "" {
		return PATSettingsURL
	}
	q := url.Values{}
	q.Set("name", "ahjo-"+owner+"-"+repo)
	q.Set("target_name", owner)
	q.Set("expires_in", "365")
	q.Set("contents", "write")
	q.Set("pull_requests", "write")
	q.Set("issues", "write")
	return PATSettingsURL + "?" + q.Encode()
}

// PrintInstructions writes the PAT-setup block to out. ownerRepo is
// "<owner>/<repo>" (e.g. "lasselaakkonen/ahjo"); pass "" to fall back to
// the bare URL plus the long permissions checklist (non-GitHub remote,
// parse failure, etc.).
func PrintInstructions(out io.Writer, ownerRepo string) {
	owner, repo, hasRepo := strings.Cut(ownerRepo, "/")
	patURL := BuildPATURL(owner, repo)

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Set a GitHub token for this repo? gh inside containers can use it.")
	fmt.Fprintln(out)
	if hasRepo && owner != "" {
		fmt.Fprintln(out, "  Fine-grained PAT (recommended):")
		fmt.Fprintln(out, "  ")
		fmt.Fprintln(out, "  "+patURL)
		fmt.Fprintln(out, "  ")
		fmt.Fprintf(out, "    → Toggle \"Only select repositories\" → pick %s\n", repo)
		fmt.Fprintf(out, "    → Generate token.\n")
	} else {
		fmt.Fprintln(out, "  Fine-grained PAT (recommended) — scope it to JUST this repo:")
		fmt.Fprintln(out, "    → "+patURL)
		fmt.Fprintln(out, "        Permissions:")
		fmt.Fprintln(out, "          - Contents       (RW — needed to push commits and merge PRs)")
		fmt.Fprintln(out, "          - Pull requests  (RW)")
		fmt.Fprintln(out, "          - Issues         (RW)")
		fmt.Fprintln(out, "          - Metadata       (read — required)")
		fmt.Fprintln(out, "        Expiration:         your call")
	}
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
