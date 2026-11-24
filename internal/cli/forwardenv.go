package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/ahjocontainer"
)

// Consent gate for customizations.ahjo.forward_env.
//
// Trust model: the global config.ForwardEnv (~/.ahjo/config.toml) and a
// repo's registry.Repo.ApprovedForwardEnv (~/.ahjo/registry.toml) live
// outside the container and are trusted. The names requested by
// /repo/.ahjo/ahjocontainer.json are controlled by the repo author and by
// anything running in the container, so they are untrusted: a requested name
// is forwarded only when one of the trusted lists already contains it, and it
// is never printed unless it passes ahjocontainer.ValidEnvName (invalid names
// are only ever shown %q-quoted, which keeps terminal escape sequences out of
// prompts and hints).

// partitionForwardEnv splits repo-requested names. global and approved are
// trusted (host-side); requested is untrusted (from ahjocontainer.json).
// allowed = requested ∩ (global ∪ approved); pending = valid but in neither;
// invalid = fails ahjocontainer.ValidEnvName. Each output is de-duplicated
// and keeps first-seen order.
func partitionForwardEnv(global, approved, requested []string) (allowed, pending, invalid []string) {
	trusted := make(map[string]struct{}, len(global)+len(approved))
	for _, k := range global {
		trusted[k] = struct{}{}
	}
	for _, k := range approved {
		trusted[k] = struct{}{}
	}
	seen := make(map[string]struct{}, len(requested))
	for _, k := range requested {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		if !ahjocontainer.ValidEnvName(k) {
			invalid = append(invalid, k)
			continue
		}
		if _, ok := trusted[k]; ok {
			allowed = append(allowed, k)
		} else {
			pending = append(pending, k)
		}
	}
	return allowed, pending, invalid
}

// forwardEnvKeys returns global ∪ allowed, de-duplicated, global first.
// Empty names are dropped.
func forwardEnvKeys(global, allowed []string) []string {
	out := make([]string, 0, len(global)+len(allowed))
	seen := make(map[string]struct{}, len(global)+len(allowed))
	for _, list := range [][]string{global, allowed} {
		for _, k := range list {
			if k == "" {
				continue
			}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	return out
}

// resolveForwardEnv looks each key up in repoEnv (may be nil; the per-repo
// env file, highest precedence), then via lookup (tokenstore.Resolver:
// process env, then ~/.ahjo/.env). Keys with no value anywhere are omitted.
// Always returns a non-nil map.
func resolveForwardEnv(keys []string, repoEnv map[string]string, lookup func(string) (string, bool)) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		if v, ok := repoEnv[k]; ok {
			out[k] = v
			continue
		}
		if lookup == nil {
			continue
		}
		if v, ok := lookup(k); ok {
			out[k] = v
		}
	}
	return out
}

// promptForwardEnvConsent asks one [y/N] per pending name and returns the
// approved subset. interactive=false approves nothing, never reads in, and
// prints the skip hint instead. isSet reports whether the host currently has
// a value (shown as "(set)" / "(not set)" next to each name, never the
// value). Anything other than y/yes, including EOF, declines; declined names
// get the same hint so the user knows how to approve them later. pending
// must already be validated (partitionForwardEnv's pending output).
func promptForwardEnvConsent(repoAlias string, pending []string, interactive bool, isSet func(string) bool, in io.Reader, out io.Writer) []string {
	if len(pending) == 0 {
		return nil
	}
	if !interactive {
		fmt.Fprint(out, forwardEnvHint(repoAlias, pending))
		return nil
	}
	labels := make([]string, len(pending))
	for i, k := range pending {
		state := "not set"
		if isSet != nil && isSet(k) {
			state = "set"
		}
		labels[i] = fmt.Sprintf("%s (%s)", k, state)
	}
	fmt.Fprintln(out, "Repo config asks ahjo to forward these host environment variables into its containers:")
	fmt.Fprintf(out, "  %s\n", strings.Join(labels, ", "))
	fmt.Fprintln(out, "Anything running in the container can read forwarded values.")
	reader := bufio.NewReader(in)
	var approved, declined []string
	for _, k := range pending {
		fmt.Fprintf(out, "Forward %s to this repo's containers? [y/N] ", k)
		line, err := reader.ReadString('\n')
		ans := strings.ToLower(strings.TrimSpace(line))
		if err != nil && line == "" {
			// EOF / read error: decline this and every remaining name.
			fmt.Fprintln(out)
			ans = ""
		}
		if ans == "y" || ans == "yes" {
			approved = append(approved, k)
		} else {
			declined = append(declined, k)
		}
	}
	if len(declined) > 0 {
		fmt.Fprint(out, forwardEnvHint(repoAlias, declined))
	}
	return approved
}

// forwardEnvHint formats the skip message for requested-but-unapproved
// names (two lines, newline-terminated):
//
//	note: ahjocontainer.json requests env vars not approved for this repo: A, B — not forwarded.
//	      approve with: ahjo repo approve-env <repoAlias> A B
//
// pending must already be validated. An empty repoAlias (no registry row)
// prints a <repo-alias> placeholder.
func forwardEnvHint(repoAlias string, pending []string) string {
	if len(pending) == 0 {
		return ""
	}
	if repoAlias == "" {
		repoAlias = "<repo-alias>"
	}
	return fmt.Sprintf(
		"note: ahjocontainer.json requests env vars not approved for this repo: %s — not forwarded.\n"+
			"      approve with: ahjo repo approve-env %s %s\n",
		strings.Join(pending, ", "), repoAlias, strings.Join(pending, " "))
}

// warnInvalidForwardEnv reports each invalid requested name once, %q-quoted
// so control characters from the untrusted file are escaped.
func warnInvalidForwardEnv(w io.Writer, invalid []string) {
	for _, k := range invalid {
		fmt.Fprintf(w, "warn: ignoring invalid forward_env name %q\n", k)
	}
}

// forwardEnvSkip is what branchEnv / refreshHostEnv could not forward: the
// requested-but-unapproved names (already validated) and the invalid ones.
type forwardEnvSkip struct {
	RepoAlias string
	Pending   []string
	Invalid   []string
}

// report prints the invalid-name warnings and the approve hint to w. Callers
// that build the env more than once per command (prepareBranchContainer's
// postStart pass, then the attach) report only once.
func (s forwardEnvSkip) report(w io.Writer) {
	warnInvalidForwardEnv(w, s.Invalid)
	fmt.Fprint(w, forwardEnvHint(s.RepoAlias, s.Pending))
}

// requestedForwardEnv unions customizations.ahjo.forward_env across confs
// (nil entries allowed), keeping first-seen order. The result is untrusted.
func requestedForwardEnv(confs ...*ahjocontainer.Config) []string {
	var out []string
	for _, c := range confs {
		if c == nil {
			continue
		}
		out = append(out, c.Customizations.Ahjo.ForwardEnv...)
	}
	return out
}
