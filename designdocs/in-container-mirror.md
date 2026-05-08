# In-container mirror daemon

**Status:** draft · **Scope:** `ahjo mirror` only · **Depends on:** designdocs/no-more-worktrees.md (Phase 1 complete)

## Goal

Replace the VM-side `fsnotify`+`rsync` mirror (or, after Doc A Phase 2, the storage-pool-path-watching version) with an in-container daemon. The daemon watches `/repo/` and writes through a Mac-backed bind mount to the user's chosen Mac directory. The mirror lives entirely in supported Incus surface area: a `disk` device sourced from a Mac path (via Lima virtiofs) plus a long-lived in-container daemon process. No reaching into Incus internals.

## Non-goals

- Cross-FS hardlinks for mirrored content. Mirror is intentionally lossy: it copies, doesn't share storage. (The pnpm-store hardlink win lives inside the container already, not in the mirror.)
- Bidirectional sync. Mirror is one-way: container → Mac. Mac-side edits to the mirror dir are ignored (rsync overwrites them on next sync).
- Multiple simultaneous mirror targets per branch. One mirror per branch container, like today.

## Why now

- Doc A Phase 2 lands mirror on `/var/lib/incus/storage-pools/...` which Incus considers "internal." Works, but unsupported.
- The reverse-direction model (Mac path → bind-mount → in-container daemon → rsync) uses only first-class Incus primitives: `disk` devices and `incus exec`. Survives Incus version upgrades cleanly.
- The Mac→VM→container mount chain is already proven (Lima virtiofs surfaces `/Users/...`; Incus disk devices accept VM paths as source). No new infrastructure.

## Topology

```
Mac:       ~/mirrors/foo/bar/feat/
            ↓ Lima virtiofs (Mac /Users → VM /Users; preserves paths)
VM:        /Users/lasse/mirrors/foo/bar/feat/
            ↓ Incus disk device, source=<vm-path>, path=/mirror, readonly=false
Container: /mirror/
            ↑ rsync writes here, driven by inotifywait on /repo/
```

## Phase 1 — mirror runs inside the container

### What ahjo does

1. **`ahjo mirror on foo-bar@feat --target ~/mirrors/foo/bar/feat`**:
   1. Validate target is under Lima virtiofs root (existing check at `internal/cli/mirror.go:275`).
   2. Resolve VM path: same string as Mac path because Lima virtiofs preserves `/Users/...`.
   3. Ensure container is running.
   4. `incus config device add <container> mirror disk source=<vm-path> path=/mirror readonly=false`.
   5. Bootstrap once: `incus exec ... rsync -a --delete-during --filter=':- .gitignore' --exclude=.git --exclude=.coi /repo/ /mirror/`.
   6. Spawn the in-container daemon: `incus exec --background ... ahjo-mirror /repo /mirror /mirror/.ahjo-mirror.log`.
   7. Persist mirror state: `{branch, target, container, daemon-pid}`.

2. **`ahjo mirror off foo-bar@feat`**:
   1. Lookup mirror state.
   2. Signal the daemon: `incus exec ... kill -TERM <daemon-pid>`.
   3. Wait for shutdown (or timeout-and-kill).
   4. `incus config device remove <container> mirror`.
   5. Clear mirror state.

3. **`ahjo mirror status`** — read state file + verify daemon is running (`incus exec ... kill -0 <pid>`).

### Daemon implementation

Two options:

- **Option α — small Go binary baked into `ahjo-base`.** Single static binary at `/usr/local/bin/ahjo-mirror`. Same logic as `internal/mirror/daemon.go` (fsnotify + debounced rsync), built for Linux and shipped in the image. Pros: typed, tested, identical logic to today; no shell quoting hazards. Cons: ahjo-base image rebuild required to update; coupling between ahjo CLI version and image.
- **Option β — shell wrapper around `inotifywait` + `rsync`.** Tiny shell script, pushed via `incus file push` at activation time (so it travels with the ahjo CLI version). Pros: trivially updatable, no image dependency. Cons: shell quoting, debounce in pure shell is ugly.

**Lean: Option α** — fsnotify daemon is small, well-tested in the existing codebase, and the ahjo-base build pipeline is already part of the project. Drift between CLI and image is real but manageable: ship the image alongside ahjo releases and let `ahjo doctor` flag stale image versions. Option β stays open as a fallback if image-version coupling becomes annoying in practice.

### Code changes

| File | Change |
|---|---|
| `cmd/ahjo-mirror/main.go` (new) | Small binary wrapping the existing fsnotify+rsync logic; src/dst as args; logs to a path passed in |
| `internal/mirror/daemon.go` | Refactor `RunDaemon` so it can be reused in the new binary (likely already reusable as-is) |
| `internal/cli/mirror.go` | Activate: ensure container running → add disk device → spawn daemon via `incus exec --background`. Deactivate: signal + remove device |
| `internal/incus/incus.go` | Add `AddMirrorDevice(container, source, path)`, `RemoveMirrorDevice(container)` |
| `internal/mirror/state.go` | Persist container name + daemon pid + Mac target |
| ahjo-base build | Include `ahjo-mirror` binary in the image |

### Lifecycle hazards

1. **Daemon dies with the container.** `ahjo mirror on` returns once the daemon is spawned; daemon runs until `ahjo mirror off` or container stops. If the container stops while mirror is on, the daemon dies and mirror state is stale. `ahjo mirror status` should detect this and offer to restart on next container start.
2. **`incus exec --background` semantics.** `incus exec` doesn't natively detach. Use `nohup` + `setsid` inside the exec, or wrap the daemon in a tiny forking launcher. Validate during implementation.
3. **Daemon log location.** Logging into `/mirror/.ahjo-mirror.log` puts logs on Mac (visible to user) but the rsync filter must exclude that filename to avoid mirror-syncs-its-own-log loops. Alternative: log to `/var/log/ahjo-mirror.log` inside container, expose via `ahjo mirror logs`.
4. **Idmap stack.** Lima virtiofs preserves uids (Mac 501 ↔ VM 501); `raw.idmap` translates VM 501 ↔ container 1000. Files written by daemon as container uid 1000 land on Mac as uid 501. Verify during implementation.
5. **First-time startup ordering.** Bootstrap rsync runs synchronously before daemon spawn; daemon then installs inotify watches. Brief race window between daemon-start and watches-installed. Acceptable — debounced rsync catches up on the next tick.

### Migration from Doc A Phase 2

Doc A Phase 2 watches `/var/lib/incus/storage-pools/.../rootfs/repo/` from the VM. Doc B Phase 1 replaces that with the in-container daemon. The `internal/mirror/state.go` schema gains a `daemon-pid` field. `mirror.go` activation code is rewritten — clean swap, not a delicate migration.

### Open questions

1. **Multiple-target mirrors.** A user might want one branch mirrored to two Mac dirs simultaneously. Today: single target. Defer to user demand.
2. **Filter rules.** Today: gitignore + `.git` + `.coi` excluded. Future: add `node_modules`, build dirs, the daemon's own log file. Same set should be consistent across CLI and daemon.
3. **Stale-image detection.** If ahjo CLI is upgraded but `ahjo-base` is stale, `ahjo-mirror` binary may be missing. Detect at activation time and either (a) push the binary via `incus file push` and run it from `/tmp`, or (b) error out with "rebuild ahjo-base." Lean (a) — graceful self-heal.

## What's deliberately omitted

- Two-way sync. Out of scope; would need conflict resolution.
- Per-file event piping (event-driven copy without rsync). Big architecture change; not needed at current performance targets.
- Container-side performance tuning of inotify watch limits. Existing skiplist (`internal/mirror/daemon.go:23`) handles this; carry it into the in-container daemon as-is.
