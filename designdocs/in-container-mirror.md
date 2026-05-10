# In-container mirror daemon

**Status:** proposed (v3, gitignore-parity spike folded in 2026-05-10) · **Scope:** `ahjo mirror` only · **Replaces:** the disabled stub at `internal/cli/mirror.go` and the abandoned Phase 2 of `no-more-worktrees.md` · **Depends on:** `no-more-worktrees.md` Phase 1 (already shipped) and the post-mortem in `research/mirror-source-access.md` · **Supersedes:** v1 (fsnotify + debounce + per-tick rsync daemon) and v2 (rsync retained for bootstrap + overflow). v3 removes rsync from the daemon entirely: a parity spike on 2026-05-10 found rsync's `--filter=:- .gitignore` does not honour `!` negation patterns, which would produce silent ghost-files for the canonical `.env*` + `!.env.example` config pattern. Bootstrap is now a daemon-internal walk using the same gitignore matcher as the live path.

## Goal

Restore `ahjo mirror` end-to-end: container `/repo` → user-chosen Mac directory, one-way push, **robust, easy to understand, easy to debug**. The watcher and the per-file copies both run **inside the container**, where `/repo` lives in the container's rootfs on the Incus storage pool (btrfs or zfs depending on `incus admin init`; either way a real kernel filesystem inside the container's mount namespace, not a FUSE shim) and inotify works without restriction. The mirror destination is a Mac path exposed into the container as a normal Incus `disk` device, riding the existing Lima virtiofs share. No reaching into Incus internals, no privilege escalation, no FUSE in the inotify path, no debounce timer, no tree-diff machinery, no rsync.

### Why the mirror exists

The container holds the source tree; some build steps and dev-loop tooling can only run on the Mac host. The mirror's job is to keep the host's view of the source tree current so those host-side workflows have something to read:

- **iOS/macOS native builds.** Xcode, code-signing, simulators — none of these run inside a Linux container. The agent edits Swift/Obj-C files inside the container; the developer runs `xcodebuild` (or hits ⌘R in Xcode) on the Mac against the mirror.
- **Host-side build orchestrators.** Tools like Bazel, Nx, Turbo, Vite, Tilt, or a custom `watchexec` rig that the developer has already configured on their host machine and would prefer not to re-stand-up inside the container. The host's watcher (FSEvents on macOS, inotify on Linux) sees mirror writes and re-fires builds.
- **Host-bound dev-environment dependencies.** A Cloudflare D1 dev DB populated with realistic data, a local Postgres with migrations applied, a Stripe CLI logged into the dev account, an `xcrun simctl` simulator state — anything where the developer has spent real effort wiring the *host's* environment and doesn't want to duplicate that wiring inside every container. The agent generates code in the container; the developer runs the app against the mirror with the host's env, while the agent (in parallel) iterates against a stub or empty DB inside the container.
- **Mac-side `git status` / search / IDE indexing.** Secondary, but real: opening the mirror dir in Finder/VS Code/JetBrains/`rg` Just Works.

The host doesn't need sub-second freshness for any of these — kicking off a build or refreshing an indexer is the limiting factor — but it does need *every* file the agent touches to land on the host eventually, and to land complete (no half-written files visible to a host-side rebuild). Per-event copy with tempfile-rename is the simplest thing that delivers both.

**Caveat for host-side watchers.** Mac FSEvents fires for VM-originated virtiofs writes in current Lima/macOS, but coverage is *not* on par with native FS events: some IDE extensions and FS watchers (TS server, ESLint daemons, Vite HMR) may need `--poll` or a manual reload. Linux-host watchers (inotify on the same kernel) are unaffected because there's no virtiofs hop. Surface this to the user in the `mirror on` success message — "host-side file watchers may need polling mode."

The architectural choice over v1: the kernel hands us per-file events; act on each event by copying the named file. v3's further simplification: bootstrap and overflow recovery are also daemon-internal walks using the same per-file copy routine, so a single gitignore matcher governs every decision the mirror makes. No rsync, no second filter implementation that could silently disagree with the live path.

## Non-goals

- **Bidirectional sync.** Mirror is one-way: container → Mac. Mac-side edits are clobbered on the next event for the same file. Two-way is a different product (and a different set of trade-offs around conflict resolution); out of scope.
- **Deletion tracking.** When a file is deleted in `/repo`, the corresponding `/mirror` file is left in place. Stale files accumulate over time (e.g. after `git checkout` between branches with different file sets); recovery is `ahjo mirror off && rm -rf <target>/* && ahjo mirror on`. Eliminating delete-tracking removes the only thing that requires two-tree reconciliation in the live path; the simplification cascades.
- **Sub-second latency.** Phase 0 had a 200ms debounce; the requirement was Phase 0 inertia, not user-stated. The mirror feeds host-side builders/watchers (Xcode, Bazel, Vite, etc.) and the developer's own dev-loop — none of those re-trigger faster than human reaction time. Live latency in practice is bounded by inotify queueing + a single `cp` syscall (~1–10ms); we don't engineer for it.
- **Cross-FS hardlinks for mirrored content.** Mirror copies; doesn't share storage. The hardlink-friendly path that pnpm depends on is *inside the container*; the mirror is intentionally lossy.
- **Multiple mirrors per container.** One mirror per branch container, like Phase 0.
- **Watching paths outside `/repo`.**

## Why now

The original Phase 2 of `no-more-worktrees.md` aimed to keep the existing VM-side daemon and just repoint its source at `/var/lib/incus/storage-pools/<pool>/containers/<name>/rootfs/repo`. The activation/lifecycle code was written; running it surfaced two distinct problems documented in `research/mirror-source-access.md`:

1. The container rootfs is `d--x------ <idmap-shifted-uid> root` on the host — only root can traverse. Incus's protection model for idmapped containers, not a config oversight.
2. The obvious supported workaround — `incus file mount <name>/repo <path>` — surfaces the directory as `fuse.sshfs`, but inotify on FUSE-sshfs **does not see container-side writes**. It only fires for client-side mutations.

Reversing the mount direction sidesteps both. The container is the inotify side; the Mac is the copy target. inotify against `/repo` runs against the container's rootfs (a real kernel filesystem provided by the Incus storage pool, not a FUSE shim) inside the container's mount namespace — exactly the surface inotify was designed for.

v1 of this doc proposed an fsnotify + debounce + rsync daemon — i.e., catch events, debounce 200ms, then run rsync to reconcile the trees. Working through the design surfaced that rsync's job (compute the diff between two trees) is misaligned with what we actually need (push the file the event named). v2 dropped rsync from the live path and kept it for bootstrap + overflow only; v3 (after the gitignore-parity spike below) drops it entirely, because rsync's filter syntax silently disagrees with git on `!` negation, and a two-implementation gitignore is a built-in correctness gap.

## Topology

```
Mac:        ~/mirrors/foo/bar/feat/                       ← user-chosen
              ↑   (Lima virtiofs, /Users/* preserved, writable)
VM:         /Users/lasse/mirrors/foo/bar/feat/            ← same path
              ↑   (Incus disk device: source=<vm-path>, path=/mirror)
Container:  /mirror/                                      ← writable, idmap-translated
            /repo/                                        ← inotify watches here
              └── /usr/local/bin/ahjo-mirror              ← daemon (inotify + per-file copy + gitignore)
```

The Lima `/Users/<user>` virtiofs mount is `writable: true` in ahjo's standard `lima.yaml` template (verified for the active VM via `limactl list --format=json` — the mount has `"writable":true`). A stock Lima setup defaults to read-only; if anyone forks ahjo's bring-up they must keep the writable mount or this design breaks at the first `cp`.

**On bare-metal Linux** the topology collapses by one layer: there is no Lima VM and no virtiofs hop. The host *is* the VM. The Incus `disk` device's `source=` is a regular host path, the container's `/mirror` is a direct bind-mount of that path, and uid translation is handled entirely by the existing per-container `raw.idmap`. inotify watches in the container, per-file copy, gitignore semantics, and the systemd unit are unchanged. The only platform-specific code path is `validateMirrorTarget` (already correct: `paths.MacHostHome()` returns `(_, false)` on Linux, so only the `~/.ahjo` exclusion guard fires).

UID stack on the way up (a write inside the container surfaces on the Mac with the user's ownership):

```
container ubuntu (uid 1000)
  ↓ raw.idmap: container 1000 → VM 501          (already present on every ahjo container)
VM lasse (uid 501)
  ↓ Lima virtiofs passthrough (Mac 501 ↔ VM 501)
Mac lasse (uid 501)
```

Same chain Phase 0 used in the *opposite* direction. Verified for read-only mounts (`ahjo-host-keys`, `ahjo-authorized-keys` at `internal/cli/repo.go:391-404`) **and now verified for writable disk devices via a pre-implementation spike** (see "Spike: writable disk device under raw.idmap" below). No `shift=true` needed; raw.idmap delegation passes uid 1000 → 501 cleanly through to the Mac.

### Spike: writable disk device under raw.idmap (resolves Open Question 1)

Run on `2026-05-10` against an existing ahjo container (`ahjo-lasselaakkonen-ahjo-e2e-sandbox-test-1`, raw.idmap = `uid 501 1000\ngid 1000 1000`, kernel 6.8 in Lima, virtiofs mount of `/Users/lasse`):

```
limactl shell ahjo -- incus config device add <c> mirror-spike disk \
    source=/Users/lasse/tmp/ahjo-mirror-spike path=/mirror
limactl shell ahjo -- incus exec <c> --user 1000 -- \
    bash -c 'mkdir -p /mirror/sub && echo nested > /mirror/sub/n.txt'
```

Observed:

| Layer       | uid:gid          | resolves to               |
|-------------|------------------|---------------------------|
| Container   | `1000:0`         | ubuntu, root              |
| VM (Lima)   | `501:1000`       | lasse, lasse              |
| **Mac**     | **`501:20`**     | **lasse, staff**          |

Mode bits and content preserved. Subdirectories created from inside as `ubuntu` are reachable, listable, and editable on the Mac as the regular user. **No `shift=true` was needed**; the existing per-container `raw.idmap` is sufficient because writes from container uid 1000 surface on the VM as host uid 501, and Lima's virtiofs passes uid 501 verbatim through to the Mac.

Quirks noted but not load-bearing:
- `/mirror`'s root inode reports as `0:0` *inside the container* (a virtiofs-on-bind-mount cosmetic), but writes succeed because the underlying VM-side dir is owned by uid 501 (= ubuntu via raw.idmap) with mode 755.
- The created file's gid surfaces as `0` inside the container, then maps cleanly to the user's gid on the way out (VM gid 1000, Mac gid 20). Owner uid is what gates the host-side experience; gid quirks are invisible to the developer.

Conclusion: ship the design as written. Drop `shift=true` from consideration, drop the COI-Lima-idmap-workaround interaction worry, and let `internal/cli/mirror.go` add the disk device with no extra device options.

### Spike: gitignore parity (resolves Open Question 2)

Run on `2026-05-10` against six handcrafted fixtures plus the live ahjo
repo. Each fixture is a real git repo; for every path we record three
verdicts (ignored vs not), with `git check-ignore --verbose --non-matching
--stdin` as ground truth, and count disagreements pairwise. Source +
fixture builder are checked into `research/spike-gitignore/`.

Disagreement rates against `git check-ignore`:

| Fixture                                       | sabhiram (root only) | go-git plumbing/format/gitignore | rsync `--filter=:- .gitignore` |
|-----------------------------------------------|----------------------|----------------------------------|--------------------------------|
| A-flat (root rules + `!important.log`)        | 20.0% (3/15)         | **0.0%**                         | 6.7% (`!important.log`)        |
| B-nested (per-dir + nested `!keep.tmp`)       | 31.6% (6/19)         | **0.0%**                         | 5.3% (`!keep.tmp`)             |
| C-globs (`**/foo`, `qux/**`, `/bar`)          | 6.7% (1/15)          | 6.7% (qux dir entry only)        | **0.0%**                       |
| D-neg (negation chain)                        | 0.0%                 | **0.0%**                         | 20.0% (`!debug.log`)           |
| E-monorepo (per-package nested)               | 27.8% (5/18)         | **0.0%**                         | **0.0%**                       |
| F-env (`.env*` + `!.env.example`)             | 0.0%                 | **0.0%**                         | 16.7% (`!.env.example`)        |
| ahjo (this repo, 136 paths)                   | 1.5% (2/136)         | **0.0%**                         | **0.0%**                       |

Conclusions:

- **`sabhiram/go-gitignore` is unusable.** Its idiomatic form (load only the
  root `.gitignore`) misses nested files and miscategorises directory-only
  patterns (`dist/` vs `dist`); 20–30% disagreement on realistic fixtures.
  Adopting it would mean every monorepo, every project with a `submodule`,
  and every project whose `.gitignore` ends with a `/` produces ghost files
  on the Mac.
- **`github.com/go-git/go-git/v5/plumbing/format/gitignore` is git-faithful**
  on every realistic case. The single non-zero case (`qux/**`'s parent
  directory entry in C-globs) is the documented "`qux/**` matches contents
  but not the directory itself" git distinction. For a watch-and-copy
  daemon this is functionally equivalent: nothing under `qux/` ever
  surfaces on the Mac either way.
- **rsync's `--filter=:- .gitignore` does NOT honour `!` negation patterns.**
  Any project using `*.log` + `!important.log`, `.env*` + `!.env.example`,
  `dist/` + `!dist/static.css`, etc., produces a silent bootstrap-vs-live
  disagreement: rsync skips the negated file, the live daemon (using
  go-git) copies it on next mutation, and the Mac sees a stale state in
  between. The `.env*` + `!.env.example` pattern alone disqualifies rsync
  — it is the canonical config shape in Vite, Next.js, Rails, Django, and
  dozens of framework starters.

Decision:

- **Live + bootstrap both use `github.com/go-git/go-git/v5/plumbing/format/gitignore`.**
  One library, one matcher, identical decisions across phases. No
  bootstrap-vs-live disagreement is possible by construction.
- **rsync leaves the mirror's hot path entirely.** Bootstrap is a
  daemon-internal walk: `filepath.WalkDir` over `/repo`, consult the
  matcher per-path, for each kept regular file `os.Stat` source and dest
  and skip when size+mtime match, otherwise copy via the same
  tempfile-then-rename routine as the live path. Symlinks reuse the live
  path's `os.Lstat` + `os.Readlink` + `os.Symlink` branch. IN_Q_OVERFLOW
  recovery invokes the same walk function. (rsync remains installed by
  `ahjo-runtime` for developer convenience, but the daemon does not invoke
  it.)
- The fixture set above is locked into the integration suite as
  `internal/mirror/gitignore_parity_test.go`; future library upgrades or
  new repo conventions catch silently broken parity at CI.

## Phase 1 — implementation

### CLI behavior

`ahjo mirror <alias> --target <mac-path> [--no-skiplist]`

The full flow runs under `internal/lockfile.Acquire()` (the same lock `mirror off` already takes), so two simultaneous `mirror …` invocations don't race on incus device config or unit state.

1. Validate target. `validateMirrorTarget` (recoverable from commit `2b3b997`) refuses paths under `~/.ahjo/` on every host, and refuses paths outside the Mac home only when `paths.MacHostHome()` reports a Mac home — true under Lima, false on bare-metal Linux.
2. Resolve container name from registry. **Refuse if the container is stopped** — print hint to run `ahjo shell` first. (Memory: `project_ahjo_mirror_lifecycle_coupling.md` — activation must not become a hidden way to start containers.)
3. Refuse if any other container already has an active mirror (single-active in v1; state lives in incus device config).
4. Reconcile the daemon binary. If `/usr/local/bin/ahjo-mirror` is missing OR its embedded version stamp doesn't match the CLI's expected stamp: **stop the unit if running** (`systemctl stop ahjo-mirror.service`, tolerates "not loaded"), `incus file push` the embedded binary, then continue. Stop-push-start avoids any ambiguity about replacing a running binary's text segment on whatever kernel/incus combination the user is on; cost is ~1s of mirror downtime during upgrades.
5. Reconcile `--no-skiplist`. Write or remove the systemd drop-in `/etc/systemd/system/ahjo-mirror.service.d/flags.conf` setting `Environment=AHJO_MIRROR_NO_SKIPLIST=1` when the flag is in effect. The daemon reads the env var on startup and, when set, skips the static skiplist filter; gitignore still applies. Drop-in form keeps the base unit file untouched and reverts cleanly on the next activation.
6. `mkdir -p` the Mac target dir as the user.
7. `incus config device add <container> mirror disk source=<vm-path> path=/mirror` — idempotent (tolerates "already exists" via the same pattern as `internal/incus/incus.go:82`).
8. `incus exec <container> -- systemctl daemon-reload` (cheap; idempotent).
9. `incus exec <container> -- systemctl enable --now ahjo-mirror.service`. Daemon's first action is the bootstrap walk; from there it processes events.
10. **Skiplist-presence warning.** After enable succeeds, `incus exec <c> -- find /repo -maxdepth 4 -type d \( -name node_modules -o -name .git -o ... \) -prune -print` for every static skiplist name. If any matches surface, print them after the success message: *"these directories will not be mirrored — pass `--no-skiplist` if you need them"*. The `-prune` keeps the find cheap on `node_modules`-heavy trees.

`ahjo mirror off`
1. `incus exec <container> -- systemctl disable --now ahjo-mirror.service`. Tolerates "not loaded."
2. `incus config device remove <container> mirror` (existing `incus.RemoveDevice` at `internal/incus/incus.go:309`). Tolerates "not found."

State is the disk-device presence in incus; no separate state file.

`ahjo mirror status`
1. List containers across the registry; for each, check whether a `mirror` disk device is configured (`incus config device list <c>`).
2. For each active mirror: `incus exec <c> -- systemctl is-active ahjo-mirror.service` (treat exit code 3 as "inactive," not as error).
3. Print alias, container, source path (`/repo`), target (the device's `source=`), and current unit state.

`ahjo mirror logs <alias>`
1. `incus exec <container> -- journalctl -u ahjo-mirror.service -n 200 --follow`.

Status and logs are passthroughs to `systemctl` and `journalctl`. We do not invent a state machine; systemd already has one and the user's existing tooling already speaks it.

### Daemon (`ahjo-mirror`)

A small Linux binary, ~200 lines. Three responsibilities, in this order at startup:

1. **Install watches first.** Walk `/repo`, register an inotify watch on every directory not in the static skiplist and not gitignored. Order is deliberate: watches before bootstrap means any change between watch-install and bootstrap-end queues an event we'll drain after.
2. **Bootstrap once.** Daemon-internal walk: `filepath.WalkDir` over `/repo`; for each entry consult the gitignore matcher (skip if ignored), `os.Lstat` and route to the same per-file copy routine the live path uses (regular file → tempfile-rename copy with size+mtime delta skip; symlink → `os.Readlink` + `os.Symlink`; anything else → log + skip). One function, shared verbatim with the live event handler — by construction the bootstrap and live phases produce identical Mac-side artifacts. Reused on `IN_Q_OVERFLOW`. The size+mtime delta keeps repeated bootstraps cheap (no rewrite when content already matches).
3. **Process events live.** For each event under `/repo`:
   - Strip `/repo/` prefix; compute target path under `/mirror/`.
   - Skip if path matches the static skiplist. v2 trims the inherited list to **directories that are categorically never source code and reliably blow past inotify limits when populated** — `.git`, `node_modules`, `__pycache__`, `.venv`, `venv`, `.pytest_cache`, `.ruff_cache`, `.mypy_cache`, `.next`, `.nuxt`, `.svelte-kit`, `.turbo`. Dropped from `internal/mirror/daemon.go:23`: `dist`, `build`, `target`, `vendor` — these collide too often with real source dirs (Python wheels, vendored Go modules, projects publishing a `dist/` of edited config). `.gitignore` handles them in the typical case; the static list is no longer the place to second-guess. Operators who want zero static filtering pass `--no-skiplist` to `ahjo mirror on` (gitignore still applies).
   - Skip if path matches loaded gitignore rules.
   - On `Create` for a directory: `mkdir` the target, install a recursive watch on the new dir, walk the new dir for any files already present and copy each. Closes the new-dir-CREATE race (carry `internal/mirror/daemon.go:87-91` verbatim).
   - **Source read is always lstat-first.** Before any open of a `/repo` path, `os.Lstat` it and decide based on the result. If it's a regular file, open with `os.OpenFile(src, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)` so a mid-event symlink swap can never trick the daemon into reading through a link. If it's a symlink, take the symlink branch below. Anything else (socket, device, fifo) is logged and skipped.
   - On a regular-file `Create` / `Write` / `Rename` (target side): copy via Go's `io.Copy` to `<dst>.ahjo-mirror.tmp.<rand>` in the same target directory, `os.Chmod` to match source, then `os.Rename` over the final path. Tempfile-in-same-dir keeps the rename atomic on the same FS, so a partial copy can never be observed by a host-side reader. The rename is naturally idempotent w.r.t. anything bootstrap already wrote — overwriting a freshly-bootstrapped file with the same content during the bootstrap-window event drain is a no-op from the host's perspective.
   - On a symlink: recreate as a symlink on the target side via `os.Readlink` + `os.Symlink` (the bootstrap walk uses the same routine, so live and bootstrap can never disagree on link handling). **Do not follow symlinks live** — a link to `/etc/passwd` inside the container must not surface its target on the Mac.
   - On any `.gitignore` write *or* `.gitignore` create-via-tempfile-rename (editors like vim, JetBrains, VS Code use rename-on-save, so trigger on both): exit non-zero. systemd restarts; bootstrap walk re-runs with new rules. Crude but very simple — no in-memory invalidation logic to test or debug.
   - On `IN_Q_OVERFLOW`: re-run bootstrap walk. Logged.

**Bootstrap-window idempotency.** Watches are installed *before* bootstrap runs, so any write during bootstrap queues an event the daemon drains afterwards. For each such event the daemon will issue a per-file copy on top of whatever the bootstrap walk already wrote. Because both phases call the same copy routine, the tempfile-then-rename pattern makes that overwrite atomic and observably a no-op when content is identical; when content has changed in between, the second copy publishes the newer bytes atomically. A host-side reader/builder never catches a half-written file even when bootstrap and the post-bootstrap drain both touch the same path. No deduplication logic, no event-vs-walk coordination — the FS-level atomic rename is the synchronization primitive.

Two layers of filtering, by deliberate split:

- **Static skiplist** = a watch-count guard. An 80k-dir `node_modules` would exhaust `fs.inotify.max_user_watches` regardless of what's in `.gitignore`. Hardcoded list keeps watches bounded for any repo.
- **gitignore evaluation** = repo-specific rules and individual ignored files. Loaded from `/repo/.gitignore`, nested `.gitignore` files, and `/repo/.git/info/exclude`. Library: `github.com/go-git/go-git/v5/plumbing/format/gitignore` — the spike above validated 0% disagreement with `git check-ignore` across six fixtures and the live ahjo repo, including nested `.gitignore`s and `!` negation chains. Used identically by the live event filter AND the bootstrap walk: one matcher, no parity gap.

The skiplist alone wouldn't catch a project that uses `output/` instead of `dist/` or an individual `*.bak` rule. gitignore alone wouldn't bound watches for a fresh `pnpm install`. We need both.

Logging: stderr, captured by systemd journal. Each copied file is one log line. No log file in `/mirror` (would mirror its own log).

### systemd unit

Shipped via the `ahjo-runtime` Feature (image build) AND pushable on demand by the CLI (self-heal for older containers).

```ini
# /etc/systemd/system/ahjo-mirror.service
[Unit]
Description=ahjo: mirror /repo to host-side /mirror
ConditionPathIsMountPoint=/mirror

[Service]
Type=simple
User=ubuntu
ExecStart=/usr/local/bin/ahjo-mirror /repo /mirror
Restart=always
RestartSec=2
# Disable systemd's start-rate limit. The daemon exits intentionally on
# .gitignore change; default StartLimitBurst=5 would put the unit in
# `failed` state after a handful of legitimate restarts.
StartLimitBurst=0
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

Notes:
- `Type=simple`: long-running event consumer.
- `Restart=always` (not `on-failure`): a clean exit on `.gitignore` change should also be restarted.
- `StartLimitBurst=0`: disables the failure-after-N-restarts cliff.
- `ConditionPathIsMountPoint=/mirror`: blocks start when the disk device isn't attached. Failed condition is benign — unit just doesn't run, doesn't error.
- We `enable` the unit so it auto-starts after container reboot. Safe because resume is just "re-bootstrap from current state" — no daemon state to reconstruct. **Different from v1**, which deliberately *didn't* auto-resume; v2 can because resume is idempotent.

### State

No state file. `internal/mirror/state.go` is deleted.

State lives where the operations actually happen:
- The presence of a `mirror` disk device on a container → mirror is configured.
- `incus config device list <c>` + `disk.source=` → target path.
- `systemctl is-active ahjo-mirror.service` → current run state.

This eliminates the entire class of "state file says X but daemon says Y" inconsistencies the original `mirror.go` carried (PIDAlive checks, stale-PID detection, atomic-rename semantics). incus and systemd are the source of truth.

### Binary delivery

Two complementary paths:

1. **Embedded in the `ahjo-runtime` Feature** for cold-start: `install.sh` detects `dpkg --print-architecture`, copies the matching pre-built binary to `/usr/local/bin/ahjo-mirror`, drops the systemd unit, runs `daemon-reload`. Fresh containers carry the daemon out of the box.
2. **Pushed by the host CLI** for self-heal: on `mirror on`, if the binary is missing OR a version-stamp mismatch is detected, the CLI stops the unit (if running), `incus file push`es the embedded binary, and continues activation. See step 4 of CLI behavior.

The host CLI release bundles the linux binary matching its host arch (linux/arm64 in the darwin/arm64 CLI; linux/amd64 in darwin/amd64). One ~5MB binary added per release artifact.

`go:embed` chains the build, with a generate step to keep the embedded bytes honest:
- A `//go:generate` directive (in `cmd/ahjo-mirror/generate.go`) runs `make build-mirror-linux` and produces `ahjo-mirror.linux-{arm64,amd64}` next to the embed targets. `go generate ./...` is the single source of truth for "rebuild the daemon binaries."
- `Makefile`'s default target runs `go generate ./...` before any `go build` of the CLI, so `make` users always have fresh embedded binaries.
- **CI gates on `go generate ./... && git diff --exit-code`.** A stale embed lands as a non-empty diff and fails the build with a clear message. This is the safety net for non-`make` builds (`go build ./...`, IDE builds, manual builds in worktrees) that would otherwise silently embed last-build's bytes.
- The CLI embeds the matching arch directly.
- The Feature dir embeds both arches via `go:embed all:feature` (existing pattern at `internal/ahjoruntime/embed.go:14`).

### Code changes

| File | Change |
|---|---|
| `cmd/ahjo-mirror/main.go` (new) | ~250-line Linux binary: install watches, bootstrap walk (filepath.WalkDir + per-file copy), inotify event loop, gitignore filter (go-git matcher), per-file copy with size+mtime delta + tempfile-rename, exit-on-gitignore-change. Honors `AHJO_MIRROR_NO_SKIPLIST=1` (set via systemd drop-in when CLI was invoked with `--no-skiplist`) by skipping the static skiplist filter; gitignore still applies. |
| `cmd/ahjo-mirror/generate.go` (new) | `//go:generate` directive that builds both linux arch binaries to the embed location. Invokes `go build` directly with `GOOS=linux GOARCH={arm64,amd64}` — no `make` round-trip — so the build dependency graph is acyclic. |
| `internal/mirror/daemon.go` | Demoted to library code shared with `cmd/ahjo-mirror`: skiplist + gitignore loader (go-git matcher across nested `.gitignore`s) + watch-add helper + per-file copy. **Trim the existing skiplist** — drop `dist`, `build`, `target`, `vendor`; keep `.git`, `node_modules`, `.next`, `.nuxt`, `.svelte-kit`, `__pycache__`, `.venv`, `venv`, `.pytest_cache`, `.ruff_cache`, `.mypy_cache`, `.turbo`. The current rsync wrapper, debounce timer, and `RunDaemon` go away. |
| `internal/mirror/gitignore_parity_test.go` (new) | Pin the spike fixtures (A–F + a "real ahjo repo" snapshot) into the test suite. For each fixture, walk every path; assert the daemon's matcher verdict equals `git check-ignore`'s on every entry. Catches a future library swap, an upstream regression, or a new repo convention silently breaking parity. |
| `go.mod` | Add `github.com/go-git/go-git/v5` (used only for `plumbing/format/gitignore`; nothing else in go-git is pulled in at runtime). Drop `github.com/sabhiram/go-gitignore` if it had been added speculatively. |
| `internal/mirror/state.go` | **Deleted.** State lives in incus + systemd. |
| `internal/cli/mirror.go` | Replace `mirrorDisabledMsg` with the activation flow: `lockfile.Acquire`, validate target, refuse if other mirror active, stop-push-start on stale binary, write/remove `--no-skiplist` drop-in, add disk device, daemon-reload, enable+start, run skiplist-presence find and warn. New `--no-skiplist` flag. |
| `internal/incus/incus.go` | Add thin wrappers: `SystemctlDaemonReload`, `SystemctlEnableNow`, `SystemctlDisableNow`, `SystemctlIsActive` (treat exit 3 as inactive), `SystemctlStop`. `RemoveDevice` already exists at `:309`. |
| `internal/ahjoruntime/feature/install.sh` | Install pre-built `ahjo-mirror` binary (arch-matched), drop the systemd unit, run `daemon-reload`. |
| `internal/ahjoruntime/feature/` | Add `ahjo-mirror.linux-amd64` + `ahjo-mirror.linux-arm64` + `ahjo-mirror.service`. |
| `Makefile` | Default target runs `go generate ./...` (which produces `ahjo-mirror.linux-{arm64,amd64}`) before any CLI build. New target `build-mirror-linux` is what `go generate` invokes under the hood. |
| `.github/workflows/ci.yml` (or equivalent) | New step: `go generate ./... && git diff --exit-code`. Catches stale embedded daemon binaries before merge. |
| `internal/cli/rm.go`, `internal/cli/shell.go --update` | Pre-destroy hook: `mirror off` if the to-be-destroyed container has a mirror device (memory: `project_ahjo_mirror_lifecycle_coupling.md`). |
| `internal/initflow/...` (new step) | Bump `fs.inotify.max_user_watches=1048576` AND `fs.inotify.max_queued_events=65536` via `/etc/sysctl.d/99-ahjo.conf` + `sudo sysctl --system`. Same code path on Mac (writes inside the Lima VM) and bare-metal Linux (writes on the host). **Not** in `internal/preflight/preflight.go` (which is read-only; v1 misstated this). |

Estimated diff: ~200 lines of new Go in `cmd/ahjo-mirror`, ~150 lines of modified Go in `internal/cli/mirror.go` + `internal/incus/incus.go`, ~30 lines of shell + systemd, plus the embedded binaries. Smaller than v1's 300–400-line estimate because no debounce, no per-tick rsync wrapper, no state file, no PID tracking.

### Lifecycle hazards

1. **Container stops with mirror on.** Daemon dies (container's PID 1 systemd goes with it). On next start, unit auto-resumes (we `enable`d it); first action is the bootstrap walk to converge from any prior state. Disk device is reattached automatically on container restart. **Different from v1**: v1 deliberately *didn't* auto-resume; v2 onward does, safely, because resume is idempotent.
2. **Mac target dir disappears mid-session.** Disk device's `source=` becomes invalid; per-file copies fail; daemon logs and continues. If the dir comes back, copies resume.
3. **inotify watch limits.** Lima's default `fs.inotify.max_user_watches` is small. The static skiplist keeps a typical web-app repo well under 8k watches. Big monorepos may hit the limit — daemon detects and logs the watch count installed at startup. Mitigation: bump the sysctl on the **Lima VM**, not on the container. `linux.sysctl.fs.inotify.*` is **not** among the namespace-scoped sysctls Incus exposes via per-container `linux.sysctl.*`; the existing precedent `internal/cli/repo.go:440` (`linux.sysctl.net.ipv4.ip_unprivileged_port_start`) only works because `net.*` is namespace-scoped. The bump goes in a new `ahjo init` step that writes `/etc/sysctl.d/99-ahjo.conf` + `sudo sysctl --system`.
4. **Daemon's own log loop.** Logs go to journald inside the container; container-local; accessible via `ahjo mirror logs`. Never crosses into `/mirror/`.
5. **Disk device left behind on crash.** If `mirror on` crashes between `device add` and unit enable, the device sticks around. Idempotent `mirror on` (re-add tolerates "already exists") plus `mirror off` always tries to remove the device — same idempotency the rest of `internal/incus/incus.go` already uses.
6. **`ConditionPathIsMountPoint` only checked at start.** If the disk device is removed mid-run (race with `mirror off`, or external `incus config device remove`), per-file copies fail; `Restart=always` re-fires; condition blocks subsequent starts. Net behavior is benign — unit goes inactive, no error loop.
7. **New-dir-CREATE race.** Files written into a freshly-created subdirectory before our watch lands inside. Mitigated by walking the new dir on the parent's `Create` event and copying any files already present. Carry `internal/mirror/daemon.go:87-91` verbatim.
8. **`IN_Q_OVERFLOW`.** Daemon detects the kernel's overflow signal and re-runs the bootstrap walk. Logged. Recoverable.
9. **`.gitignore` edits.** Daemon exits non-zero; systemd restarts within 2s; bootstrap walk re-runs with fresh rules. Cost: ~2s of "rules might be stale" right after a `.gitignore` save plus a full walk of `/repo` (which can be multi-second on a 30k-file monorepo, especially when a `git checkout` between branches touches multiple nested `.gitignore`s and triggers a restart per file). **Accepted** — `.gitignore` edits are rare in steady-state development, and the size+mtime delta in the per-file copy routine keeps the walk cheap when nothing has actually changed (only `.gitignore` itself). The cost of not maintaining an in-memory ignore index would be invalidation logic to test and debug; not worth it for this rate of change.
10. **Stale Mac files after branch switch.** A `git checkout` that removes files in `/repo` does **not** remove their `/mirror` counterparts (no delete tracking). Documented behavior. Recovery: `ahjo mirror off && rm -rf <target>/* && ahjo mirror on`.
11. **Symlinks: bootstrap and live agree by construction.** Both phases call the same per-file routine, which routes symlinks through `os.Lstat` + `os.Readlink` + `os.Symlink` and never `io.Copy`s a link's target. A link to `/etc/passwd` inside the container can never surface its contents on the Mac.
12. **Bootstrap-window write race.** Watches install before bootstrap, so a write during bootstrap queues an event the daemon drains afterwards. The post-bootstrap drain may issue a per-file copy on top of whatever the bootstrap walk wrote for the same path. The tempfile-then-`os.Rename` pattern makes that overwrite atomic and content-equivalent — a host-side reader/builder never sees a half-written file even when both bootstrap and the live event drain touch the same file. No deduplication logic needed; the FS-level atomic rename is the synchronization primitive.

### What survives a `git push`

The mirror is a one-way *file* push. `.git/` is excluded explicitly — the user's editor sees source files but the Mac directory is not a git working tree. Opening the Mac dir and running `git status` returns "not a repo." Documented behavior, same as Phase 0 and v1.

## Test plan

Manual on the e2e-sandbox:

1. `ahjo mirror lasselaakkonen/ahjo-e2e-sandbox --target ~/tmp/sandbox-mirror`. Verify `/repo` content appears under `~/tmp/sandbox-mirror` with uid `lasse`.
2. **Per-file push:** `incus exec <c> -- bash -c 'echo X >> /repo/README.md'`. Mac file reflects the new line within ~50ms.
3. **New directory:** `incus exec <c> -- bash -c 'mkdir /repo/sub; touch /repo/sub/a /repo/sub/b'`. Both surface on Mac. New dir is watched.
4. **Skiplist filtering:** `incus exec <c> -- mkdir -p /repo/node_modules/x && touch /repo/node_modules/x/y`. Verify NOT on Mac.
5. **gitignore filtering, repo-specific:** edit `.gitignore` to add `*.bak`; expect daemon to restart + re-bootstrap. Then `touch /repo/foo.bak`. Verify NOT on Mac.
6. **Delete behavior (deliberate non-tracking):** `incus exec <c> -- rm /repo/README.md`. Mac file remains. Document this in the test.
7. **Burst:** `incus exec <c> -- bash -c 'for i in {1..1000}; do echo $i > /repo/file$i.txt; done'`. All 1000 files surface on Mac within a few seconds. Daemon either drains events one by one or hits IN_Q_OVERFLOW and re-walks; either path converges.
8. `ahjo mirror status` — reports active.
9. `ahjo mirror logs` — shows per-event copy lines.
10. `ahjo mirror off` — daemon stops, disk device removed, `/mirror` no longer in the container.
11. **Container stop while mirror on:** `incus stop <c>`. Then `incus start <c>`. Mirror auto-resumes; bootstrap walk re-runs to converge any drift while stopped.
12. **inotify exhaustion:** target a synthetic 50k-file repo without bumping the sysctl; verify the daemon logs the partial watch count and degrades gracefully (events on watched dirs still flow; missed dirs caught by next bootstrap walk).
13. **raw.idmap on writable disk:** Already verified by the pre-implementation spike (see "Spike: writable disk device under raw.idmap"). Re-run as a regression check inside the integration suite: `incus exec <c> --user 1000 -- touch /mirror/probe`; on Mac, expect uid 501 / lasse. Failure here means the kernel/Lima/Incus combination drifted from what the spike validated.
14. **Symlinks live + bootstrap parity:** `incus exec <c> --user 1000 -- ln -s README.md /repo/README.link`. Verify the Mac side has a *symlink* (not a copy) and that `readlink` returns `README.md`. Then `ahjo mirror off && ahjo mirror on` and re-verify — both phases call the same per-file routine, so they must match by construction; this test confirms there's no drift in implementation.
15. **Bootstrap-window race (no half-written files):** during the bootstrap walk of a 5k-file fixture, in a tight loop write monotonically increasing content into one of the bootstrapped files from inside the container. On the Mac, sample the file repeatedly with `cat` and confirm every read returns a complete line (never a truncation). Validates the tempfile+rename invariant under concurrent bootstrap+drain.
16. **Virtiofs rename atomicity under contention.** Independent of bootstrap. With mirror running, two in-container writers tight-loop `printf '%d\n' $i > /repo/contended.txt` (different content streams) for 60s; on the Mac, a polling reader confirms every read returns a complete line (no truncation, no zero-length reads). This is what makes the tempfile-then-`os.Rename` pattern load-bearing — if it doesn't hold on virtiofs, the bootstrap-window claim collapses too.
17. **`--no-skiplist` flag.** `incus exec <c> -- mkdir -p /repo/dist /repo/node_modules`. Activate without the flag: confirm `/dist/...` IS mirrored (dropped from the skiplist in v2) and `/node_modules/...` is NOT. Re-activate with `--no-skiplist`: confirm both are mirrored. Verify the systemd drop-in `/etc/systemd/system/ahjo-mirror.service.d/flags.conf` is written/removed correctly across the two activations.
18. **Skiplist-presence warning.** `incus exec <c> -- mkdir -p /repo/node_modules && touch /repo/node_modules/x`. Run `ahjo mirror on …`; verify the success output lists `node_modules` under "these directories will not be mirrored" and points at `--no-skiplist`.
19. **Bare-metal Linux smoke (when CI infrastructure allows).** Run a representative subset of tests 1–6 on a Linux host running ahjo natively (no Lima). The disk device's `source=` is a host path; uid translation is `raw.idmap`-only; everything else identical. Catches any accidental Lima-only assumption that crept in.
20. **Negation parity, end-to-end (regression for the v3 spike).** With `.env*\n!.env.example\nnode_modules/\n` in `/repo/.gitignore`, write `.env`, `.env.local`, `.env.example` from inside the container; activate the mirror. On Mac: `.env.example` IS mirrored, the others are not. Then live-mutate `.env.example` and `.env`; only `.env.example`'s update lands on Mac. This is the F-env spike fixture run end-to-end against the real daemon — its purpose is to catch any regression that re-introduces a non-git-faithful filter (e.g. an accidental return to rsync's `--filter=:- .gitignore` for bootstrap).

Automated:
- `mirror_test.go` against a fixture container, gated on `INCUS=1`.
- `gitignore_parity_test.go`: pure-function tests pinning the spike's six fixtures (A–F) plus an ahjo-repo snapshot. For every path in every fixture, assert the daemon's matcher equals `git check-ignore` exactly. CI enforces 100% agreement; a future library swap or upstream regression fails the build.
- Pure-function unit tests for the static skiplist.

## Migration / rollout

1. Land the daemon binary + systemd unit + Feature install changes. CLI still ships the disabled stub.
2. Bump `ahjo-base` so fresh containers carry `ahjo-mirror`. Existing dev installs need `ahjo update` to pull the new image (memory: rolling-current toolchains, no runtime migration).
3. Land the CLI activation path. Mirror unblocks for any container running new `ahjo-base`. For older containers the `incus file push` self-heal kicks in transparently — no user action required.
4. CHANGELOG: "ahjo mirror restored — in-container inotify daemon. Per-file push via tempfile-rename; bootstrap and overflow recovery share the same per-file routine. Single git-faithful gitignore matcher (`go-git`) governs every decision; no rsync in the daemon. No delete tracking; reset via `mirror off`. Older containers self-heal on first activation."

No registry schema bump (state lives in incus + systemd).

## Discarded options

We considered eight other paths and rejected them. The first four were covered by `research/mirror-source-access.md`; the next three came up during the v1 → v2 rework; option H came out of the v2 → v3 spike. All are recorded here so future maintainers can see the tradeoffs without reconstructing them.

### A. sudo + storage-pool path

Run a daemon as root against `/var/lib/incus/storage-pools/<pool>/containers/<name>/rootfs/repo` (the abandoned Phase 2). Smallest possible diff. Variant evaluated during v2 rework: a one-time-installed VM-side root system unit, bring-up via `ahjo init` rather than per-mirror sudo. Less bad than the per-invocation sudo of the original Option A, but still couples to the storage-pool path that's "officially internal Incus territory." Rejected: more work to reverse later than to skip now.

### B. `incus file mount` + polling rsync

Mount `/repo` on the VM via FUSE-sshfs, polled rsync to Mac. Rejected because inotify on FUSE-sshfs doesn't see container-side writes (verified during research) and because the long-running FUSE mount adds `fusermount -u` cleanup hazards.

### C. Hybrid (sudo fsnotify on storage-pool path, rsync from FUSE mount)

Half-root, two source paths, two failure modes. Too clever to recommend.

### D. Mutagen / Syncthing / Unison

External two-way file-sync tools. Mutagen has `--mode=one-way-replica`; Unison has `-force` for one-way push. Both genuinely match the requirements. Rejected because they still need binary delivery into the container, add a session/config model that ahjo would have to wrap, and replace `internal/mirror/daemon.go` with carrying a larger external surface for the same job. The custom Go binary is small and ahjo-shaped; the trade isn't worth it for v2's narrow scope. If we ever want bidirectional sync, Mutagen is the first candidate.

### E. fsnotify + debounce + per-tick rsync (this doc's v1)

The original proposal. Caught events, debounced 200ms, then ran rsync to reconcile. Rejected because:
- rsync's job (compute the diff between two trees) is misaligned with what we need (push the file the event named). The event already names the file; tree-diffing is wasted.
- Carries supervisor complexity that solves problems v2 doesn't have: debounce timer, restart-rate-limit semantics, bootstrap-vs-watch race, per-dir gitignore filter merging during steady-state operation.
- The sub-second latency that motivated the debounce was Phase 0 inertia, not a stated requirement.

v2 kept rsync only where it appeared to earn its keep — one-shot bootstrap and overflow recovery — but the gitignore-parity spike (see option H below) found rsync's filter is not git-faithful. v3 drops rsync entirely.

### F. systemd `.timer` + per-tick rsync (no fsnotify at all)

Drop fsnotify entirely; use a 2s `[Timer]` firing rsync. Considered seriously during the v2 design pass — dramatically simpler than v1 (no daemon, no embed chain, no debounce). Rejected because the per-tick whole-tree stat-walk is wasted work when the action we actually need is "copy the file the event named." Polling makes sense when actions are expensive or events are rare; here events are common (developer activity touches files constantly) and per-event work is trivial. Notification + per-event copy matches the kernel's grain better.

### G. Embed the binary, skip systemd; spawn via `setsid nohup`

Have `ahjo mirror on` spawn the daemon detached and supervise from ahjo. Rejected because the container has systemd as PID 1 (ahjo-base inherits the Incus default); using it costs nothing — one unit file, three CLI calls — and gets crash-restart, journald, and standard `systemctl is-active` semantics for free.

### H. rsync for bootstrap + overflow only (this doc's v2)

v2's choice: live path Go, bootstrap and IN_Q_OVERFLOW recovery rsync. Looked clean — rsync exists for tree reconcile, the daemon doesn't have to ship one. Rejected after the gitignore-parity spike (see "Spike: gitignore parity" above) found:

- rsync's `--filter=:- .gitignore` does not honour `!` negation patterns. The canonical `.env*` + `!.env.example` config pattern silently mis-syncs: bootstrap excludes `.env.example`, the live daemon (using a faithful matcher) copies it on next mutation, and the Mac sees a stale state in between.
- Two gitignore implementations (rsync's filter + a Go library) means two ways to be wrong, with the only safety net being a parity test we'd have to maintain forever.
- The "rsync was designed for tree reconcile" framing was right about the operation but wrong about the rules. Once the rules diverge from git, rsync stops being the right tool — the rules are the contract, and the daemon owns them.

v3 replaces bootstrap with a daemon-internal `filepath.WalkDir` that calls the same per-file routine as the live path, governed by the same go-git matcher. Same shape, fewer moving parts, no parity risk.

## Open questions

1. ~~**`shift=true` on the disk device, or rely on the existing container `raw.idmap`?**~~ **Resolved 2026-05-10 by spike** — see "Spike: writable disk device under raw.idmap" above. Use raw.idmap; do not set `shift=true`. The COI Lima auto-detect workaround in memory `project_ahjo_coi_lima_idmap_workaround.md` is unaffected.
2. ~~**gitignore library choice.**~~ **Resolved 2026-05-10 by spike** — see "Spike: gitignore parity" above. Use `github.com/go-git/go-git/v5/plumbing/format/gitignore` for both the live event filter and the bootstrap walk. rsync exits the daemon entirely; both phases share one matcher, so bootstrap-vs-live ghost-files are eliminated by construction. The spike's six fixtures + a real-ahjo-repo snapshot are pinned into `internal/mirror/gitignore_parity_test.go` as regression coverage.
3. **`mirror off` clean-slate behavior.** The v3 bootstrap walk (no delete tracking, by design) preserves stale Mac-side files — a stale `/mirror/foo.txt` survives a branch switch that removed `foo.txt` from `/repo`. Options: (a) leave stale files, document the recovery (`rm -rf <target>/* && ahjo mirror on`); (b) `mirror off --clean` flag that wipes the target. **Lean: (a) for v1; add `--clean` only if user demand surfaces.**
4. ~~**Lima sysctl bump for inotify limits.**~~ **Resolved.** A new `ahjo init` step writes `/etc/sysctl.d/99-ahjo.conf` and runs `sudo sysctl --system`. Two values land in the same file:
   - `fs.inotify.max_user_watches=1048576` — covers worst-case watch counts on big monorepos.
   - `fs.inotify.max_queued_events=65536` — 4× the default (16384). Sized for the worst plausible offline window: a full-tree bootstrap walk (multi-second on 30k files) following a `.gitignore`-driven exit/restart, with a busy agent burst running concurrently. Larger values cost RAM (~256 bytes per event) for no real-world payoff at this topology.
   - On Mac, the step writes inside the Lima VM. On bare-metal Linux, the same code path writes to the host's `/etc/sysctl.d/99-ahjo.conf` directly — same step, same file, no platform branch.
5. **Embedded-binary version stamp format.** CLI carries a stamp via ldflags (same `-X main.version=$(VERSION)` the main binary already uses); on activation, the CLI compares with `ahjo-mirror version` output in the container; mismatch → stop unit, push, continue (see step 4 of CLI behavior). Stale image stays usable. Same self-heal pattern Claude Code uses with its `update` subcommand.

## Requirements summary

Host VM:
- Linux kernel ≥ 5.19 for VFS idmapped mounts on Btrfs (Lima ships 6.8 — fine).
- Incus ≥ 0.4 for `disk` device with idmap propagation through `raw.idmap` (Lima's incus is current).
- `fs.inotify.max_user_watches` ≥ `1048576` and `fs.inotify.max_queued_events` ≥ `65536` (new `ahjo init` step).

Inside `ahjo-base`:
- systemd as PID 1 (default for Incus + Ubuntu).
- `/usr/local/bin/ahjo-mirror` and `/etc/systemd/system/ahjo-mirror.service` from the `ahjo-runtime` Feature.

(`rsync` is no longer a daemon dependency. It remains installed via the upstream image for developer convenience but the mirror daemon does not invoke it.)

Host CLI changes only — no new external dependencies on the Mac side.

## What's deliberately omitted

- Bidirectional sync. Different problem, different doc. Mutagen would be the starting point if we ever take it on.
- Delete tracking on the Mac side. Stale files accumulate between branch switches; reset via `mirror off && rm -rf <target>/* && mirror on`. Eliminating delete tracking is the simplification that lets us drop two-tree reconciliation from the live path.
- Multi-target fan-out. State-shape change with no demand signal.
- Container-side inotify limit autotuning. Host-side `ahjo init` bump is sufficient.
- Cross-container `git fetch` fan-out. Different feature, tracked separately.
- Recovery-on-container-restart auto-reactivation as an opt-in. v2 always auto-resumes after container restart because resume is idempotent (re-bootstrap from current state). v1 deliberately didn't; v2's simpler resume semantics make it safe.
- Two-tree reconciliation in the live path. The live phase is per-event push; the bootstrap and overflow paths reuse the same per-file routine via a `filepath.WalkDir` over `/repo`. No rsync, no separate reconcile algorithm.
