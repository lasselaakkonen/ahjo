# Should ahjo switch to Docker Sandboxes for containerization on macOS?

## Context

ahjo's value proposition is a fast, hard sandbox per `(repo, branch)` for running Claude Code "yolo". The current implementation:

- macOS: Lima VM (`vz` + `vzNAT`, Ubuntu) → Incus system containers, one per branch
- Linux: Incus directly on the host, no VM layer
- Branch container creation is `incus copy` + btrfs reflink — seconds, not minutes
- Inside the container: systemd, root, Docker-in-Incus via the built-in `ahjo/docker` Feature (relies on `security.nesting=true`, btrfs rootfs, mknod/setxattr syscall intercepts — explicitly avoiding the upstream Feature's `privileged: true` + mount path)

Docker released **Docker Sandboxes (`sbx`)** as an early-access product positioning itself as the recommended way to run AI coding agents safely. The user is asking whether ahjo should swap its Lima+Incus core for `sbx` (or its underlying microVM mechanism) on macOS.

**Short answer up front:** No — at least not today, and likely not as a wholesale swap. `sbx` overlaps with ahjo's goal but the design points diverge on the two things ahjo treats as load-bearing: (1) instant per-branch clones with hot caches, and (2) explicit, repo-auditable control over what crosses the boundary. The rest of this doc is the side-by-side that justifies that read, plus where a partial adoption (e.g. supporting `sbx` as a backend or borrowing specific features) would still make sense.

## Quick side-by-side

| Axis | ahjo (today) | Docker Sandboxes (`sbx`) |
| --- | --- | --- |
| Isolation primitive on Mac | Lima `vz` VM → Incus unprivileged system container (userns + `security.nesting`) | microVM per sandbox (hypervisor not documented publicly — likely `Virtualization.framework` on Mac; KVM on Linux) |
| Isolation primitive on Linux | Incus on host (no VM) | microVM on host (KVM, `sudo usermod -aG kvm` required) |
| Kernel boundary | One VM kernel shared across all branches; container userns inside | One kernel **per sandbox** |
| Docker inside | Yes, via built-in `ahjo/docker` Feature; nesting + mknod intercepts | Yes — each sandbox ships **its own Docker daemon** as the headline feature |
| Branch clone speed | `incus copy` + btrfs reflink → seconds, hot `node_modules` / `.venv` carried | Per-sandbox cold microVM boot; `--branch` manages git worktree, image cache is per-sandbox not shared |
| Platforms | Mac arm64 (tested), Mac x64 + Linux x64/arm64 (in principle), no Windows | Mac (Homebrew), Windows (winget), Linux Ubuntu (KVM); arm64/x64 split not documented |
| Network policy | NAT'd via Lima `vzNAT`; egress wide-open by default; per-branch ports via Incus proxy device | **Deny-by-default**, all egress via host HTTP/HTTPS proxy with allowlisted domains; raw TCP/UDP/ICMP blocked |
| Mounts | Only `/repo` (lives in container rootfs, not a host bind), generated SSH host keys (ro), opt-in `/dev/loop*` for nested Incus | Workspace passthrough mount, RW, shared with host filesystem |
| Credentials | Forwarded SSH agent socket (Mac→VM→container), per-repo `GH_TOKEN` from Keychain; allow-listed `forward_env` | Host-side proxy injects auth headers into outbound HTTP; raw secrets not handed to the sandbox |
| Auditing | Out of scope today; egress is unrestricted | Proxy is the choke point; centralized policy + audit features pitched to teams (no public log-format details) |
| Trust posture for adoption | OSS, all behavior visible in the repo; in-VM state inspectable with `limactl shell` | Experimental, closed-source `sbx` CLI, "may be discontinued without notice" per the product page |

## Topic-by-topic

### 1. Isolation level

**ahjo.** On Mac the stack is *VM kernel + unprivileged userns container*. The Mac is shielded by `vz`; containers are isolated from each other by their own userns, filesystem, network namespace, and a non-overlapping subuid/subgid range. They share the VM kernel — that's a deliberate trade-off `CONTAINER-ISOLATION.md` calls out, and the elevated `nested_incus` opt-in is explicitly flagged as widening the in-VM kernel attack surface. On Linux the host kernel *is* the container kernel.

**Docker Sandboxes.** One **kernel per sandbox**. This is a stronger boundary by construction: a kernel bug exploitable from one sandbox doesn't reach the others or the host. On ahjo a successful kernel exploit reaches the VM (Mac) or the workstation (Linux). The Linux-bare-metal case is where the difference is biggest — ahjo on Linux has no equivalent of the per-sandbox kernel.

**Read.** `sbx` wins the strict-isolation axis. Whether that matters to ahjo's users is the real question: ahjo's threat model is *hostile in-container dependency or Claude action*, not *kernel-LPE-armed adversary*. Both products defend the former. Only `sbx` defends the latter cleanly.

### 2. Capability breadth inside the container

Both support Docker-in-sandbox. Both give the agent root inside. Differences are mostly cosmetic except:

- **systemd / long-lived services.** ahjo runs systemd-as-PID-1 (Incus system container), so `sshd`, `ahjo-mirror`, and the user's own services run as units. `sbx` docs don't commit to systemd; a microVM rootfs can have one but Docker's framing is "agent + Docker daemon," not "general-purpose dev VM." If the workflow leans on systemd inside the sandbox (ahjo's does — sshd is how `ahjo ssh` works), that's an ahjo-only behavior today.
- **Nested Incus / loop devices.** ahjo has the `nested_incus` opt-in for ahjo-in-ahjo dogfooding. `sbx` doesn't expose anything analogous; "Docker-in-Docker" works because each sandbox has its own daemon, but mounting arbitrary block-backed filesystems via `losetup` isn't part of the pitch.
- **Pre-installed tooling.** Both have golden images / Features. ahjo's Feature system is the upstream devcontainer spec + an `ahjo/*` namespace for built-ins. `sbx`'s extensibility surface is undocumented in the early-access pages.

**Read.** Breadth is roughly comparable for typical dev workflows; ahjo is broader at the edges (nested Incus, systemd, custom Features).

### 3. Mount/network/audit configuration

This is where the products diverge most visibly.

**Mounts.**
- ahjo: `/repo` lives *inside* the container's rootfs (no host bind-mount); the only host→container bind mounts are the per-branch SSH host-keys dir (ro) and optional `/dev/loop*`. Host `~/.ssh`, `~/.gitconfig`, shell history, `~/Library` — none of it crosses. Per-repo `GH_TOKEN` is forwarded as an env var, not a mount.
- `sbx`: workspace is a **passthrough RW mount** from the host. That's the same posture as `code-on-incus` and `sandbox-claude`, and the opposite of ahjo's "checkout lives in the container" design. ahjo deliberately rejected the host-worktree model (`designdocs/no-more-worktrees.md`).

The mount model is an architectural difference, not a config switch. Switching to `sbx`'s model would put the working tree back on the host filesystem — fast iteration on the Mac side, but the boundary now has to defend a live RW mount instead of a copy. This is a step backwards for ahjo's threat model.

**Network.**
- ahjo: NAT'd via `vzNAT`, no localhost reachback to the Mac, no egress filter. Per-branch ports via Incus proxy devices (`127.0.0.1:<port>` on the VM, surfaced through Lima's port forward).
- `sbx`: **deny-by-default egress through a host HTTP/HTTPS proxy with domain allowlists**; raw TCP/UDP/ICMP blocked. Materially stronger for the AI-agent threat model — a malicious dependency can't `curl` to a C2 host that's not on the allowlist. It also means the proxy is the natural audit point (which Docker pitches but does not publicly document).

**Read.** `sbx` wins decisively on network policy and auditing. ahjo's current network story is "the VM NATs you; the rest is your problem." If a team wanted enforced egress allowlisting and a single audit log, they'd build that on top of ahjo today (e.g. via `nested_incus` + nftables, or in-VM firewalld). `code-on-incus` ships some of this (three-mode network policy, nftables monitoring) — there's precedent for ahjo to add similar without swapping the core.

### 4. Branch startup speed

This is ahjo's headline feature and the place `sbx` is most clearly *behind*.

- ahjo: branch container is an `incus copy` of the repo's default-branch container over btrfs reflinks. The new container inherits a fully-installed toolchain, `node_modules`, language SDKs, and the repo checkout. Wall-clock: seconds. There is no second "package install" tax. See `internal/cli/create.go:254-285` and `internal/cli/repo.go:99,191,491-503` for the mechanics.
- `sbx`: each `sbx run` boots a microVM with the workspace passthrough-mounted. Boot of a microVM on `Virtualization.framework` is fast in absolute terms (~1–3s for a clean VM), but the *workspace state* — Docker image cache, `node_modules`, `.venv` — is per-sandbox and rebuilt each time unless the user wires up shared caches. The `--branch` flag manages a git worktree, not a hot-cache snapshot.

ahjo's model assumes "container per branch with multiple branches in flight." `sbx`'s model assumes "spin up a sandbox for an agent run." Different shapes. ahjo's is more expensive to set up (the base container build takes minutes) but amortizes that across all branches; `sbx` is cheaper to set up and more expensive per branch.

**Read.** For the "multiple PRs in flight, fast switch between them, hot caches everywhere" workflow ahjo is designed for, `sbx` is a regression. For "one-shot agent run, throwaway" `sbx` is great. ahjo's target user is the former.

### 5. Platform support

- ahjo: Mac arm64 (tested), Mac x64 (in principle, untested per README), Linux x64/arm64 (in principle via native Incus). **No Windows.**
- `sbx`: Mac (Homebrew), Windows (winget), Linux Ubuntu (KVM). arm64/x64 split not documented publicly; almost certainly arm64-capable on Mac via `Virtualization.framework`. Linux requires KVM (works on x64 and arm64 Linux given a recent kernel).

**Read.** `sbx` wins on Windows coverage, which ahjo explicitly doesn't support. If Windows is a goal, that's a meaningful gap. If not, ahjo's platform matrix is at least competitive.

### 6. Other differences

- **Trust / longevity.** `sbx` is experimental, closed source, and Docker says it "may be discontinued without notice." Building ahjo's core on it is taking a dependency on a product that explicitly disclaims stability. ahjo is OSS top-to-bottom; users can read the isolation code.
- **Operational surface for the user.** ahjo wires SSH, port forwards, host keys, PATs, env, `gh auth setup-git` into one CLI. `sbx` is narrower (run/rm/branch + Docker daemon). Replicating ahjo's UX on top of `sbx` is rebuilding most of ahjo with a different containerization layer.
- **Where existing OSS lives in the design space.**
  - `code-on-incus` is roughly *ahjo with stronger defaults but no fast-clone story* — Incus system containers + nftables-based active-defense + image build takes 5–10 minutes per session. Comparable isolation primitive, weaker on speed.
  - `sandbox-claude` is *ahjo's twin*: Incus on Linux, Incus inside an OrbStack VM on Mac, btrfs golden images, instant container creation via CoW. Same architecture, different VM provider (OrbStack vs Lima), comparable boundary. This is convergent evolution — it suggests ahjo's design point is reasonable, not idiosyncratic.

## Recommendation

**Don't swap.** The two things ahjo treats as load-bearing — instant CoW branch clones with hot caches, and an in-rootfs working tree rather than a host passthrough mount — are explicitly *not* what `sbx` is built around. Swapping the core for `sbx` would weaken both. ahjo's design overlap with `sandbox-claude` confirms the Incus-CoW shape is a reasonable answer to the same problem.

**What's worth borrowing from `sbx`, though:**

1. **Egress allowlist + proxy.** `sbx`'s deny-by-default proxy is the strongest part of its design and the weakest part of ahjo's. ahjo could add an opt-in `egress_allowlist` (per repo, in `.ahjo/ahjocontainer.json`, mirroring the trust model already used for Features and `nested_incus`). nftables in the VM + an in-VM Squid for HTTPS SNI inspection (`sandbox-claude`-style) would land most of the value without changing the container runtime.
2. **Per-sandbox kernel as an option.** For users on Linux who *do* want kernel-level isolation, an experimental microVM backend for the per-branch sandbox could share the rest of ahjo's UX. This is a sizable change and shouldn't precede stronger user demand, but worth listing as a future option, not a swap.
3. **Auditing.** Whether or not the proxy lands, an opt-in egress audit log (JSONL, per-container) would be a small extension on top of (1) and a real differentiator.

**What's not worth borrowing:** the passthrough workspace mount, the per-sandbox cold start, the closed-source / experimental dependency.

## Sources

- <https://www.docker.com/products/docker-sandboxes/>
- <https://docs.docker.com/ai/sandboxes/> (overview), `…/architecture/`, `…/security/`
- <https://github.com/mensfeld/code-on-incus>
- <https://github.com/pvillega/sandbox-claude>
- `CONTAINER-ISOLATION.md`, `README.md`, `BUILT-IN-FEATURES.md`
- ahjo branch-clone mechanics: `internal/cli/create.go:254-285`, `internal/cli/repo.go:99,191,491-503`
