package incus

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Names and settings for the incusbr0 network hardening that `ahjo init` and
// `ahjo update` apply (internal/cli networkHardeningStep) and `ahjo doctor`
// checks (CheckNetworkHardening). They live here, not in internal/cli, so
// internal/preflight can import them without an import cycle.
//
// BridgeNetwork, DefaultProfile and BridgeNIC must match the objects created by
// internal/initflow/assets/incus-preseed.yaml.tmpl.
const (
	BridgeNetwork  = "incusbr0"
	DefaultProfile = "default"
	BridgeNIC      = "eth0"

	// EgressACL is the network ACL holding the egress allow-list.
	EgressACL = "ahjo-egress"
	// EgressACLHashKey is the ACL config key storing a hash of the rendered
	// rules, so re-runs can tell whether the rules are current without diffing
	// ~100 rules.
	EgressACLHashKey = "user.ahjo.rules-hash"

	// NetworkACLsKey is the comma-separated list of ACLs attached to a network.
	NetworkACLsKey = "security.acls"
	// NetworkDefaultEgressKey and NetworkDefaultIngressKey set what happens to
	// traffic no attached ACL rule matches.
	NetworkDefaultEgressKey  = "security.acls.default.egress.action"
	NetworkDefaultIngressKey = "security.acls.default.ingress.action"

	// NICPortIsolationKey stops a NIC talking to other isolated NICs on the
	// same bridge. Bridge ACLs cannot filter container-to-container traffic,
	// so this is the only sibling isolation.
	NICPortIsolationKey = "security.port_isolation"
	NICMACFilteringKey  = "security.mac_filtering"
	NICIPv4FilteringKey = "security.ipv4_filtering"
)

// KV is one config key and the value hardening expects for it.
type KV struct {
	Key   string
	Value string
}

// String renders the pair as the "key=value" argv entry `incus ... set` takes.
func (kv KV) String() string { return kv.Key + "=" + kv.Value }

// HardenedNetworkDefaults is the default ACL actions hardening sets on
// BridgeNetwork: reject egress no allow rule matches (fail fast rather than
// hang), allow ingress (nothing unsolicited reaches the NATed bridge except
// from the host; siblings are handled by port isolation).
func HardenedNetworkDefaults() []KV {
	return []KV{
		{NetworkDefaultEgressKey, "reject"},
		{NetworkDefaultIngressKey, "allow"},
	}
}

// HardenedNICKeys is the device config hardening sets on BridgeNIC in
// DefaultProfile.
func HardenedNICKeys() []KV {
	return []KV{
		{NICPortIsolationKey, "true"},
		{NICMACFilteringKey, "true"},
		{NICIPv4FilteringKey, "true"},
	}
}

// ACLListContains reports whether a comma-separated security.acls value
// names acl. Entries are trimmed; matching is exact.
func ACLListContains(list, acl string) bool {
	return slices.Contains(splitACLList(list), acl)
}

// ACLListWith returns list with acl appended if it is not already there,
// keeping any other ACLs the user attached, in their order. Empty entries and
// surrounding spaces are dropped.
func ACLListWith(list, acl string) string {
	names := splitACLList(list)
	if !slices.Contains(names, acl) {
		names = append(names, acl)
	}
	return strings.Join(names, ",")
}

func splitACLList(list string) []string {
	var out []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// NetworkHardeningObserved is the live state hardening cares about. Values
// for keys of an object that does not exist are left empty.
type NetworkHardeningObserved struct {
	NetworkExists bool
	// NetworkACLs is BridgeNetwork's raw security.acls value.
	NetworkACLs string
	// Network holds BridgeNetwork's values for the HardenedNetworkDefaults keys.
	Network map[string]string

	NICExists bool
	// NICNetwork is the `network` key of BridgeNIC in DefaultProfile.
	NICNetwork string
	// NIC holds BridgeNIC's values for the HardenedNICKeys keys.
	NIC map[string]string

	ACLExists bool
	// ACLHash is EgressACL's EgressACLHashKey value.
	ACLHash string
}

// ObserveNetworkHardening reads the state above from Incus. Any incus failure
// (daemon down, permission denied) is returned as an error, never reported
// as "not hardened".
func ObserveNetworkHardening() (NetworkHardeningObserved, error) {
	o := NetworkHardeningObserved{Network: map[string]string{}, NIC: map[string]string{}}
	var err error

	if o.NetworkExists, err = NetworkExists(BridgeNetwork); err != nil {
		return o, err
	}
	if o.NetworkExists {
		if o.NetworkACLs, err = NetworkGet(BridgeNetwork, NetworkACLsKey); err != nil {
			return o, err
		}
		for _, kv := range HardenedNetworkDefaults() {
			if o.Network[kv.Key], err = NetworkGet(BridgeNetwork, kv.Key); err != nil {
				return o, err
			}
		}
	}

	if o.NICExists, err = ProfileDeviceExists(DefaultProfile, BridgeNIC); err != nil {
		return o, err
	}
	if o.NICExists {
		if o.NICNetwork, err = ProfileDeviceGet(DefaultProfile, BridgeNIC, "network"); err != nil {
			return o, err
		}
		for _, kv := range HardenedNICKeys() {
			if o.NIC[kv.Key], err = ProfileDeviceGet(DefaultProfile, BridgeNIC, kv.Key); err != nil {
				return o, err
			}
		}
	}

	if o.ACLExists, err = NetworkACLExists(EgressACL); err != nil {
		return o, err
	}
	if o.ACLExists {
		if o.ACLHash, err = NetworkACLGet(EgressACL, EgressACLHashKey); err != nil {
			return o, err
		}
	}
	return o, nil
}

// EgressFiltered reports whether BridgeNetwork has EgressACL attached and
// rejects egress the ACL does not allow. Both are needed: with a default
// egress action of allow, an allow-only ACL filters nothing.
func (o NetworkHardeningObserved) EgressFiltered() bool {
	return o.NetworkExists && o.ACLExists &&
		ACLListContains(o.NetworkACLs, EgressACL) &&
		o.Network[NetworkDefaultEgressKey] == "reject"
}

// PortIsolated reports whether BridgeNIC in DefaultProfile is on
// BridgeNetwork with port isolation on.
func (o NetworkHardeningObserved) PortIsolated() bool {
	return o.NICExists && o.NICNetwork == BridgeNetwork && o.NIC[NICPortIsolationKey] == "true"
}

// NetworkHardeningStatus is the read-only verdict for `ahjo doctor`.
type NetworkHardeningStatus struct {
	EgressFiltered bool
	PortIsolated   bool
	// Reason says what is missing, "" when both are true.
	Reason string
}

// OK reports whether both halves of the hardening are in place.
func (s NetworkHardeningStatus) OK() bool { return s.EgressFiltered && s.PortIsolated }

// Status reduces the observed state to the doctor verdict. It does not check
// whether the ACL's rules are current (that needs the config and host
// addresses); `ahjo update` handles drift.
func (o NetworkHardeningObserved) Status() NetworkHardeningStatus {
	s := NetworkHardeningStatus{EgressFiltered: o.EgressFiltered(), PortIsolated: o.PortIsolated()}
	var why []string
	switch {
	case !o.NetworkExists:
		why = append(why, "network "+BridgeNetwork+" not found")
	case !o.ACLExists:
		why = append(why, "ACL "+EgressACL+" not found")
	case !ACLListContains(o.NetworkACLs, EgressACL):
		why = append(why, BridgeNetwork+" does not have "+EgressACL+" in "+NetworkACLsKey)
	case o.Network[NetworkDefaultEgressKey] != "reject":
		why = append(why, fmt.Sprintf("%s %s is %q, want reject", BridgeNetwork, NetworkDefaultEgressKey, o.Network[NetworkDefaultEgressKey]))
	}
	switch {
	case !o.NICExists:
		why = append(why, "profile "+DefaultProfile+" has no "+BridgeNIC+" device")
	case o.NICNetwork != BridgeNetwork:
		why = append(why, fmt.Sprintf("%s in profile %s is on network %q, not %s", BridgeNIC, DefaultProfile, o.NICNetwork, BridgeNetwork))
	case o.NIC[NICPortIsolationKey] != "true":
		why = append(why, BridgeNIC+" in profile "+DefaultProfile+" lacks "+NICPortIsolationKey+"=true")
	}
	s.Reason = strings.Join(why, "; ")
	return s
}

// CheckNetworkHardening is the read-only check for internal/preflight: whether
// incusbr0 filters egress through ahjo-egress and whether eth0 in the default
// profile has port isolation. An incus failure is returned as an error.
func CheckNetworkHardening() (NetworkHardeningStatus, error) {
	o, err := ObserveNetworkHardening()
	if err != nil {
		return NetworkHardeningStatus{}, err
	}
	return o.Status(), nil
}

// NetworkExists reports whether a network with this exact name exists, using
// the structured `network list --format=json` rather than a "not found"
// error string.
func NetworkExists(name string) (bool, error) {
	out, err := runIncusCapture("", "network", "list", "--format=json")
	if err != nil {
		return false, err
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return false, fmt.Errorf("parse incus network list: %w", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// ProfileDeviceExists reports whether a profile has a device with this exact
// name. `incus profile device list <profile>` prints one device name per line.
// A missing profile is an error.
func ProfileDeviceExists(profile, device string) (bool, error) {
	out, err := runIncusCapture("", "profile", "device", "list", profile)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == device {
			return true, nil
		}
	}
	return false, nil
}
