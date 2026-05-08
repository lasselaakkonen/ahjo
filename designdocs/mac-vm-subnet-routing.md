# Routable VM subnet on Mac

**Status:** draft · **Scope:** macOS host only (Linux is unaffected)

## Goal

Make every Incus container's bridge IP (`10.20.30.0/24`) directly reachable from the Mac host, so any port on any container can be addressed as `<container-ip>:<original-port>` — and, layered on top, as `<branch>.<repo>.test:<original-port>` via DNS.

This replaces the "allocate a unique loopback port per container listener" model with "every container is a first-class IP on the network". The loopback-pool stays as a fallback for users who don't opt in.

## Non-goals

- Replacing `vzNAT` for the VM's own outbound traffic (DNS, package fetches, git over SSH). Outbound stays on `vzNAT`.
- Changing anything on Linux. Linux already has a reachable bridge IP per container.
- Auto-installing host prerequisites silently (see `project_ahjo_claude_github_only`).

## Topology change

Before — VM is NAT'd, container subnet is not addressable from Mac:

```
Mac ──vzNAT──► Lima VM ──incusbr0──► container (10.20.30.x)
                                        ▲
                                        │  unreachable from Mac
```

After — VM gains a second NIC on a `socket_vmnet` shared network; Mac gains a static route to the container subnet via the VM's vmnet IP:

```
Mac ──vzNAT────────► Lima VM         (kept for outbound)
Mac ──vmnet (eth1)─► Lima VM ──incusbr0──► container (10.20.30.x)
        └── route 10.20.30.0/24 via <vm-vmnet-ip> ──┘
```

`socket_vmnet`'s shared mode hands the VM an IP on a vmnet subnet (default `192.168.105.0/24`) that the Mac kernel also has an interface on. The VM's `incusbr0` is on a separate subnet, so a single static route on the Mac is sufficient — no NAT, no proxy, packets transit the VM as plain IP forwarding.

## What ahjo does

1. **Detect & gate.** `ahjo init` checks for `socket_vmnet` (`/opt/socket_vmnet/bin/socket_vmnet` by convention). Missing → print warn + install instructions, fall back to today's loopback-pool model. Never auto-install (no silent host installs).
2. **Provision the VM with a vmnet interface.** Lima supports this declaratively via the `networks:` stanza referencing a `socket_vmnet` network defined in `~/.lima/_config/networks.yaml`. ahjo writes that entry on first run, then re-creates / starts the `ahjo` VM with the interface attached.
3. **Enable IP forwarding inside the VM.** `sysctl -w net.ipv4.ip_forward=1` (persisted). Required for the Mac → vmnet → incusbr0 path.
4. **Add the host route.** `route -n add -net 10.20.30.0/24 <vm-vmnet-ip>` on Mac. Re-applied on every `ahjo` invocation that needs it (route table is volatile across reboots and the VM's vmnet IP can change). Removal on `ahjo down` is best-effort.
5. **Skip proxy-device creation** in this mode. `ahjo expose` and `reconcileAutoExpose` short-circuit — there's nothing to expose, the IP is already reachable. They still record allocations for `ahjo ls` so the user has a single source of truth for "what's running where".
6. **Config knob.** `~/.ahjo/config.toml` gains `[mac] networking = "vmnet" | "loopback"` (default `loopback` to preserve current behaviour). Per `feedback_ahjo_platform_config_namespacing`, this lives under `[mac]`.

## DNS layer (`<branch>.<repo>.test`)

Routing makes container IPs reachable; DNS makes them memorable. Two pieces:

1. **Authoritative responder inside the VM.** Tiny dnsmasq (or CoreDNS) bound to the VM's vmnet IP, listening on `:53`. ahjo writes one A record per worktree on container create (`feat-abc.muistiin-com.test → 10.20.30.42`) and removes it on `ahjo rm`. Source of truth: the existing `internal/registry`.
2. **Per-TLD resolver on Mac.** `/etc/resolver/test` with `nameserver <vm-vmnet-ip>`. macOS's resolver consults this only for `*.test`, leaving the rest of DNS untouched. Written once during `ahjo init` (with sudo prompt — see install tax below).

This avoids `/etc/hosts` churn and works for any tool that uses the system resolver, including browsers.

## Mac install tax

Three things require sudo on the Mac, all one-time:

| Step | Why | When |
| --- | --- | --- |
| `brew install socket_vmnet` + `sudo brew services start socket_vmnet` | setuid binary; requires root to open vmnet sockets | First-time opt-in |
| Write `/etc/resolver/test` | Owned by root; standard macOS path for per-TLD resolvers | First-time opt-in |
| `route add` for `10.20.30.0/24` | Route table needs root | Every VM start (route is volatile) |

ahjo prints exact commands and asks; never runs them itself. The "every VM start" route refresh is the one ongoing friction point — likely worth a one-shot helper (`sudo ahjo route-refresh`) or a launchd agent the user opts into separately.

## Lifecycle hazards

- **VM vmnet IP can change** across `socket_vmnet` daemon restarts (DHCP-ish). ahjo must read the current IP from `limactl list --json` (or in-VM `ip -4 addr show`) on every operation that re-applies the route, not cache it.
- **Mac sleep/wake** drops the vmnet interface; macOS usually re-creates it cleanly, but the static route survives only if the IP didn't change. Detect a missing route on `ahjo shell` entry and re-apply.
- **`socket_vmnet` daemon not running** is invisible to Lima — the VM boots but eth1 stays down. `ahjo doctor` should probe both `pgrep socket_vmnet` and `ip -4 addr show eth1` inside the VM.
- **Conflicting subnets.** If the user already routes `10.20.30.0/24` somewhere (corporate VPN, another VM), ahjo cannot silently take it. Detect the collision, surface it, offer a config knob to relocate Incus to a different `/24`.

## Open questions

1. **Does this fully replace the loopback pool, or coexist?** Coexist initially; revisit after the feature has miles on it. The pool is well-tested and works without sudo.
2. **Wildcard TLS.** Browsers will warn on `*.muistiin-com.test`. Out of scope for this doc, but a follow-up could ship a per-VM mkcert root and an automatic per-hostname cert from the in-VM proxy.
3. **Multiple repos.** Each repo gets its own `<repo>.test` zone. Confirm dnsmasq config can be regenerated atomically as worktrees come and go without dropping in-flight queries.
4. **VM-to-VM, not just Mac-to-VM.** Out of scope — ahjo is single-VM today.

## What's deliberately omitted

- HTTP-only reverse-proxy hostnames (Caddy/Traefik on the VM). That's a different design (Option A) and can layer cleanly on top of this one if both exist.
- `vznet` / `vmnet-bridged` modes that put the VM on the LAN. Useful for showing work to a teammate on the same network, but a much bigger blast radius and a separate decision.
