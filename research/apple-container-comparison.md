# Should ahjo switch to apple/container?

**Question.** Replace ahjo's Lima→Incus stack with [apple/container](https://github.com/apple/container) (and the underlying [apple/containerization](https://github.com/apple/containerization) Swift framework) as the macOS runtime?

**Verdict (short).** No — not as a wholesale replacement. Apple's model has genuinely stronger isolation (a Linux VM per container), but it deletes the two properties ahjo's design is built around: (1) cheap, near-instant per-branch containers via btrfs CoW from a warm "default" container, and (2) portability beyond Apple-silicon macOS 26. There's a defensible middle road as a second backend on Mac for users who want VM-grade isolation and don't mind paying cold-boot per branch — but it would not retire the Lima/Incus path.

The rest of this doc is the evidence. Versions, dates, and limitations are as of 2026-05-19. apple/container is at **0.12.3** (2026-04-30), pre-1.0, with explicit breaking-change windows between minor releases.

---

## 1. Isolation model

| | **ahjo (Lima → Incus)** | **apple/container** |
|---|---|---|
| Isolation unit | Per-branch **system container** (LXC-style) inside one shared Lima VM | Per container is **its own lightweight Linux VM** (Virtualization.framework) |
| Kernel | One shared kernel = the Lima VM kernel | A separate kernel per container; ships a Kata-derived `vmlinux.container` (kata-3.28.0 in 0.12.0) |
| Userns / idmap | Unprivileged userns; per-container `raw.idmap` maps host VM uid → in-container `ubuntu` (uid 1000) — see [CONTAINER-ISOLATION.md:54](/repo/CONTAINER-ISOLATION.md) | No userns concern — the VM boundary *is* the boundary; `vminitd` (Swift) runs as PID 1 over vsock |
| Container-to-container | All siblings share the VM kernel; only filesystem/process/userns separate them | Hard separation by kernel + IP — each container is its own VM |
| Host trust | Lima VM kernel is a single shared blast surface; ahjo's threat model documents this explicitly (`nested_incus` widens it further) | Each VM has its own attack surface; a kernel exploit in one container does not reach siblings |
| Rootless | Incus daemon runs as VM root, but containers are unprivileged; ahjo CLI is normal user | No privileged daemon at all — `container system start` is a per-user launch agent |

**Bottom line on isolation.** apple/container is structurally stricter. ahjo's Incus containers are tight (unprivileged userns, idmapped mounts, syscall intercepts only where Docker-in-container needs them) but they share the Lima kernel. ahjo's design doc names this honestly: kernel-bug-class escapes from `security.nesting` or `nested_incus` are out of scope ([CONTAINER-ISOLATION.md:154-159](/repo/CONTAINER-ISOLATION.md)). For apple/container, the equivalent escape needs to defeat the VM boundary, which Apple's framework treats as its core product.

This is the *one* place apple/container is unambiguously better. Everything below trades against it.

---

## 2. Capabilities inside the container

### Docker-in-container

- **ahjo**: First-class via the built-in `ahjo/docker` Feature ([BUILT-IN-FEATURES.md:62-65](/repo/BUILT-IN-FEATURES.md)). Uses dockerd's modern default (containerd snapshotter + overlayfs + xattr-whiteout) on top of btrfs rootfs; `security.nesting=true` + `mknod`/`setxattr` syscall intercepts give the necessary kernel surface without privileged mode. Commit `3ac257e` removed an earlier forced legacy graph driver; the configuration now relies on dockerd's default. This is the *canonical* in-ahjo workload.
- **apple/container**: Possible but rough. Nested-virt support exists ([containerization #376](https://github.com/apple/containerization/issues/376)), but the shipped Kata kernel has no KVM; users must supply their own. Docker-in-container specifically still hits `iptables (nf_tables) … Could not fetch rule set generation id` ([#1002](https://github.com/apple/container/issues/1002)). Not advertised as a supported workflow.

### systemd

- **ahjo**: Default — `ahjo-base` is a full system container with systemd as PID 1.
- **apple/container**: Works as of **0.4.1** (2025-08-28, [#92](https://github.com/apple/container/issues/92)). Before that, `/sbin/init` hung.

### Other

- **GPU**: Neither supports it on macOS. apple/container has multiple closed wontfix issues + open tracker [#1511](https://github.com/apple/container/issues/1511); Virtualization.framework does not expose Metal to Linux guests. ahjo inherits the same limitation through Lima/vz.
- **Devices** (`/dev/loop*`, USB, etc.): ahjo wires `/dev/loop-control` + `/dev/loop0..7` when a repo opts in via `customizations.ahjo.nested_incus` ([CONTAINER-ISOLATION.md:136-152](/repo/CONTAINER-ISOLATION.md)) — that's how *ahjo-in-ahjo* dogfooding works. apple/container has no equivalent.
- **Capabilities**: apple/container exposes `--cap-add`/`--cap-drop` (0.12.0) and reduced default caps. ahjo runs containers unprivileged with userns; granular cap shaping is not the surface.

**Bottom line on capabilities.** For dev environments specifically, ahjo wins. Docker, systemd, nested Incus, and devcontainer Features are all working today. apple/container can do systemd, can OCI-pull, can BuildKit-build, but it can't run Docker reliably and the device story is a closed door.

---

## 3. Mounts, networks, port forwarding, auditing

### Mounts

- **ahjo**: There are no host bind-mounts of the workspace at all. `/repo` lives **inside** each container's rootfs as a real checkout; branch containers inherit it via btrfs CoW ([designdocs/no-more-worktrees.md](/repo/designdocs/no-more-worktrees.md)). This is the design pivot that lets pnpm hardlinks work and lets `incus copy` finish in seconds. The only host-side surface is `~/.ahjo-shared/` (9p, VM↔Mac, ssh-config & alias map) and a single-file mount for `authorized_keys`.
- **apple/container**: Bind mounts via **virtiofs** (`--volume host:guest`). Known open bugs: corrupted symlinks during tar extract into virtiofs ([#1209](https://github.com/apple/container/issues/1209)), `openat(O_CREAT)` failing without owner-read ([#1344](https://github.com/apple/container/issues/1344)), >300-subdir sharing limitation ([#678](https://github.com/apple/container/issues/678)), no UID/GID mapping ([#165](https://github.com/apple/container/issues/165)). **File-watching across virtiofs is not advertised** — important for any dev loop with bundlers/test watchers/LSPs.

### Networking

- **ahjo**: Per-container port allocation on Mac loopback via `incus proxy` devices (range 10000–10999 in [internal/ports/ports.go](/repo/internal/ports)). Mac → Lima (vzNAT) → container. A "vmnet routable subnet" design exists ([designdocs/mac-vm-subnet-routing.md](/repo/designdocs/mac-vm-subnet-routing.md)) but is not shipped — containers are currently reachable only through allocated loopback ports.
- **apple/container**: On **macOS 26**, each container gets its own routable IPv4 on a host vmnet bridge (default `192.168.64.0/24`) and an `mDNS-style` `<name>.test` hostname. Container↔container works. Multiple isolated networks via `container network create`. On macOS 15 this is broken in important ways (no container-to-container, vmnet race) — so this property *requires* macOS 26.

This is the one place apple/container is unambiguously cleaner than today's ahjo (and on par with where ahjo's vmnet design wants to go).

### Port forwarding

Both support `-p host:container` style. apple/container added port ranges in 0.7.0. ahjo additionally auto-exposes any listener ≥ 3000.

### Traffic auditing / egress policy

Neither tool ships per-container egress firewalling or audit hooks. apple/container has no advertised egress controls; ahjo also has none in the codebase. If "auditing traffic" is a real requirement, **both fail equally** — the right place is a host-level proxy/firewall outside either tool.

---

## 4. Startup speed for the per-branch case

The user's framing: "base VM is already up, the repo's default Incus container exists, user creates a new branch."

**ahjo path** ([designdocs/no-more-worktrees.md:67-75](/repo/designdocs/no-more-worktrees.md)):

1. `incus copy ahjo-foo-bar ahjo-foo-bar-feature-x` — btrfs reflink, **near-zero disk cost**, sub-second.
2. Apply `raw.idmap`, rewire SSH key mount.
3. `incus start` — starts a container against an already-running kernel (no VM boot).
4. `incus exec … git checkout -b feature-x`.
5. Done.

The reflink inherits `node_modules/`, `~/.local/share/pnpm/store/`, the `.git` object database, and any other warm state already present in the default container. This is the property the whole "container as repo" refactor exists to deliver.

**apple/container path** for the equivalent:

1. No snapshot / CoW-fork / fork-from-warm-VM primitive exists — issue search confirms zero "snapshot" issues. Every container = a fresh ext4 block + fresh kernel boot.
2. Image content is content-addressed and cached locally (`container-core-images`), so the image pull is one-time, but the *VM boot* is paid per container.
3. Per-container memory floor is the default `1 GiB, 4 vCPU` per the docs (configurable). Memory ballooning is "only partially supported" (called out in the technical overview) — freed guest memory does not return to macOS.
4. Cold-start is sub-second per container as a claim; third-party benchmarks land around ~1 s ([RepoFlow](https://www.repoflow.io/blog/benchmarking-apple-containers-vs-docker-desktop)).

So *technically* an apple/container "branch" boots in ~1 s — comparable to `incus start`. But:

- You're not inheriting node_modules / pnpm store / git objects from a warm default container, because there is no clone. You'd have to either re-pull from a registry, re-clone from a git server, or design a layer-cache shim — none of which is provided.
- Per-branch idle cost is "one VM with 1 GiB floor and a separate kernel," and that memory is sticky. Ten branches = ~10 GiB sticky, vs. ahjo's ten Incus containers sharing one Lima VM's working set.

**Bottom line on speed.** Pure `start` time is similar. The actual experience — "I just made a branch and my dependencies are warm" — is *currently impossible* on apple/container without building a layer-cache mechanism ahjo gets for free from btrfs. This is the largest concrete regression a switch would cause.

---

## 5. Platform support

| | ahjo | apple/container |
|---|---|---|
| macOS Apple silicon | Yes (Lima vz) | Yes — primary target |
| macOS Intel | Yes (Lima vz/qemu, slower) | **No**, not planned ([#233](https://github.com/apple/container/issues/233)) |
| macOS version | Lima works on macOS 13+ | **macOS 26 (Tahoe) effectively required.** macOS 15 lacks container↔container networking and multi-network support. |
| Linux host | Yes — Lima collapses out, Incus runs directly ([CONTAINER-ISOLATION.md:29](/repo/CONTAINER-ISOLATION.md)) | **No**, not planned |
| Windows host | No | **No** |
| Container arch (Linux/arm64) | Native on arm64, emulated elsewhere | Native |
| Container arch (Linux/amd64) | Native on amd64, qemu elsewhere | Via **Rosetta 2** translation |

The Makefile builds `darwin-arm64 darwin-amd64 linux-arm64 linux-amd64` ([/repo/Makefile](/repo/Makefile)). A switch to apple/container would shrink that matrix to one cell: `darwin-arm64` on macOS 26.

If ahjo has any current or prospective users on Linux workstations, Intel Macs, or pre-Tahoe macOS, switching wholesale is a non-starter.

---

## 6. Other relevant differences

**Maturity and breakage cadence.** apple/container is pre-1.0; the README states API stability is only guaranteed within patch versions, and 0.10.0/0.12.0 both shipped breaking CLI/API changes. ahjo would inherit a moving target as its only Mac runtime.

**Ecosystem.**
- No Docker socket / Docker Engine API ([#66](https://github.com/apple/container/issues/66) closed wontfix). Tools that target `unix:///var/run/docker.sock` (most IDE container plugins, testcontainers, etc.) do not work directly.
- **No native Compose** ([#230](https://github.com/apple/container/issues/230)). Third-party `Container-Compose` is limited.
- **No devcontainer integration** ([#84](https://github.com/apple/container/issues/84) closed without implementation). ahjo is a devcontainer-Features-aware tool — this is the single most awkward gap. Either ahjo keeps doing Features-resolution itself and points the install at apple/container's `exec`, or that whole code path needs rewiring.

**Image / template story.** ahjo builds one `ahjo-base` image at `ahjo init` and never rebuilds it per repo — the base is reused for every repo, then per-repo dev tools install via devcontainer Features into the *default* container, which is then CoW'd per branch. apple/container's OCI image store is content-addressed but does not have an equivalent to "the warm repo-default container is the snapshot source." You'd be reaching for a custom image build per repo, or for a side mechanism to populate a fresh container quickly.

**SSH-into-container.** ahjo gives every container its own sshd on a unique loopback port with generated host keys ([CONTAINER-ISOLATION.md:37](/repo/CONTAINER-ISOLATION.md)). apple/container does not advertise this; you'd run `container exec` or roll your own.

**Mirror.** ahjo's mirror (in-container `ahjo-mirror` watching `/repo`, designdoc [in-container-mirror.md](/repo/designdocs/in-container-mirror.md)) assumes a writable, watchable filesystem inside the container kernel. It would re-architect as a host-virtiofs problem on apple/container — and virtiofs file-watching is the one thing apple's docs are silent on.

**Distribution & install.** apple/container ships signed `.pkg` to `/usr/local`. ahjo ships a Go binary verified by SHA256SUMS via `install.sh`. Neither is hard, but the apple/container installer expects to own a launch agent on the user's machine.

**License.** Apache-2.0 (apple/container) and Apache-2.0 (Incus). No license-side blocker either way.

---

## 7. Recommendation

Don't replace Lima + Incus. The deltas that matter for ahjo's stated goal — *per-(repo, branch) sandboxes that are cheap to create and inherit warm state* — net negative:

- **Branch creation loses btrfs CoW** — the single biggest UX property ahjo has — with no replacement primitive.
- **Linux host, Intel Mac, and pre-macOS-26 users are dropped.**
- **Docker-in-container, devcontainer Features, the SSH-into-container model, and the mirror flow** all require new implementations on top of apple/container.
- **In return** we get one structural improvement (VM-per-container isolation against kernel escapes) and one ergonomic improvement (routable per-container IPs on macOS 26 — already on ahjo's roadmap via vmnet on Lima).

If the kernel-escape worry is the driver, the cheaper move is to harden the Lima boundary (which already holds against `nested_incus`'s widened surface per CONTAINER-ISOLATION.md) and finish the vmnet routable-subnet design ([mac-vm-subnet-routing.md](/repo/designdocs/mac-vm-subnet-routing.md)), not to swap runtimes.

### If the question is "can apple/container be an *alternative* backend?"

Plausible, but it's a sizeable wedge:

- Add a `runtime: apple-container` flag, available only on macOS 26+ arm64.
- Containers under that backend lose: CoW branch clones, in-container mirror, sshd-per-container, devcontainer Features (or those move to a build-time bake step that produces per-repo OCI images), Docker-in-container.
- They gain: per-container kernel + IP, stronger isolation.

The only audience this serves is "I run on macOS 26 + Apple silicon, my workloads don't need Docker-in-container, I'm willing to pay per-branch cold-boot and lose warm `node_modules`, and I want VM-grade isolation." That's a real but narrow set. Worth a one-page design doc before any code, not a wholesale port.

### What to revisit later

- When apple/container ships a snapshot / CoW-fork primitive.
- When virtiofs file-watching is officially supported.
- When Docker-in-container / DinD is reliable out of the box.
- When the macOS version floor (currently effectively 26) is no longer a problem for the user base.

If any two of those land, re-open the question.

---

## Sources

**ahjo (this repo):**
- [/repo/CONTAINER-ISOLATION.md](/repo/CONTAINER-ISOLATION.md)
- [/repo/BUILT-IN-FEATURES.md](/repo/BUILT-IN-FEATURES.md)
- [/repo/designdocs/no-more-worktrees.md](/repo/designdocs/no-more-worktrees.md)
- [/repo/designdocs/mac-vm-subnet-routing.md](/repo/designdocs/mac-vm-subnet-routing.md)
- [/repo/designdocs/in-container-mirror.md](/repo/designdocs/in-container-mirror.md)
- Commit `3ac257e` — `fix(features/docker): stop forcing legacy graph driver in daemon.json`

**apple/container:**
- Repos: [apple/container](https://github.com/apple/container) · [apple/containerization](https://github.com/apple/containerization)
- Docs: [technical-overview](https://github.com/apple/container/blob/main/docs/technical-overview.md) · [how-to](https://github.com/apple/container/blob/main/docs/how-to.md) · [API docs](https://apple.github.io/container/documentation/)
- Releases: [0.1.0](https://github.com/apple/container/releases/tag/0.1.0) · [0.4.1](https://github.com/apple/container/releases/tag/0.4.1) · [0.7.0](https://github.com/apple/container/releases/tag/0.7.0) · [0.10.0](https://github.com/apple/container/releases/tag/0.10.0) · [0.12.0](https://github.com/apple/container/releases/tag/0.12.0) · [0.12.3](https://github.com/apple/container/releases/tag/0.12.3)
- Key issues: systemd [#92](https://github.com/apple/container/issues/92) · DinD [#1002](https://github.com/apple/container/issues/1002) · Linux host [#233](https://github.com/apple/container/issues/233) · Compose [#230](https://github.com/apple/container/issues/230) · Engine API [#66](https://github.com/apple/container/issues/66) · GPU [#1511](https://github.com/apple/container/issues/1511) · virtiofs [#1209](https://github.com/apple/container/issues/1209) [#1344](https://github.com/apple/container/issues/1344) [#678](https://github.com/apple/container/issues/678) [#165](https://github.com/apple/container/issues/165) · devcontainer [#84](https://github.com/apple/container/issues/84)
- Third-party: [RepoFlow benchmark](https://www.repoflow.io/blog/benchmarking-apple-containers-vs-docker-desktop) · [InfoQ](https://www.infoq.com/news/2025/06/apple-container-linux/) · [The New Stack](https://thenewstack.io/apple-containers-on-macos-a-technical-comparison-with-docker/)
