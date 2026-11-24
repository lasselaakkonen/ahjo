package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/initflow"
	"github.com/lasselaakkonen/ahjo/internal/lima"
)

// networkHardeningPost is printed after the step applies changes. Incus
// applies a network ACL to running instances at once, but has not been
// observed doing so on a live system yet; and security.port_isolation is not
// live-updatable on a bridged NIC, so Incus may remove and re-add eth0 on
// running containers. The wording holds either way and warns about the
// disruptive case.
const networkHardeningPost = "  → the firewall applies to running containers right away; Incus may re-attach their network interface, " +
	"which drops open connections; restart a container if it loses network"

// bridgeIPv4Key is the incusbr0 config key holding the gateway address and
// subnet length, e.g. "10.20.30.1/24".
const bridgeIPv4Key = "ipv4.address"

// egressInputs is what the desired ACL is computed from, read from the host,
// Incus and config by loadEgressDesired.
type egressInputs struct {
	// HostAddrs are the IPv4 addresses on the host's interfaces.
	HostAddrs []netip.Addr
	// LimaHost is host.lima.internal inside a Lima VM; invalid (the zero
	// Addr) on a native Linux host.
	LimaHost netip.Addr
	// LimaGateways are the IPv4 default-route gateways inside a Lima VM, all
	// of which are the Mac (the vzNAT gateway, typically 192.168.64.1, and
	// usually host.lima.internal too). Nil on a native Linux host, where the
	// default gateway is the user's router and theirs to opt in.
	LimaGateways []netip.Addr
	// BridgeIPv4 is incusbr0's raw bridgeIPv4Key value.
	BridgeIPv4 string
	// AllowCIDRs is the user's network.allow_cidrs.
	AllowCIDRs []string
}

// egressDesired is the ACL content the step converges on.
type egressDesired struct {
	HostAddrs    []netip.Addr
	LimaHost     netip.Addr
	LimaGateways []netip.Addr
	// BridgeSubnet is incusbr0's subnet, masked (10.20.30.0/24).
	BridgeSubnet netip.Prefix
	OptIn        []netip.Prefix
	Blocked      []netip.Prefix
	// Hard are the blocks no opt-in overrides.
	Hard []netip.Prefix
	// OptInHardBlocked is the part of OptIn that Hard keeps blocked.
	OptInHardBlocked []netip.Prefix
	Allowed          []netip.Prefix
	// RulesYAML is the rendered rule body; Hash is computed over it, so a
	// change to any input that changes the allowed set changes the hash.
	RulesYAML string
	Hash      string
}

// parseBridgeSubnet turns incusbr0's ipv4.address ("10.20.30.1/24") into its
// subnet. The bridge is NATed IPv4 by construction (the preseed sets it), so
// an empty, "none" or unparsable value means something is off, and the step
// fails rather than leaving the subnet out of the hard blocks.
func parseBridgeSubnet(v string) (netip.Prefix, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "none" || v == "auto" {
		return netip.Prefix{}, fmt.Errorf("network %s has %s=%q, want an IPv4 gateway/prefix such as 10.20.30.1/24; ahjo needs the bridge subnet to block container-to-host traffic. Check `incus network get %s %s`",
			incus.BridgeNetwork, bridgeIPv4Key, v, incus.BridgeNetwork, bridgeIPv4Key)
	}
	p, err := netip.ParsePrefix(v)
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("network %s has %s=%q, want an IPv4 gateway/prefix such as 10.20.30.1/24. Check `incus network get %s %s`",
			incus.BridgeNetwork, bridgeIPv4Key, v, incus.BridgeNetwork, bridgeIPv4Key)
	}
	return p.Masked(), nil
}

// computeEgressDesired derives the ACL from in. An invalid opt-in fails with
// ParseOptIn's error, an unusable bridge address with parseBridgeSubnet's.
func computeEgressDesired(in egressInputs) (egressDesired, error) {
	optIn, err := initflow.ParseOptIn(in.AllowCIDRs)
	if err != nil {
		return egressDesired{}, err
	}
	subnet, err := parseBridgeSubnet(in.BridgeIPv4)
	if err != nil {
		return egressDesired{}, err
	}
	addrs := slices.Clone(in.HostAddrs)
	if in.LimaHost.IsValid() {
		addrs = append(addrs, in.LimaHost)
	}
	addrs = append(addrs, in.LimaGateways...)
	blocked := initflow.BlockedPrefixes()
	hard := initflow.HardBlockedPrefixes(addrs, subnet)
	allowed := initflow.AllowedEgress(blocked, optIn, hard)
	rules := initflow.EgressACLYAML(allowed)
	return egressDesired{
		HostAddrs:        in.HostAddrs,
		LimaHost:         in.LimaHost,
		LimaGateways:     in.LimaGateways,
		BridgeSubnet:     subnet,
		OptIn:            optIn,
		Blocked:          blocked,
		Hard:             hard,
		OptInHardBlocked: initflow.OptInHardBlocked(optIn, hard),
		Allowed:          allowed,
		RulesYAML:        rules,
		Hash:             rulesHash(rules),
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

// hardBlockSummary is the Action line listing what stays blocked whatever the
// opt-ins say.
func (d egressDesired) hardBlockSummary() string {
	parts := []string{incus.BridgeNetwork + " subnet " + d.BridgeSubnet.String()}
	if len(d.HostAddrs) > 0 {
		parts = append(parts, "host addresses "+joinAddrs(d.HostAddrs))
	}
	if d.LimaHost.IsValid() {
		parts = append(parts, "host.lima.internal "+d.LimaHost.String())
	}
	// host.lima.internal is usually also a default gateway; name it once.
	var gws []netip.Addr
	for _, g := range d.LimaGateways {
		if g != d.LimaHost && !slices.Contains(gws, g) {
			gws = append(gws, g)
		}
	}
	if len(gws) > 0 {
		parts = append(parts, "Mac gateways "+joinAddrs(gws))
	}
	return "  → always blocked, even inside network.allow_cidrs: " + strings.Join(parts, "; ")
}

// optInHardBlockedLine is the Action line shown when an opt-in covers a hard
// block, so the user knows why part of their range stays unreachable. "" when
// no opt-in overlaps one.
func (d egressDesired) optInHardBlockedLine() string {
	if len(d.OptInHardBlocked) == 0 {
		return ""
	}
	what := "host addresses and the " + incus.BridgeNetwork + " subnet"
	if d.LimaHost.IsValid() || len(d.LimaGateways) > 0 {
		what = "host addresses, the Mac and the " + incus.BridgeNetwork + " subnet"
	}
	return "  → network.allow_cidrs covers " + joinPrefixes(d.OptInHardBlocked) +
		", which stays blocked (" + what + "); reach a host service with `ahjo forward`"
}

// loadEgressDesired reads config, host addresses, incusbr0's address and,
// inside a Lima VM, host.lima.internal and the VM's default gateways, and
// computes the target.
func loadEgressDesired() (egressDesired, error) {
	cfg, err := config.Load()
	if err != nil {
		return egressDesired{}, fmt.Errorf("load ~/.ahjo/config.toml: %w", err)
	}
	in := egressInputs{AllowCIDRs: cfg.Network.AllowCIDRs}
	if in.HostAddrs, err = initflow.HostIPv4Addrs(); err != nil {
		return egressDesired{}, err
	}
	if in.BridgeIPv4, err = incus.NetworkGet(incus.BridgeNetwork, bridgeIPv4Key); err != nil {
		return egressDesired{}, err
	}
	// Only a Lima guest has a Mac to block. lima.IsGuest is a stat, so a
	// native Linux host skips the DNS lookup. Inside Lima the name is pinned
	// in /etc/hosts, so failing to resolve it means something is off.
	if lima.IsGuest() {
		if in.LimaHost, err = incus.LimaHostIPv4(); err != nil {
			return egressDesired{}, fmt.Errorf("resolve host.lima.internal inside the Lima VM: %w", err)
		}
		// Every default gateway of the VM is the Mac (the vzNAT gateway is
		// not host.lima.internal), and a broad opt-in such as 192.168.0.0/16
		// would otherwise expose every Mac service bound to 0.0.0.0. None at
		// all is fine; failing to read them is not.
		if in.LimaGateways, err = initflow.DefaultGatewaysIPv4(); err != nil {
			return egressDesired{}, fmt.Errorf("read default gateways inside the Lima VM: %w", err)
		}
	}
	return computeEgressDesired(in)
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
	fmt.Fprintf(&b, "blocked: %s\n", joinPrefixes(initflow.BlockedPrefixes()))
	fmt.Fprintf(&b, "always blocked, even inside an opt-in: the %s subnet, every IPv4 address on a host interface and, inside a Lima VM, the Mac at host.lima.internal and every default gateway (read when the step runs)\n",
		incus.BridgeNetwork)
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
			o, err := incus.ObserveNetworkHardening()
			if err != nil {
				return false, "", err
			}
			// Preconditions first: resolve reads incusbr0's address, and a
			// missing network should fail with the precondition's message,
			// not a bare `incus network get` error.
			if err := checkHardeningPreconditions(o); err != nil {
				return false, "", err
			}
			d, err := resolve()
			if err != nil {
				return false, "", err
			}
			return networkHardeningSkip(o, d.Hash)
		},
		Note: "containers may reach the public internet and nothing else: not the host, the Mac, the LAN, " +
			"VPN/Tailscale ranges, cloud metadata, or sibling containers. Allow a private range with " +
			"[network] allow_cidrs in ~/.ahjo/config.toml, then `ahjo update`. Host services stay reachable " +
			"only through `ahjo forward`. The firewall applies to running containers right away, and Incus may " +
			"re-attach their network interface, dropping open connections. On a native Linux host this changes the " +
			"shared " + incus.BridgeNetwork + " network and " + incus.DefaultProfile + " profile, so it also affects " +
			"instances there that ahjo did not create.",
		Show: networkHardeningShow(allowCIDRs, cfgErr),
		Action: func(out io.Writer) error {
			d, err := resolve()
			if err != nil {
				return err
			}
			fmt.Fprintln(out, d.hardBlockSummary())
			if line := d.optInHardBlockedLine(); line != "" {
				fmt.Fprintln(out, line)
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
