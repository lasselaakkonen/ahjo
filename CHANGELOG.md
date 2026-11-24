# Changelog

## Unreleased

### Changed (behaviour change)

- Containers can no longer reach private networks, the host, or each other. `ahjo init` and `ahjo update` attach a network ACL (`ahjo-egress`) to `incusbr0` that rejects traffic to private, loopback, link-local and CGNAT ranges (including the Mac via `host.lima.internal`, the VM, your LAN, VPN and Tailscale, and cloud metadata), and enable port isolation on the default profile's `eth0`. The public IPv4 internet is unchanged.
- To let containers reach a private range, add it to `[network] allow_cidrs` in `~/.ahjo/config.toml` (IPv4, `/8` or longer). On macOS this is the file inside the Lima VM (`limactl shell ahjo -- nano ~/.ahjo/config.toml`), not the Mac-side one. The host's own addresses, the `incusbr0` subnet and the Mac stay blocked even inside an opted-in range. To reach a single host service, use `ahjo forward`.
- Existing installs: run `ahjo update` once to apply this. The firewall applies to running containers right away; the first time, Incus may re-attach each container's network interface and drop open connections, so run it when no container is mid-task and restart a container if it loses network. Later `allow_cidrs` changes apply on `ahjo update` with no restart. `ahjo doctor` warns while it is missing.
