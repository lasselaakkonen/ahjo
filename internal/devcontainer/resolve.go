// Phase 2b dependency resolution for devcontainer Features. Builds
// the apply order from a top-level `features:` map plus each Feature's
// `dependsOn` (hard, recursive — transitively pulled into the install
// set) and `installsAfter` (soft, conditional — only enforced when
// both Features are already in the set).
//
// We don't honor `legacyIds` or restart-between-rounds. The design doc
// explicitly leaves both out: ahjo applies Features in a single round,
// and a Feature that requires a restart between Features is rejected
// with a clear error rather than silently succeeding.

package devcontainer

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// FetchedFeature is one Feature ready to apply, paired with the metadata
// the resolver read off it. Production callers don't construct these
// directly — Resolve fetches them through the FetchFunc passed in.
type FetchedFeature struct {
	Ref      FeatureRef
	Feature  Feature
	Metadata *Metadata
}

// FetchFunc fetches the Feature at ref into a local dir and returns it
// alongside the parsed metadata. Implementations must return a fresh
// Feature.Dir per call (the caller relies on extracted contents not
// stomping each other) and can cache by ref so repeated calls during
// transitive resolution don't re-pull.
type FetchFunc func(ctx context.Context, ref FeatureRef, options map[string]any) (FetchedFeature, error)

// NormalizeOptions flattens a top-level `features:` map value into a
// {KEY: "value"} map suitable for the Feature runner's env envelope.
// The spec allows three shapes:
//
//   - object: {"version": "20", "tls": true} → as written
//   - bool / string / number: shorthand for `{"version": <val>}`
//   - empty (null/absent): no options
//
// Booleans render as "true" / "false"; numbers via fmt.Sprint. A nested
// object value is rejected — Features ahjo encounters in the wild use
// flat options.
func NormalizeOptions(raw any) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	switch v := raw.(type) {
	case map[string]any:
		out := make(map[string]string, len(v))
		for k, val := range v {
			s, err := optionToString(k, val)
			if err != nil {
				return nil, err
			}
			out[strings.ToUpper(k)] = s
		}
		return out, nil
	case string, bool, float64, int, int64:
		// Shorthand — string becomes `version` per spec convention.
		s, err := optionToString("version", v)
		if err != nil {
			return nil, err
		}
		return map[string]string{"VERSION": s}, nil
	default:
		return nil, fmt.Errorf("unsupported feature options shape %T", raw)
	}
}

func optionToString(key string, v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	case float64:
		// JSON numbers always decode as float64 through encoding/json.
		// Render integers without a decimal point so opt=20 → "20", not
		// "20.000000".
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x)), nil
		}
		return fmt.Sprintf("%g", x), nil
	case int:
		return fmt.Sprintf("%d", x), nil
	case int64:
		return fmt.Sprintf("%d", x), nil
	case nil:
		return "", nil
	default:
		return "", fmt.Errorf("option %q: unsupported value type %T", key, v)
	}
}

// stripRefVersion returns the registry+repository part of an OCI ref,
// dropping the `:tag` or `@digest` suffix. Used for installsAfter
// matching, which the spec defines without version pins.
func stripRefVersion(s string) string {
	if i := strings.LastIndex(s, "@"); i > 0 {
		return s[:i]
	}
	if i := strings.LastIndex(s, ":"); i > 0 && !strings.Contains(s[i:], "/") {
		return s[:i]
	}
	return s
}

// Resolve fetches every Feature referenced by features (top-level)
// plus every transitive `dependsOn`, then returns them in a
// topologically-sorted apply order satisfying:
//
//   - dependsOn (hard): a Feature applies after every Feature it
//     declares in its `dependsOn`.
//   - installsAfter (soft): a Feature applies after every Feature it
//     declares in `installsAfter` *iff* that Feature is in the install
//     set. Soft refs that aren't in the set are ignored.
//
// Cycles (whether through dependsOn alone or mixed with installsAfter)
// abort with an error citing the cycle's members. Order within a tier
// is alphabetical by ref for stable apply logs and reproducible test
// output.
func Resolve(ctx context.Context, features map[string]any, fetch FetchFunc) ([]FetchedFeature, error) {
	resolved := map[string]FetchedFeature{} // canonical ref → fetched
	type queueItem struct {
		raw     string
		options map[string]any
	}
	queue := make([]queueItem, 0, len(features))
	// Stable iteration order for testable behavior.
	topKeys := make([]string, 0, len(features))
	for k := range features {
		topKeys = append(topKeys, k)
	}
	sort.Strings(topKeys)
	for _, k := range topKeys {
		opts, _ := features[k].(map[string]any)
		// optionToString handles the non-object shapes via normalizeOptions
		// later; here we just need *something* to thread.
		queue = append(queue, queueItem{raw: k, options: opts})
	}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		ref, err := ParseFeatureRef(item.raw)
		if err != nil {
			return nil, fmt.Errorf("feature %q: %w", item.raw, err)
		}
		key := ref.String()
		if _, ok := resolved[key]; ok {
			// Already pulled; the spec lets the first reference win for
			// option values when the same Feature appears multiple times.
			continue
		}
		fetched, err := fetch(ctx, ref, item.options)
		if err != nil {
			return nil, fmt.Errorf("fetch feature %s: %w", ref, err)
		}
		resolved[key] = fetched
		// Enqueue transitive deps. dependsOn is keyed by source ref;
		// values are the option map for that dep.
		for depRef, depOpts := range fetched.Metadata.DependsOn {
			queue = append(queue, queueItem{raw: depRef, options: depOpts})
		}
	}

	// Build the dep graph. Edges run from prereq → dependent; topo sort
	// emits prereqs first.
	type node struct {
		ref      string
		fetched  FetchedFeature
		incoming map[string]struct{}
		outgoing map[string]struct{}
	}
	nodes := make(map[string]*node, len(resolved))
	for k, f := range resolved {
		nodes[k] = &node{
			ref:      k,
			fetched:  f,
			incoming: map[string]struct{}{},
			outgoing: map[string]struct{}{},
		}
	}
	addEdge := func(from, to string) {
		if from == to {
			return
		}
		if _, ok := nodes[from]; !ok {
			return
		}
		if _, ok := nodes[to]; !ok {
			return
		}
		nodes[from].outgoing[to] = struct{}{}
		nodes[to].incoming[from] = struct{}{}
	}
	// Hard deps: dependsOn. We resolve by canonical ref string. The
	// dependsOn key may differ in tag form (e.g. `:1` vs `:1.0.0`); we
	// canonicalize via ParseFeatureRef before lookup.
	for k, n := range nodes {
		for depRefRaw := range n.fetched.Metadata.DependsOn {
			depRef, err := ParseFeatureRef(depRefRaw)
			if err != nil {
				return nil, fmt.Errorf("feature %s dependsOn %q: %w", k, depRefRaw, err)
			}
			addEdge(depRef.String(), k)
		}
	}
	// Soft deps: installsAfter, only enforced when the named Feature is
	// in the install set. Match by stripped version so a top-level
	// `node:1` satisfies an `installsAfter: ["node"]` reference.
	stripped := map[string]string{} // stripped → canonical ref
	for k := range nodes {
		stripped[stripRefVersion(k)] = k
	}
	for k, n := range nodes {
		for _, soft := range n.fetched.Metadata.InstallsAfter {
			if other, ok := stripped[stripRefVersion(soft)]; ok {
				addEdge(other, k)
			}
		}
	}

	// Kahn's algorithm with a sorted ready set (alphabetical by ref) so
	// the output is deterministic across runs.
	var order []FetchedFeature
	ready := []string{}
	for k, n := range nodes {
		if len(n.incoming) == 0 {
			ready = append(ready, k)
		}
	}
	sort.Strings(ready)
	for len(ready) > 0 {
		k := ready[0]
		ready = ready[1:]
		n := nodes[k]
		order = append(order, n.fetched)
		// Walk outgoing in a stable order so removal observable in tests.
		outs := make([]string, 0, len(n.outgoing))
		for o := range n.outgoing {
			outs = append(outs, o)
		}
		sort.Strings(outs)
		for _, o := range outs {
			delete(nodes[o].incoming, k)
			if len(nodes[o].incoming) == 0 {
				ready = append(ready, o)
			}
		}
		sort.Strings(ready)
		delete(nodes, k)
	}

	if len(nodes) > 0 {
		// Whatever's left is in a cycle. Surface the members so the user
		// can untangle them.
		var rest []string
		for k := range nodes {
			rest = append(rest, k)
		}
		sort.Strings(rest)
		return nil, fmt.Errorf("feature dependency cycle among: %s", strings.Join(rest, ", "))
	}
	return order, nil
}
