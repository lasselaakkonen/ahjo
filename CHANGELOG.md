# Changelog

## Unreleased — Phase 1: drop worktrees, container holds the repo

### Breaking

- **Storage driver flip: `dir` → `btrfs`.** The Incus storage pool now uses
  btrfs so `incus copy` of a repo's default container is a near-free
  reflink that inherits `node_modules` and the pnpm store on the same
  vfsmount (eliminating the EXDEV error that pnpm hit across the old
  vfsmount boundary). Existing dev installs must `sudo rm -rf
  /var/lib/incus/*` and re-run `ahjo init` — there is no migration code.
- **Registry version bumped to 2.** `Worktree` → `Branch`; TOML key
  `[[worktrees]]` → `[[branches]]`; fields `worktree_path`,
  `bare_path`, `ssh_host_keys_dir` removed; `is_default` added. Loading
  a v1 `~/.ahjo/registry.toml` returns an upgrade error — clear the file
  and `ahjo repo add` again.
- **`ahjo mirror` is temporarily disabled.** The old VM-resident worktree
  path is gone; Phase 2 restores mirror via storage-pool-internal paths.
  `ahjo mirror status` and `ahjo mirror off` still work for cleaning up
  a daemon left over from a prior build.
- **No more host-side bare repos.** `~/.ahjo/repos/` and
  `~/.ahjo/worktrees/` are no longer created or used. Every git
  operation runs inside the branch container against `/repo`.
- **`ahjo rm` requires `--force-default` to remove a repo's
  default-branch container.** The default container is the COW source
  for every other branch in the repo; removing it without
  `--force-default` is refused.
- **COI bumped from v0.8.0 → v0.8.1.** `ahjo update` rebuilds
  `ahjo-base` from the new toolchain. v0.8.1 drops the in-container
  `python3` dependency (sandbox JSON merge moved to Go), fixes a tmux
  env-var leak, and writes `/etc/claude-code/managed-settings.json` to
  suppress claude's new auto-mode prompt.

### Changed

- **Branch creation collapses to `incus copy` + `git checkout -b`.**
  `ahjo new` no longer materializes a host-side worktree and rebases
  mounts; it COW-clones the repo's default container, reapplies
  `raw.idmap`, rewires the per-branch SSH host-keys mounts, and runs
  `git checkout -B <branch>` inside.
- **`ahjo repo add` warm-installs dependencies inside the container.**
  Detects `pnpm-lock.yaml` / `package-lock.json` / `bun.lockb` /
  `uv.lock` / `Cargo.lock` and runs the matching installer in `/repo`
  before stopping the container so subsequent `incus copy` clones
  inherit the warm dep cache.
- **COI dropped from the runtime container path.** Every container is
  created via `incus init ahjo-base <name>`, attached via `incus exec
  --force-interactive`, and managed via direct `incus` calls.
  `coi build` (image pipeline) is the only retained COI dependency.
- **Container security flags baked into the `ahjo-base` image.** After
  `coi build`, `ahjo init`/`update` runs `incus image edit` to merge
  `security.nesting`, `security.syscalls.intercept.{mknod,setxattr}`,
  `linux.sysctl.net.ipv4.ip_unprivileged_port_start=0`, and
  `security.guestapi=false` into the image config so `incus launch
  ahjo-base <name>` inherits them automatically.
- **`.ahjoconfig` is read from inside the container.** `ahjoconfig.Load`
  is replaced by `ahjoconfig.LoadFromContainer(name)` for every runtime
  caller (`ahjo new`, `ahjo repo add`, `ahjo expose`).
- **`forward_env` is propagated via `incus exec --env`.** Replaces the
  COI `[defaults] forward_env` mechanism. Only keys actually set on the
  host are forwarded into the container.
- **`ahjo top` details pane shows `container` + `path: /repo`** instead
  of the old VM-side worktree path.
- **`ahjo gc` skips default-branch containers** so it can never
  garbage-collect the COW source.

### Removed

- `git.AddWorktree`, `git.RemoveWorktree`, `git.CloneBare`, `git.Fetch`,
  `git.DefaultBranch`, `git.RefExists`, `git.ListWorktrees`.
- `coi.Setup`, `coi.RenderConfig`, `coi.ResolveContainer`,
  `coi.ContainerStart/Stop/Delete`, `coi.ContainerExec[As]`,
  `coi.Shutdown`, `coi.ExecShell`, `coi.ExecClaude`,
  `coi.TemplateData`. `coi-config-template.toml` deleted.
- `paths.RepoBarePath`, `paths.WorktreePath`, `paths.WorktreesDir`,
  `paths.ReposDir`. Added `paths.RepoMountPath = "/repo"` constant.
- `incus.UpdateWorktreeMounts`. Replaced by the per-branch device
  re-source step inside `cloneFromBase`.

### Added

- `incus.LaunchStopped`, `incus.Start`, `incus.WaitReady`,
  `incus.ExecAttach`, `incus.ExecAs`, `incus.FilePush`,
  `incus.SetImageDefaults`.
- `ahjoconfig.LoadFromContainer(name string)` — reads
  `/repo/.ahjoconfig` via `incus exec`.
