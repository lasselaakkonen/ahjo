package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/initflow"
)

// networkHardeningPost is printed after the step applies changes. It is one
// constant so the wording can be corrected once live testing shows exactly
// when running containers pick up profile and network changes.
const networkHardeningPost = "  → running containers pick this up on their next restart"

// egressDesired is the ACL content the step converges on.
type egressDesired struct {
	HostAddrs []netip.Addr
	OptIn     []netip.Prefix
	Blocked   []netip.Prefix
	Allowed   []netip.Prefix
	// RulesYAML is the rendered rule body; Hash is computed over it.
	RulesYAML string
	Hash      string
}

// computeEgressDesired derives the ACL from the host's IPv4 addresses and the
// user's network.allow_cidrs. An invalid opt-in fails with ParseOptIn's error.
func computeEgressDesired(hostAddrs []netip.Addr, allowCIDRs []string) (egressDesired, error) {
	optIn, err := initflow.ParseOptIn(allowCIDRs)
	if err != nil {
		return egressDesired{}, err
	}
	blocked := initflow.BlockedPrefixes(hostAddrs)
	allowed := initflow.AllowedEgress(blocked, optIn)
	rules := initflow.EgressACLYAML(allowed)
	return egressDesired{
		HostAddrs: hostAddrs,
		OptIn:     optIn,
		Blocked:   blocked,
		Allowed:   allowed,
		RulesYAML: rules,
		Hash:      rulesHash(rules),
	}, nil
}

// rulesHash is the value stored in the ACL's user.ahjo.rules-hash key.
func rulesHash(rulesYAML string) string {
	sum := sha256.Sum256([]byte(rulesYAML))
	return hex.EncodeToString(sum[:])
}

// aclEditYAML is the full body for `incus network acl edit`. The edit replaces
// the whole ACL, config included, so the hash travels in the same document:
// rules and hash change in one call and can never disagree.
func (d egressDesired) aclEditYAML() string {
	return d.RulesYAML + "config:\n  " + incus.EgressACLHashKey + ": \"" + d.Hash + "\"\n"
}

// loadEgressDesired reads config and host addresses and computes the target.
func loadEgressDesired() (egressDesired, error) {
	cfg, err := config.Load()
	if err != nil {
		return egressDesired{}, fmt.Errorf("load ~/.ahjo/config.toml: %w", err)
	}
	addrs, err := initflow.HostIPv4Addrs()
	if err != nil {
		return egressDesired{}, err
	}
	return computeEgressDesired(addrs, cfg.Network.AllowCIDRs)
}

// checkHardeningPreconditions fails when the objects the preseed creates are
// missing or not wired as the preseed wires them. Hardening some other network
// or NIC would be a guess, and a wrong guess leaves containers unfiltered
// while reporting success.
func checkHardeningPreconditions(o incus.NetworkHardeningObserved) error {
	if !o.NetworkExists {
		return fmt.Errorf("network %s not found; it is created by `ahjo init` (Incus preseed). Check `incus network list`", incus.BridgeNetwork)
	}
	if !o.NICExists {
		return fmt.Errorf("profile %s has no %s device; ahjo expects the preseed's NIC (network=%s) there. Check `incus profile show %s`",
			incus.DefaultProfile, incus.BridgeNIC, incus.BridgeNetwork, incus.DefaultProfile)
	}
	if o.NICNetwork != incus.BridgeNetwork {
		return fmt.Errorf("%s in profile %s has network=%q, want %s; ahjo only hardens %s, so containers would stay unfiltered. Check `incus profile show %s`",
			incus.BridgeNIC, incus.DefaultProfile, o.NICNetwork, incus.BridgeNetwork, incus.BridgeNetwork, incus.DefaultProfile)
	}
	return nil
}

// hardeningDrift lists what differs from the target; empty means nothing to
// do. Callers check preconditions first.
func hardeningDrift(o incus.NetworkHardeningObserved, wantHash string) []string {
	var drift []string
	switch {
	case !o.ACLExists:
		drift = append(drift, "ACL "+incus.EgressACL+" missing")
	case o.ACLHash != wantHash:
		drift = append(drift, "ACL rules out of date")
	}
	if !incus.ACLListContains(o.NetworkACLs, incus.EgressACL) {
		drift = append(drift, incus.EgressACL+" not attached to "+incus.BridgeNetwork)
	}
	for _, kv := range incus.HardenedNetworkDefaults() {
		if o.Network[kv.Key] != kv.Value {
			drift = append(drift, incus.BridgeNetwork+" "+kv.String())
		}
	}
	for _, kv := range incus.HardenedNICKeys() {
		if o.NIC[kv.Key] != kv.Value {
			drift = append(drift, incus.BridgeNIC+" "+kv.String())
		}
	}
	return drift
}

// networkHardeningSkip is the pure Skip decision. Missing preconditions are
// an error, so the step fails loudly instead of skipping or guessing.
func networkHardeningSkip(o incus.NetworkHardeningObserved, wantHash string) (bool, string, error) {
	if err := checkHardeningPreconditions(o); err != nil {
		return false, "", err
	}
	if len(hardeningDrift(o, wantHash)) == 0 {
		return true, incus.EgressACL + " attached to " + incus.BridgeNetwork + " with current rules; " +
			incus.BridgeNIC + " isolated", nil
	}
	return false, "", nil
}

func joinPrefixes(ps []netip.Prefix) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return strings.Join(s, ", ")
}

func joinAddrs(as []netip.Addr) string {
	s := make([]string, len(as))
	for i, a := range as {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

func kvArgs(kvs []incus.KV) []string {
	out := make([]string, len(kvs))
	for i, kv := range kvs {
		out[i] = kv.String()
	}
	return out
}

// networkHardeningShow renders the preview. allowCIDRs comes from the config
// read when the steps are assembled; cfgErr, if set, is shown so the user
// knows the step will fail.
func networkHardeningShow(allowCIDRs []string, cfgErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "incus network acl create %s   (if missing)\n", incus.EgressACL)
	fmt.Fprintf(&b, "incus network acl edit %s < <one egress allow rule per allowed range, %s=<sha256 of rules>>\n",
		incus.EgressACL, incus.EgressACLHashKey)
	fmt.Fprintf(&b, "incus network set %s %s=<existing ACLs>,%s %s\n",
		incus.BridgeNetwork, incus.NetworkACLsKey, incus.EgressACL, strings.Join(kvArgs(incus.HardenedNetworkDefaults()), " "))
	fmt.Fprintf(&b, "incus profile device set %s %s %s\n",
		incus.DefaultProfile, incus.BridgeNIC, strings.Join(kvArgs(incus.HardenedNICKeys()), " "))
	fmt.Fprintf(&b, "blocked: %s\n", joinPrefixes(initflow.BlockedPrefixes(nil)))
	b.WriteString("  + a /32 for every IPv4 address on a host interface (read when the step runs)\n")
	switch {
	case cfgErr != nil:
		fmt.Fprintf(&b, "opt-in (network.allow_cidrs): config error, the step will fail: %v\n", cfgErr)
	case len(allowCIDRs) == 0:
		b.WriteString("opt-in (network.allow_cidrs): none\n")
	default:
		fmt.Fprintf(&b, "opt-in (network.allow_cidrs): %s\n", strings.Join(allowCIDRs, ", "))
	}
	return b.String()
}

// networkHardeningStep firewalls the Incus bridge so container code cannot
// reach the host, the Mac (host.lima.internal), the LAN, cloud metadata, or
// sibling containers: an egress allow-list ACL on incusbr0 with default
// egress reject, plus port isolation and MAC/IPv4 filtering on the default
// profile's eth0.
//
// Shared between `ahjo init` and `ahjo update`. It runs after the
// incus-admin group join, so plain `incus` (no sudo) works, as for every
// later step: on a first init the join step re-execs ahjo under
// `sg incus-admin`, and this step runs in that re-exec'd process.
//
// Skip errors (incus unreachable, a getter failing, a precondition missing)
// are returned, which stops the runner with "<title>: detect: ...". The step
// never skips on an error.
func networkHardeningStep() initflow.Step {
	var allowCIDRs []string
	cfg, cfgErr := config.Load()
	if cfgErr == nil {
		allowCIDRs = cfg.Network.AllowCIDRs
	}

	// desired is computed in Skip, which the runner calls right before
	// Action, and reused there so the applied rules are the ones Skip
	// compared against.
	var desired *egressDesired
	resolve := func() (egressDesired, error) {
		if desired != nil {
			return *desired, nil
		}
		d, err := loadEgressDesired()
		if err != nil {
			return egressDesired{}, err
		}
		desired = &d
		return d, nil
	}

	return initflow.Step{
		Title: "Firewall " + incus.BridgeNetwork + " egress and isolate containers (" + incus.EgressACL + " ACL)",
		Skip: func() (bool, string, error) {
			d, err := resolve()
			if err != nil {
				return false, "", err
			}
			o, err := incus.ObserveNetworkHardening()
			if err != nil {
				return false, "", err
			}
			return networkHardeningSkip(o, d.Hash)
		},
		Note: "containers may reach the public internet and nothing else: not the host, the Mac, the LAN, " +
			"VPN/Tailscale ranges, cloud metadata, or sibling containers. Allow a private range with " +
			"[network] allow_cidrs in ~/.ahjo/config.toml, then `ahjo update`. Host services stay reachable " +
			"only through `ahjo forward`.",
		Show: networkHardeningShow(allowCIDRs, cfgErr),
		Action: func(out io.Writer) error {
			d, err := resolve()
			if err != nil {
				return err
			}
			if len(d.HostAddrs) > 0 {
				fmt.Fprintf(out, "  → also blocking host addresses: %s\n", joinAddrs(d.HostAddrs))
			}
			fmt.Fprintf(out, "  → %d egress allow rules\n", len(d.Allowed))

			// Order matters: the ACL gets its full rule set before it is
			// attached, so attaching it never cuts egress that should work.
			exists, err := incus.NetworkACLExists(incus.EgressACL)
			if err != nil {
				return err
			}
			if !exists {
				if err := incus.NetworkACLCreate(incus.EgressACL); err != nil {
					return fmt.Errorf("create ACL %s: %w", incus.EgressACL, err)
				}
			}
			if err := incus.NetworkACLReplace(incus.EgressACL, d.aclEditYAML()); err != nil {
				return fmt.Errorf("write ACL %s rules: %w", incus.EgressACL, err)
			}

			// Re-read security.acls here rather than trusting Skip's copy:
			// the user may have sat at the prompt for a while.
			cur, err := incus.NetworkGet(incus.BridgeNetwork, incus.NetworkACLsKey)
			if err != nil {
				return err
			}
			netKV := append([]string{incus.NetworkACLsKey + "=" + incus.ACLListWith(cur, incus.EgressACL)},
				kvArgs(incus.HardenedNetworkDefaults())...)
			if err := incus.NetworkSet(incus.BridgeNetwork, netKV...); err != nil {
				return fmt.Errorf("attach %s to %s: %w", incus.EgressACL, incus.BridgeNetwork, err)
			}

			if err := incus.ProfileDeviceSet(incus.DefaultProfile, incus.BridgeNIC, kvArgs(incus.HardenedNICKeys())...); err != nil {
				return fmt.Errorf("isolate %s in profile %s: %w", incus.BridgeNIC, incus.DefaultProfile, err)
			}
			return nil
		},
		Post: networkHardeningPost,
	}
}
