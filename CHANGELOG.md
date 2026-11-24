# Changelog

## Unreleased

### Changed (behaviour change)

- Containers can no longer reach private networks, the host, or each other. `ahjo init` and `ahjo update` attach a network ACL (`ahjo-egress`) to `incusbr0` that rejects traffic to private, loopback, link-local and CGNAT ranges (including the Mac via `host.lima.internal`, the VM, your LAN, VPN and Tailscale, and cloud metadata), and enable port isolation on the default profile's `eth0`. The public IPv4 internet is unchanged.
- To let containers reach a private range, add it to `[network] allow_cidrs` in `~/.ahjo/config.toml` (IPv4, `/8` or longer). To reach a single host service, use `ahjo forward`.
- Existing installs: run `ahjo update` once to apply this. Running containers pick it up on their next restart. `ahjo doctor` warns while it is missing.
