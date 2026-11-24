# Container isolation

ahjo's job is to give each `(repo, branch)` its own sandbox so an experiment, a `git pull`, an `npm install`, or a Claude action can't reach the host. This doc spells out the boundaries — what they isolate, and the one place they intentionally leak.

## The stack

```
┌──────────────────────────────────────────────────────────────────────┐
│ macOS host                                                           │
│  • shells, 1Password (SSH agent), browser, $HOME                     │
│  • ahjo CLI (thin shim)                                              │
│                                                                      │
│  ┌────────────────────────────────────────────────────────────────┐  │
│  │ Lima VM "ahjo"  (Ubuntu, vz + vzNAT)                           │  │
│  │  • Incus host, ahjo-base image (built from images:ubuntu/24.04 │  │
│  │    by applying the embedded ahjo-runtime devcontainer Feature) │  │
│  │  • per-(repo, branch) Incus container holds the checkout       │  │
│  │    at /repo (no host-side bare clones, no host worktrees)      │  │
│  │                                                                │  │
│  │  ┌─────────────────────┐  ┌─────────────────────┐  ┌────┐     │  │
│  │  │ Incus container A   │  │ Incus container B   │  │ …  │     │  │
│  │  │ <repo>@<branchA>    │  │ <repo>@<branchB>    │  │    │     │  │
│  │  │ own sshd, own port  │  │ own sshd, own port  │  │    │     │  │
│  │  └─────────────────────┘  └─────────────────────┘  └────┘     │  │
│  └────────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────────┘
```

On Linux hosts the middle layer collapses — Incus runs directly on the host, no Lima.

## What each boundary protects

**Mac → Lima VM.** The VM is a separate kernel under `vz`. Container/Incus state lives only in the VM. Code running in any container can't touch your Mac filesystem, browser session, keychain, or the rest of your home directory. Containers can't dial `localhost` apps on the Mac (reached from the VM as `host.lima.internal`, 192.168.5.2) or other hosts on your network: the `ahjo-egress` network ACL on `incusbr0` rejects that traffic. See [Network](#network); existing installs need one `ahjo update` to get it. The VM disk defaults to 100 GiB (set at `ahjo init`, sparse so it grows on demand) — runaway dependencies stop there.

**Lima VM → Incus container.** Each branch is its own Linux container with its own filesystem, process tree, and user (`ubuntu`, the upstream cloud-image canonical user at UID 1000). Containers cannot reach each other over the network: port isolation on each container's `eth0` blocks bridge traffic between siblings (see [Network](#network)). They share only what ahjo wires in: the in-container `/repo` checkout (which lives in the container's own rootfs, not a host bind-mount), generated SSH host keys (read-only mount), and the env vars listed in `forward_env` (e.g. `CLAUDE_CODE_OAUTH_TOKEN`). A container has no view of the VM's `~/.ahjo/`, no view of other containers, and no Incus admin privileges.

**Per-branch state.** Each container has its own `127.0.0.1:<port>` exposed by Incus' proxy device, its own SSH host keys under `~/.ahjo/host-keys/<slug>/`, and its own port allocations in `~/.ahjo/ports.json`. Two branches of the same repo cannot collide on ports, sockets, host-key fingerprints, or `node_modules`.

## What crosses the boundaries

These are the *intentional* leaks — every other path is closed.

| Path | Direction | Contents |
| --- | --- | --- |
| `~/.ahjo-shared/` (Lima virtiofs mount) | VM → Mac, read-write | generated `ssh-config` + `aliases` map so `ahjo ssh <alias>` works from the Mac |
| `forward_env` | VM → container | only the names listed in `~/.ahjo/config.toml` (default: `CLAUDE_CODE_OAUTH_TOKEN` + `GH_TOKEN`, sourced from `~/.ahjo/.env`) |
| Host keys dir | VM → container, read-only | per-branch sshd keys + `authorized_keys` (single-file bind mount at `/home/ubuntu/.ssh/authorized_keys`) |
| SSH agent socket | Mac → VM → container | conditional — suppressed when an HTTPS+PAT origin covers the repo; **see "GitHub credentials" below** |
| Per-repo GH PAT | Mac Keychain (or Linux disk) → VM → container | one `GH_TOKEN` per repo, injected on each relay; **see below** |
| `/dev/loop-control` + `/dev/loop0..7` | host → container, read-write | opt-in per repo via `customizations.ahjo.nested_incus`; off by default. See **Elevated runtime: `nested_incus`** below. |
| `security.nesting=true` (Incus config) | host kernel → container | opt-in: enabled automatically when the repo declares `ahjo/docker` in its Features, or when `customizations.ahjo.nested_incus` is set. Off by default. See **Elevated runtime: `security.nesting`** below. |

Nothing else crosses — no `~/.ssh/`, no `~/.gitconfig`, no shell history, no `~/Library`.

## Network

Containers sit on the Incus bridge `incusbr0`, which NATs out through the Lima VM (or the Linux host). Left alone, that bridge lets code in a container dial anything the VM can: the Mac's loopback services, the VM itself, your LAN, VPN peers, cloud metadata, and the other containers. `ahjo init` and `ahjo update` close this with two Incus settings, applied by one idempotent step.

The timing and exception behaviour described in this section (the ACL applying to running containers at once, Incus re-attaching `eth0`, established connections surviving, DNS and DHCP being accepted ahead of the ACL) is taken from Incus's documentation and source. It has not yet been confirmed on a live ahjo install.


- **Egress ACL.** A network ACL named `ahjo-egress` is attached to `incusbr0` with a default egress action of `reject`. It allows only public IPv4 plus any CIDRs you opt in (below). `reject` rather than `drop`, so blocked connections fail immediately instead of hanging.
- **Port isolation.** `security.port_isolation`, `security.mac_filtering` and `security.ipv4_filtering` are set on `eth0` of the `default` profile, so containers on the bridge cannot talk to each other. Bridge ACLs cannot filter container-to-container traffic, so this part is required, not optional.

### What is blocked

| Range | What it protects |
| --- | --- |
| `192.168.0.0/16` | The Lima VM and the Mac's loopback services, reached as `host.lima.internal` (192.168.5.2), both in Lima's user-mode network `192.168.5.0/24`; home and office LANs. The addresses at which the VM sees the Mac (`host.lima.internal` and the VM's default gateways, typically the vzNAT gateway 192.168.64.1) are not overridable by `allow_cidrs`; the Mac's own LAN address is not covered (see [What this does not solve](#what-this-does-not-solve)) |
| `10.0.0.0/8` | The `incusbr0` gateway and subnet (`10.20.30.0/24` by default, with fallbacks chosen at init), and so anything listening on the gateway, such as sshd, and sibling containers; corporate LANs and VPNs. The `incusbr0` subnet is not overridable by `allow_cidrs` |
| `172.16.0.0/12` | Private networks, including Docker bridge ranges on the host |
| `100.64.0.0/10` | CGNAT and Tailscale |
| `169.254.0.0/16` | Link-local, including cloud metadata at 169.254.169.254 |
| `127.0.0.0/8` | Loopback |
| `0.0.0.0/8`, `192.0.0.0/24`, `198.18.0.0/15`, `224.0.0.0/3` | Address space that is never a legitimate internet destination: "this network", IETF protocol assignments, benchmarking, multicast and reserved |
| Every IPv4 address on a host interface | A Linux host with a public IP: that address is outside the ranges above, yet dialing it reaches the host's own listeners. Not overridable by `allow_cidrs` |
| Other containers | Sibling containers on the same bridge, via port isolation |

IPv6 is not an exposure: the bridge has no IPv6 address and the ACL has no IPv6 allow rules, so the default reject applies.

### What stays open

- The public internet over IPv4, so `git`, package managers and `apt` keep working.
- DNS and DHCP. Incus accepts TCP/UDP port 53 and DHCP on the host from the bridge ahead of the ACL, and an ACL cannot block that. Anything on the host listening on port 53 is reachable from containers via the bridge, not only Incus's dnsmasq.
- Every channel ahjo itself uses between host and container: sshd, `ahjo expose`, auto-expose, `ahjo forward`, the paste daemon and the SSH-agent socket. These are Incus proxy devices on `127.0.0.1`, relayed between namespaces rather than routed over the bridge, so the ACL does not touch them.

To reach a service on the host from a container, use `ahjo forward`: direct connections to the host's own addresses, the `incusbr0` subnet and, on macOS, the addresses at which the VM sees the Mac are blocked, and stay blocked even if a range containing them is opted in. The Mac's own LAN address is the exception: it is blocked only while its range is not opted in.

### Opting in to private ranges

If containers need a private git host, an internal registry or something behind a VPN, list its CIDR in the global config:

```toml
# ~/.ahjo/config.toml
[network]
allow_cidrs = ["10.1.0.0/16"]
```

On macOS the firewall step runs inside the Lima VM and reads the VM user's `~/.ahjo/config.toml`, not the Mac-side file of the same name (the two are separate and not synced). Edit the one in the VM, for example with `limactl shell ahjo -- sh -c 'nano ~/.ahjo/config.toml'`. `ahjo nuke` deletes the VM and with it this file, so re-add the setting after a rebuild.

Entries must be IPv4 and `/8` or longer. Run `ahjo update` to apply the change; no container restart is needed. Prefer a narrow prefix: an opt-in wins over the block list, so everything inside it becomes reachable from every container. The exception is the host itself: every IPv4 address on one of the host's interfaces (on macOS, the Lima VM), the `incusbr0` subnet (the gateway and sibling containers) and, on macOS, the addresses at which the VM sees the Mac (`host.lima.internal`, 192.168.5.2, and the VM's default gateways, typically the vzNAT gateway 192.168.64.1) stay blocked even inside an opted-in range. The Mac's own address on your LAN (for example 192.168.1.23) is not in that list, because the VM cannot know it. Prefer the narrowest prefix that covers what you need, and do not opt in the subnet your Mac itself is on unless you accept that containers can then reach Mac services listening on all interfaces at that address. Use `ahjo forward` to reach a service on the host.

This setting is read only from the global `~/.ahjo/config.toml`, deliberately not from a repo's `devcontainer.json` (`customizations.ahjo`). That file is controlled by the repo, and the repo is the thing the container boundary is there to contain; if it could widen its own network access, the firewall would be a suggestion.

### What this does not solve

- **Public internet egress stays open.** Code in a container can still send data to any public host. The firewall limits what it can reach, not what it can send out.
- **DNS and DHCP** are accepted by Incus ahead of the ACL (see above), so port 53 and DHCP on the host stay reachable from containers via the bridge.
- **Ports you open on purpose** with `ahjo forward`, and the paste daemon, are reachable from the container by design.
- **The host-address block list is a snapshot** from the last `ahjo init` or `ahjo update`. An address the host gains later is not blocked until the next update.
- **Existing installs and running containers.** Nothing changes until `ahjo update` has run. The firewall is then expected to apply to running containers right away. The first time it is applied, Incus may re-attach each running container's network interface (changing the port-isolation key does this), which can drop open connections and change the container's address, so run `ahjo update` when no container is mid-task and restart a container if it loses network. Connections already open when the ACL was first attached are expected to survive it, because Incus accepts established connections first; restart containers after the first `ahjo update` if that matters. `ahjo doctor` warns when the step is missing.
- **A Mac with a public or otherwise non-private IPv4 address** (some university and corporate networks), **or a Mac LAN address inside an opted-in range**. The host-address block list is read inside the VM and sees the VM's interfaces, not the Mac's, so the Mac's own address is not blocked in either case and Mac services bound to all interfaces are reachable at it. The same goes for LAN neighbours on non-private ranges.
- **A native Linux host with an existing Incus.** The step changes the shared `incusbr0` network and `default` profile, so it also applies to instances ahjo did not create.
- **What `ahjo doctor` checks.** It checks the Incus configuration: ACL attached, default egress reject, port isolation key on the profile. It does not test packets, does not notice rules that are out of date with `allow_cidrs` until `ahjo update` runs, and does not see an instance that overrides `eth0` itself.

### The Lima VM is not a boundary

Under ahjo's threat model, in-container code is hostile and the Incus container boundary is the only boundary. The Lima VM is not a security boundary against that code: it holds the forwarded SSH-agent socket, per-repo tokens and `~/.ahjo` state, and a container escape lands there. The network policy above is defence in depth around the container boundary, not a replacement for it.

## Workspace UID mapping

Each container runs unprivileged in its own user namespace; Incus assigns it a non-overlapping UID range like `0 1074266112 1000000000`. The in-container `ubuntu` user (UID 1000) is *outside* that range, so any file written inside the container as `ubuntu` would be invisible to the Lima VM user (also UID 1000) — and vice versa — without an explicit mapping.

ahjo applies the mapping itself, with two pieces both wired into `ahjo init` / `ahjo update`:

1. **subuid/subgid grants for the Incus daemon**: a one-time idempotent append of `root:<hostUID>:1` to `/etc/subuid` and `root:<hostGID>:1` to `/etc/subgid` so the daemon (running as root) is allowed to delegate those IDs into a container's userns. Without this, `newuidmap` rejects the mapping at container start. The init/update step restarts the Incus daemon when (and only when) it actually appended a line.

2. **per-container `raw.idmap`**: every container ahjo creates gets `uid <hostUID> 1000` + `gid <hostGID> 1000` set on it, mapping the host VM user onto the in-container `ubuntu` user. Applied in `internal/cli/repo.go::wireBranchContainer` (default container) and `internal/cli/create.go::cloneFromBase` (COW branch containers). Files written inside as `ubuntu` land on the VM owned by the Lima user; files owned by the Lima user on the VM appear inside as `ubuntu:ubuntu`. The boundary — Claude inside can't reach UID 0, can't touch other host files, can't escape devices — is unchanged; we widen the namespace by one user, the one we already share with the workspace by construction.

The two-line `uid` + `gid` form is deliberate (instead of incus's shorter `both` form): on Lima setups where macOS uid (e.g. 501) propagates into the VM but the gid stays the Lima default (1000), `both 501 1000` would also try to map gid 501 onto 1000, which `/etc/subgid` doesn't grant. Splitting uid and gid lets each map independently.

See the [Incus docs on `raw.idmap`](https://linuxcontainers.org/incus/docs/main/reference/instance_options/#instance-raw) for the kernel-level mechanism.

## GitHub credentials

ahjo handles SSH and HTTPS remotes differently. SSH leans on agent forwarding (next section). HTTPS uses a per-repo PAT prompted at `ahjo repo add` — and at the first `ahjo create <owner/repo> <branch>`, whose auto-add would otherwise clone before any PAT is in hand — or set later with `ahjo repo set-token <alias>`. On macOS the prompt is the host-side shim (one shared routine for `repo add` and `create`, so they can't drift), because the PAT must land in the host Keychain, not on VM disk. The token is forwarded into containers as `GH_TOKEN` / `GITHUB_TOKEN` and wired into git via `gh auth setup-git`.

Where the PAT lives:

| Host         | Store                                                                          | Reader                                                                                |
| ---          | ---                                                                            | ---                                                                                   |
| macOS        | login Keychain, service `ahjo.GH_TOKEN`, account = repo slug                   | Mac shim only — reads pre-relay, injects `GH_TOKEN` on `limactl shell`                |
| Linux bare-metal | `~/.ahjo-shared/repo-env/<slug>.env` (mode 0600)                           | in-VM ahjo reads disk directly                                                        |

On Mac the shim is the only writer; the in-VM ahjo never touches disk for the PAT. A `grep ~/ -r 'ghp_\|github_pat_'` finds nothing — the Keychain DB is encrypted. `ahjo repo rm` (or `ahjo rm` of the default branch) drops a marker the shim sweeps post-relay so Keychain rows don't outlive the repo.

What this does **not** defend against: code running as you on the Mac (it can call `security` itself, or read the live container's env); the PAT inside the container (any process there sees `GH_TOKEN`). The Keychain move closes accidental disk leakage — Time Machine, `rsync ~`, secret scanners — not an active adversary with shell.

## The SSH-agent hole

`git clone git@github.com:…` inside a container needs an SSH key. ahjo does **not** copy your host keys into the VM or container. Instead, with `ssh.forwardAgent: true` set on both legs, the agent socket is forwarded:

```
agent on Mac (1Password, ssh-agent, …)  ──►  Lima VM  ──►  Incus container
       (signs)                          ($SSH_AUTH_SOCK on each hop)
```

Inside the container, `ssh` and `git` use `$SSH_AUTH_SOCK`, the request travels back across both hops, and the host agent prompts for unlock. Keys never leave the host agent.

The leak: **anything that runs in the container can ask the agent to sign**, for the lifetime of the shell. A malicious dev dependency, a hostile `git` hook, or an unintended Claude action can authenticate to *any* host your agent has keys for — typically every git remote you use. The host agent's per-key authorization prompts are the only check; once you've clicked "always allow" for a key, it signs silently.

ahjo shrinks this hole by **not forwarding the agent when it isn't needed for git**. When a repo's recorded origin is HTTPS *and* a PAT covers it, git authenticates via the token, so ahjo suppresses the proxy device (and strips any inherited one) for that repo's containers — the common HTTPS+PAT case carries no agent socket at all. The agent is still forwarded for SSH origins, PAT-less repos, and non-GitHub remotes, where git genuinely needs it.

You control this with `forward_ssh_agent` in `~/.ahjo/config.toml`:

- *unset* (default) — auto: forward unless an HTTPS+PAT origin covers the repo.
- `true` — always forward (you sign commits over SSH, push `git@` submodules/other remotes, or hit repos the fine-grained PAT isn't scoped to).
- `false` — never forward.

If you need to drop forwarding more bluntly for a given session, two Lima-level mitigations remain:

- Disable forwarding for one shell: `limactl shell ahjo -- env -u SSH_AUTH_SOCK bash` and operate inside the VM without an agent.
- Disable forwarding wholesale: `limactl edit ahjo --set '.ssh.forwardAgent=false'` and restart the VM. `ahjo repo add` against private SSH remotes will then fail until you supply credentials another way (HTTPS + token, deploy key inside the VM, etc.).

### Setup

Lima forwards exactly one socket: whatever `$SSH_AUTH_SOCK` resolves to when `limactl` is invoked. That sounds simple but trips most macOS users on first run, because:

- macOS pre-sets `$SSH_AUTH_SOCK` to a launchd-provided agent (`/private/tmp/com.apple.launchd.*/Listeners`) that is empty unless you explicitly opted into Keychain integration.
- 1Password (and Secretive, KeePassXC, gpg-agent, …) provide their own socket and tell `ssh` about it via `IdentityAgent` in `~/.ssh/config`. That works for host `git`, because host `ssh` reads `~/.ssh/config`. **Lima's ssh transport does not.** It only honors the env var.

ahjo handles this automatically. `ahjo init` runs `pickAgentStep` (`cmd/ahjo/agent_step_darwin.go`), which probes the well-known sockets (1Password, Secretive, gpg-agent) and reads `IdentityAgent` from `ssh -G github.com`, then persists the chosen socket to `~/.ahjo/config.toml [mac].ssh_auth_sock`. Every subsequent `limactl` invocation overrides `SSH_AUTH_SOCK` in its subprocess env via `lima.Env` — no shellrc edits, no VM bounce (the existing ssh ControlMaster is closed via `lima.CloseSSHControlMaster` so the next session picks up the new socket).

If detection finds no reachable agent with keys, the init step errors out — load a key into your agent and re-run `ahjo init`. The only case where you'd still set `SSH_AUTH_SOCK` manually is if you bypass ahjo and call `limactl` directly.

Verify end-to-end with `ahjo doctor` — the host-side block compares host and in-VM key counts and points at the fix when they diverge.

## ahjocontainer.json lifecycle hooks

ahjo reads `.ahjo/ahjocontainer.json` from each repo and executes the
honored subset of [the devcontainer spec's lifecycle commands](https://containers.dev/implementors/json_reference/#lifecycle-scripts):
`onCreateCommand`, `postCreateCommand`, `postStartCommand`, `postAttachCommand`.
These run inside the container as the `ubuntu` user in `/repo` with the
forwarded env vars available, so they have full network egress and full
write access to the per-branch checkout — the same posture as any other
in-container code. Treat them as part of the workload: review them before
running, and rely on the per-container boundary to bound blast radius.

`features:` is honored from Phase 2b: the runner pulls each declared
Feature artifact from the OCI registry (anonymous read; ghcr.io public
Features need no credentials), resolves the dep graph, and runs each
Feature's `install.sh` as root inside the container. Features from the
curated `ghcr.io/devcontainers/features/*` namespace are auto-trusted;
any other source pattern triggers a one-time `[y/N]` prompt at
`ahjo repo add`, recorded as a glob (e.g. `ghcr.io/foo/*`) on the Repo
row in `~/.ahjo/registry.toml`. ahjo's own `ahjo-runtime` Feature is
applied at image-build time inside the transient build container; that
path's trust posture is unchanged.

## Elevated runtime: `security.nesting`

`security.nesting=true` enables Linux user-namespace nesting inside the container, which is required for Docker-in-container (dockerd needs in-userns `CAP_SYS_ADMIN` to manage overlayfs layers) and for nested Incus/LXC workloads. It is **off by default** — ahjo only enables it when the repo's `.ahjo/ahjocontainer.json` declares:

- `"ahjo/docker"` in `features` — the built-in Docker Feature sets `customizations.ahjo.nesting: true` in its `devcontainer-feature.json`, which ahjo reads at `repo add` time before starting the container.
- `customizations.ahjo.nested_incus: true` — the loop-device opt-in for nested Incus; nesting is required to make it useful.

When either path triggers, ahjo applies `security.nesting=true` to the container (stopped), restarts it, and logs `→ security.nesting enabled`. Branch containers cloned via `incus copy` inherit the config flag, so individual branches don't need a restart.

**What this costs.** User-namespace nesting widens the overlayfs kernel attack surface. On bare Linux the container kernel *is* the workstation kernel — a kernel-level escape through the nesting surface compromises the workstation. On macOS via Lima, the Lima VM kernel is the exposed layer; the Mac itself sits behind the `vz` boundary. Repos that never run Docker or nested containers now carry none of this surface by default.

**How it is enforced.** Only `ahjo/*` built-in Features (embedded in the ahjo binary) may request nesting via `customizations.ahjo.nesting: true`. Third-party OCI Features that set the same field are silently ignored — they cannot escalate container privileges through their own metadata. The allowlist is enforced in `internal/cli/features.go:featuresRequestNesting` (Registry=="ahjo" guard).

## Elevated runtime: `nested_incus`

A repo can declare `customizations.ahjo.nested_incus: true` in `.ahjo/ahjocontainer.json` to enable nested Incus storage pools, nested LXC, or any other workload that needs loop-mounted block devices. When set, ahjo wires `/dev/loop-control` and `/dev/loop0..7` into the container at `ahjo repo add` time. The setting is per-repo, checked into git (auditable in PR review), and surfaces a `warn:` line every time it takes effect — there is no global override and no CLI flag.

**What this enables.** `losetup` against an image inside the container, combined with `mount` (already permitted by `security.nesting=true`'s in-userns `CAP_SYS_ADMIN`), means container code can mount *any* block-backed filesystem the host kernel supports — ext4, btrfs, xfs, f2fs, ntfs3, exfat, iso9660, etc. ahjo-in-ahjo is the canonical case: the nested `ahjo init` provisions a btrfs storage pool backed by a `.img` over a loop device.

**What this costs.** Without `nested_incus`, the kernel filesystem drivers reachable from inside a container are limited to `FS_USERNS_MOUNT` types (tmpfs, fuse, overlayfs, ramfs, cgroup, …) — a small, well-fuzzed set. With it, the broader block-FS driver surface becomes reachable via a crafted image. Historical kernel CVEs against ext4/btrfs/xfs/f2fs mounts are not rare; this widens that exposure for any code running in the container, including untrusted dependencies and LLM-driven actions.

The exposure matters per host:

- **Linux bare-metal.** The container kernel *is* the workstation kernel. A container → host kernel escape compromises the workstation.
- **macOS via Lima.** The container kernel is the Lima VM kernel. The VM holds the forwarded SSH agent socket, the in-VM ahjo state, and per-repo GH tokens. The Mac itself is shielded by the `vz` boundary, but the VM is not "throwaway" — an escape there reads agent traffic and tokens for the lifetime of the running VM.

The `/dev/loopN` pool is also host-shared: all containers compete for the same finite slot range, and `LOOP_CTL_ADD` is uncapped — a buggy or hostile workload can starve sibling containers (and the host's own loop-mounted storage pool) until the host is rebooted.

**When to enable.** Only when the repo legitimately runs nested container runtimes or loop-mounted image tooling, and only if you accept the kernel attack surface bump. ahjo's own repo enables it so ahjo-in-ahjo dogfooding works out of the box. The default is off.

## Out of scope

ahjo's boundaries assume in-container code can be hostile — that's the whole point of the per-container isolation. LLM-pulled dependencies, transitively-trusted packages, and prompt-injected agent actions are all in scope, and the boundaries above are sized for them.

What's *not* in scope: hostile code running on the **host**, hostile code on the **other side of an SSH-agent forward** (a hostile remote you connected to, signing requests during the connection window), and kernel-level exploits chained through `security.nesting`'s userns surface or the elevated `nested_incus` surface — those are kernel-bug-class problems, not policy ones. ahjo also doesn't sandbox `git clone` against a hostile remote; the network the container reaches is the network you let it reach.

Review the code you `ahjo repo add` like you'd review any checkout — that's the floor, not the ceiling. The per-container boundary is the ceiling, and it's there *because* in-container code may turn hostile.

## Todo

Known posture gaps, recorded here so they're tracked rather than rediscovered. These are not live exploits; they're about narrowing surface that's currently wider than it strictly needs to be.
