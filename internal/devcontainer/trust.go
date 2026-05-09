// Trust gating for user-supplied devcontainer Features. Features run
// install.sh as root in the per-repo container, so a malicious or
// compromised Feature has the same blast radius as any other code in
// the container — bounded by the Incus boundary, but unbounded inside.
// The user gets one chance to consent per Feature *source pattern*
// (not per individual Feature ref); ahjo persists the answer on the
// Repo row so subsequent ahjo invocations don't re-prompt.

package devcontainer

import (
	"path"
	"strings"
)

// CuratedTrustedGlob is the source pattern for the upstream-maintained
// devcontainer Features set. Auto-trusted; never enters per-repo
// FeatureConsent. Bumping this is a deliberate trust-policy change.
const CuratedTrustedGlob = "ghcr.io/devcontainers/features/*"

// SourceToGlob normalizes a Feature reference into the glob pattern
// users consent to. The rule: drop tag/digest, drop the leaf segment,
// append "/*". Examples:
//
//   - `ghcr.io/devcontainers/features/node:1`   → `ghcr.io/devcontainers/features/*`
//   - `ghcr.io/foo/bar/baz:2.1.0`               → `ghcr.io/foo/bar/*`
//   - `ghcr.io/foo/single@sha256:...`           → `ghcr.io/foo/*`
//
// The "drop the leaf" choice matches the design doc's example
// (`ghcr.io/foo/*`): consent is to a publisher namespace, not to a
// specific Feature within it. Same publisher = one prompt.
func SourceToGlob(source string) string {
	bare := stripRefVersion(source)
	i := strings.LastIndex(bare, "/")
	if i <= 0 {
		// Defensive: should not happen on well-formed refs (need
		// host/path), but if it does, fall back to the bare source so
		// we don't return "/*" or similar nonsense.
		return bare
	}
	return bare[:i] + "/*"
}

// IsCuratedTrusted reports whether source is published under the
// curated upstream namespace and therefore needs no per-repo consent.
func IsCuratedTrusted(source string) bool {
	return MatchesGlob(CuratedTrustedGlob, source)
}

// MatchesGlob reports whether source is matched by glob using path.Match
// semantics (so `*` does NOT cross `/`). The source is stripped of its
// tag/digest before matching, since trust is granted by namespace, not
// by version.
func MatchesGlob(glob, source string) bool {
	bare := stripRefVersion(source)
	ok, err := path.Match(glob, bare)
	return err == nil && ok
}

// MatchesAnyGlob is MatchesGlob over a glob set.
func MatchesAnyGlob(globs []string, source string) bool {
	for _, g := range globs {
		if MatchesGlob(g, source) {
			return true
		}
	}
	return false
}

// PartitionFeatureSources splits the top-level `features:` keys into
// (auto-trusted, already-consented, needs-prompt) buckets. Used by the
// repo-add prompt path. consented is the union of glob keys in the
// Repo's FeatureConsent map (true values only — false marks an
// explicit "I declined this" record we'd want to surface as an error,
// though Phase 2b only ever stores true).
func PartitionFeatureSources(sources []string, consented []string) (auto, known, prompt []string) {
	seen := map[string]struct{}{}
	for _, s := range sources {
		glob := SourceToGlob(s)
		if _, dup := seen[glob]; dup {
			continue
		}
		seen[glob] = struct{}{}
		switch {
		case glob == CuratedTrustedGlob || IsCuratedTrusted(s):
			auto = append(auto, glob)
		case MatchesAnyGlob(consented, s):
			known = append(known, glob)
		default:
			prompt = append(prompt, glob)
		}
	}
	return auto, known, prompt
}
