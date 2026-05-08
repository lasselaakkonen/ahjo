# Container as repo (no more worktrees)

**Status:** draft · **Scope:** ahjo lifecycle: `repo add`, `new`, `shell`, `rm` · plus storage pool driver

## Goal

Drop the `git worktree` primitive from ahjo. Each container holds a full clone of the repo at `/repo`. Branch containers are btrfs CoW clones of a "default container" with `git checkout -b` run inside. With btrfs reflinks, this is cheaper on disk and faster to create than today's bare+worktree+bind-mount setup, and it eliminates the layer that fights pnpm's hardlink semantics.

## Non-goals

- Phase 1 does not change mirroring. Mirror temporarily breaks at end of Phase 1; Phase 2 patches it.
- No central refresh/fetch story. Each container fetches independently for now. A future doc adds `ahjo fetch <repo>` fan-out.
- No protection scheme for the default container in Phase 1. Address as a follow-up if accidental deletion becomes a real failure mode.
- No migration tooling. ahjo is in dev mode; existing setups nuke `/var/lib/incus/` and re-init.

## Why now

- pnpm hardlinks fail across vfsmount boundaries (`EXDEV`). The only kernel-respecting fix is putting `/repo` and `~/.local/share/pnpm/store/` on the same vfsmount, i.e. inside the container.
- Once the worktree moves inside the container, btrfs CoW makes the worktree primitive itself redundant: `incus copy` produces a near-zero-cost clone with all of base's state (objects + node_modules + .pnpm-store) reflinked. Branch creation collapses to `git checkout -b` inside that clone.
- The codebase already expects btrfs (`internal/preflight/preflight.go:102` warns if driver is anything else) but bootstraps `dir` (`internal/initflow/assets/incus-preseed.yaml:12`). This phase resolves that inconsistency.

## Topology change

Before:

```
~/.ahjo/repos/foo-bar.git/         bare repo (VM ext4)
~/.ahjo/worktrees/foo-bar/main/    default-branch worktree (VM ext4) ──bind──► container /workspace
~/.ahjo/worktrees/foo-bar/feat/    feature-branch worktree (VM ext4) ──bind──► COW'd container /workspace
```

After:

```
ahjo-foo-bar (default container, btrfs)
  /repo/                              full clone of github.com/foo/bar, default branch
  /repo/node_modules/                 warmed during repo add
  ~/.local/share/pnpm/store/          warmed during repo add

ahjo-foo-bar-feat (branch container, btrfs CoW from ahjo-foo-bar)
  /repo/                              CoW reflink of default's /repo, switched to feat via `git checkout -b`
  /repo/node_modules/                 inherited from default at copy time
  ~/.local/share/pnpm/store/          inherited from default at copy time (same vfsmount → hardlinks work)
```

## Phase 1 — drop worktrees, container holds the repo

### What ahjo does

1. **`ahjo init`** — storage pool driver becomes `btrfs`. Existing setups nuke `/var/lib/incus/` and re-init (dev mode; no migration code).

2. **`ahjo repo add foo/bar`**:
   1. Allocate slug, SSH port, container name (`ahjo-foo-bar`).
   2. `incus launch ahjo-base ahjo-foo-bar`.
   3. Apply `raw.idmap`, mount SSH host keys.
   4. `incus exec ahjo-foo-bar -- git clone https://github.com/foo/bar.git /repo`.
   5. Run warm install: detect lockfiles in `/repo`, run the matching installer.
      - `pnpm-lock.yaml` → `pnpm install`
      - `package-lock.json` → `npm ci`
      - `bun.lockb` → `bun install`
      - `uv.lock` → `uv sync`
      - `Cargo.lock` → `cargo fetch`
      - Multi-tool repos run all detected installers.
   6. `incus stop ahjo-foo-bar` so subsequent `incus copy` is fast (stopped containers CoW cleanly).
   7. Register a `Branch` entry: `Repo=foo-bar, Branch=<default>, IncusName=ahjo-foo-bar, IsDefault=true`.

3. **`ahjo new foo/bar feature-x`**:
   1. Validate: `feature-x` doesn't collide with another branch container.
   2. Allocate slug, SSH port, container name (`ahjo-foo-bar-feature-x`).
   3. `incus copy ahjo-foo-bar ahjo-foo-bar-feature-x` (btrfs reflink, near-free).
   4. Apply `raw.idmap`, update SSH key mount sources.
   5. `incus start ahjo-foo-bar-feature-x`.
   6. `incus exec ... git checkout -b feature-x` (or `git checkout feature-x` if remote branch already exists).
   7. Register `Branch` entry.

4. **`ahjo shell foo-bar@feature-x`** — same as today minus the `coi.Setup` workspace-mount step. SSH proxy device wiring stays the same. Drop into `/repo`.

5. **`ahjo rm foo-bar@feature-x`**:
   1. `incus stop` + `incus delete`.
   2. Free SSH port.
   3. Remove `Branch` entry.
   4. (No `os.RemoveAll(WorktreePath)` — there is no VM-side path.)
   5. If the branch is the repo's default (`IsDefault=true`), refuse without `--force-default`.

### What disappears

| Removed | Location |
|---|---|
| Bare clone in `runRepoAdd` | `internal/cli/repo.go` |
| `git.AddWorktree` call | `internal/cli/new.go:112` |
| `git.RemoveWorktree` call | `internal/cli/rm.go:58` |
| `os.RemoveAll(WorktreePath)` | `internal/cli/rm.go:63` |
| `coi.RenderConfig` writing into worktree dir | `internal/cli/new.go:150-156` |
| `setupCOWContainer` mount-rebasing block | `internal/cli/new.go:319-329` |
| `incus.AddDiskDevice(..., "ahjo-bare", ...)` | `internal/cli/shell.go:148` |
| `incus.UpdateWorktreeMounts` | `internal/incus/incus.go:347` |
| `paths.WorktreePath`, `WorktreesDir`, `RepoBarePath`, `ReposDir` | `internal/paths/paths.go:67-75` |
| `git.AddWorktree`, `git.RemoveWorktree` | `internal/git/git.go:53,76` |

### What changes shape

| File | Change |
|---|---|
| `internal/initflow/assets/incus-preseed.yaml:12` | `dir` → `btrfs` |
| `internal/registry/registry.go:44` | `Worktree` struct → `Branch`; drop `WorktreePath`, `BarePath`; add `IsDefault bool` |
| `internal/cli/repo.go` | `runRepoAdd` rewritten: launch container + clone + warm install |
| `internal/cli/new.go` | `runNew` rewritten: COW copy + start + exec checkout |
| `internal/cli/rm.go` | Remove worktree path cleanup; add default-container guard |
| `internal/cli/shell.go` | Drop bare bind-mount; drop COI workspace setup; `cd /repo` |
| `internal/coi/template.go` | Stop using COI for repo-default and branch containers (workspace-mount feature unused). Retain COI only for the `ahjo-base` image build pipeline. |
| `internal/ahjoconfig/config.go` | `Load(containerName)` reads `.ahjoconfig` via `incus exec ... cat /repo/.ahjoconfig` |
| `internal/cli/autoexpose.go` | Same: read `.ahjoconfig` via container exec |
| `internal/tui/top/details.go:97` | Show container name + `/repo` instead of worktree path |
| `internal/paths/paths.go` | Remove worktree/bare helpers; add `RepoMountPath = "/repo"` constant |
| `internal/git/git.go` | Replace `AddWorktree`/`RemoveWorktree` with a small `CheckoutBranchInContainer(name, branch)` helper |
| `internal/incus/incus.go` | Add `ExecInContainer(name, cmd...)` if not present; `LaunchContainer(image, name)` for repo add |

### Lifecycle hazards

1. **Default container is now load-bearing.** Deleting `ahjo-foo-bar` deletes the repo. `ahjo rm` refuses without `--force-default`. Future doc: snapshot or detect+reclone.
2. **`ahjo init` re-run requirement.** Existing dev installs nuke `/var/lib/incus/` to switch storage driver. CHANGELOG calls this out.
3. **Stopped-container assumption for COW.** `ahjo new` requires the source default container to be stopped before `incus copy`. ahjo stops it as part of repo add; if the user had it running, `ahjo new` silently stops with a one-line message.
4. **Mirror temporarily broken.** End of Phase 1: mirror watches a path that no longer exists. Phase 2 fixes it. CHANGELOG explicit.

### Open questions

1. **Default branch detection.** Today: bare repo's `HEAD`. Tomorrow: `incus exec ... git symbolic-ref HEAD`. Same mechanism, different transport.
2. **Keep `.coi/config.toml` rendering?** Lean: drop for repo-default and branch containers (we don't use COI's runtime workspace-mount feature anymore). Confirm `.coi/` isn't needed elsewhere before removing.
3. **`.ahjoconfig` location.** `/repo/.ahjoconfig` inside container, read via `incus exec`. Cleaner than VM-side maintenance; one extra exec round-trip per command.

## Phase 2 — restore mirroring against the new layout

### What's broken at end of Phase 1

Mirror's source path was `~/.ahjo/worktrees/<repo>/<branch>/`. That path doesn't exist anymore.

### Phase 2 fix (interim)

Point mirror at the Incus storage-pool internal path:

```
/var/lib/incus/storage-pools/<pool>/containers/<container-name>/rootfs/repo/
```

This is a real directory on the VM filesystem (btrfs) while the container is running. `fsnotify` + `rsync` work unchanged. Lima's virtiofs surfaces the same path on Mac.

Officially "internal" Incus territory, but stable in practice. Acceptable as an interim. Doc B (`in-container-mirror.md`) replaces it with the supported architecture.

### What ahjo does

1. **`ahjo mirror on <branch> --target <mac-path>`**:
   1. Resolve container name from registry.
   2. Compute storage-pool path: `/var/lib/incus/storage-pools/<pool>/containers/<container>/rootfs/repo`.
   3. Ensure container is running (`incus start` if needed).
   4. Bootstrap rsync from storage-pool path → `<mac-path>`.
   5. Start the existing daemon watching the storage-pool path.

2. **`ahjo mirror off <branch>`** — same as today (SIGTERM the daemon).

### Code changes

| File | Change |
|---|---|
| `internal/incus/incus.go` | Add `StoragePoolPath()` (returns `/var/lib/incus/storage-pools/<pool>/`) and `ContainerRootfsPath(containerName)` |
| `internal/cli/mirror.go:100` | Resolve source from container name via storage-pool path; ensure container running before bootstrap |
| `internal/mirror/state.go:33` | Persist container name (replacing or alongside the now-defunct `WorktreePath`) |
| `internal/mirror/daemon.go` | No code change; receives a different src path |

### Lifecycle hazards

1. **Storage-pool path is "internal" Incus territory.** Functional and stable; officially "don't poke around in here." Acceptable as an interim. Doc B replaces it.
2. **Container must be running** for the path to be mounted. Mirror activation now starts the container if stopped.
3. **idmapped mounts on btrfs.** Files appear as host uid 501 on VM and Mac (via Lima virtiofs). Same idmap semantics as today.
4. **Multi-pool setups.** Path layout assumes the container's pool is queryable. If user has multiple pools, query the actual pool for the specific container.

### Open questions

1. **Ship Phase 2 with Phase 1, or as separate immediate follow-up?** Lean: same release. Phase 1 alone leaves mirror broken.
2. **Persist storage-pool path or recompute?** Recompute. Pool layout can shift; query is cheap.

## What's deliberately omitted

- Doc B's in-container mirror architecture. Phase 2 is the interim using storage-pool internal paths; Doc B is the supported-primitive replacement.
- Default-container protection / snapshot / restore.
- Cross-container `git fetch` UX.
- Migration of any existing user data (dev mode; nuke and re-init).
