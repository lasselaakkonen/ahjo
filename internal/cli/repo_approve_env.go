package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/ahjocontainer"
	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func newRepoApproveEnvCmd() *cobra.Command {
	var revoke bool
	cmd := &cobra.Command{
		Use:   "approve-env <alias> [KEY...]",
		Short: "Approve (or --revoke) host env vars a repo's forward_env may receive",
		Long: `A repo's .ahjo/ahjocontainer.json can request host env vars through
customizations.ahjo.forward_env, but that file is controlled by the repo and
by anything running in its containers, so a requested name is forwarded only
after you approve it for that repo. Approvals live in ~/.ahjo/registry.toml,
outside every container.

  ahjo repo approve-env <alias> KEY [KEY...]       approve names for this repo
  ahjo repo approve-env <alias> --revoke KEY...    remove approvals
  ahjo repo approve-env <alias>                    list approved names

Names you type here are the only input: this command never reads names from
the container. Names already in the global forward_env (~/.ahjo/config.toml)
go to every container and are not stored per repo. Changes take effect on
the next ` + "`ahjo shell`" + ` / ` + "`ahjo claude`" + `.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRepoApproveEnv(cobraOut(), args[0], args[1:], revoke)
		},
	}
	cmd.Flags().BoolVar(&revoke, "revoke", false, "remove the given names from the repo's approved list")
	return cmd
}

func runRepoApproveEnv(out io.Writer, alias string, keys []string, revoke bool) error {
	for _, k := range keys {
		if !ahjocontainer.ValidEnvName(k) {
			return fmt.Errorf("invalid env var name %q (must match [A-Za-z_][A-Za-z0-9_]*)", k)
		}
	}
	if revoke && len(keys) == 0 {
		return fmt.Errorf("--revoke needs at least one KEY")
	}
	gcfg, err := config.Load()
	if err != nil {
		return err
	}

	if len(keys) == 0 {
		reg, err := registry.Load()
		if err != nil {
			return err
		}
		repo := reg.FindRepoByAlias(alias)
		if repo == nil {
			return fmt.Errorf("no repo with alias %q (try `ahjo repo ls`)", alias)
		}
		if len(repo.ApprovedForwardEnv) == 0 {
			fmt.Fprintf(out, "no env vars approved for %s\n", alias)
		} else {
			fmt.Fprintf(out, "approved for %s: %s\n", alias, strings.Join(repo.ApprovedForwardEnv, ", "))
		}
		if len(gcfg.ForwardEnv) > 0 {
			fmt.Fprintf(out, "forwarded to every repo (global forward_env): %s\n", strings.Join(gcfg.ForwardEnv, ", "))
		}
		return nil
	}

	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()
	reg, err := registry.Load()
	if err != nil {
		return err
	}
	repo := reg.FindRepoByAlias(alias)
	if repo == nil {
		return fmt.Errorf("no repo with alias %q (try `ahjo repo ls`)", alias)
	}
	next, global := applyApproveEnv(repo.ApprovedForwardEnv, keys, gcfg.ForwardEnv, revoke)
	if len(next) == 0 {
		next = nil
	}
	repo.ApprovedForwardEnv = next
	if err := reg.Save(); err != nil {
		return err
	}

	changed := withoutKeys(dedupeKeys(keys), global)
	if revoke {
		if len(changed) > 0 {
			fmt.Fprintf(out, "→ revoked for %s: %s (takes effect on the next ahjo shell / ahjo claude; rotate the secret if the container may have kept it)\n",
				alias, strings.Join(changed, ", "))
		}
		if len(global) > 0 {
			fmt.Fprintf(out, "  note: %s is in the global forward_env (~/.ahjo/config.toml) and is still forwarded to every repo\n",
				strings.Join(global, ", "))
		}
		return nil
	}
	if len(global) > 0 {
		fmt.Fprintf(out, "  %s already forwarded globally (forward_env in ~/.ahjo/config.toml); not stored for %s\n",
			strings.Join(global, ", "), alias)
	}
	if len(changed) > 0 {
		fmt.Fprintf(out, "→ approved for %s: %s (takes effect on the next ahjo shell / ahjo claude)\n",
			alias, strings.Join(changed, ", "))
	}
	return nil
}

// applyApproveEnv is `ahjo repo approve-env`'s pure list mutation: it adds
// (or, with revoke, removes) keys from current and returns the new list,
// sorted and de-duplicated. Keys in global are never stored (they are
// forwarded to every container already) and are returned, de-duplicated in
// first-seen order, in inGlobal so the caller can say so. A revoke also
// drops a global key from current should a hand-edited registry hold one.
// Keys must already be validated.
func applyApproveEnv(current, keys, global []string, revoke bool) (next, inGlobal []string) {
	globalSet := make(map[string]struct{}, len(global))
	for _, k := range global {
		globalSet[k] = struct{}{}
	}
	set := make(map[string]struct{}, len(current)+len(keys))
	for _, k := range current {
		if k != "" {
			set[k] = struct{}{}
		}
	}
	for _, k := range dedupeKeys(keys) {
		_, isGlobal := globalSet[k]
		if isGlobal {
			inGlobal = append(inGlobal, k)
		}
		switch {
		case revoke:
			delete(set, k)
		case !isGlobal:
			set[k] = struct{}{}
		}
	}
	next = make([]string, 0, len(set))
	for k := range set {
		next = append(next, k)
	}
	sort.Strings(next)
	return next, inGlobal
}

// dedupeKeys returns keys without duplicates, first-seen order.
func dedupeKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// withoutKeys returns keys minus every entry in drop, order kept.
func withoutKeys(keys, drop []string) []string {
	dropSet := make(map[string]struct{}, len(drop))
	for _, k := range drop {
		dropSet[k] = struct{}{}
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, d := dropSet[k]; !d {
			out = append(out, k)
		}
	}
	return out
}
