# Changelog

## Unreleased — `--container-config` and built-in stacks

### Added

- **`--container-config=<value>` flag on `ahjo repo add` and `ahjo claude`**.
  Picks the container configuration for a repo at repo-add time. Resolution
  order: explicit flag wins, otherwise `.ahjo/ahjocontainer.json` in the
  repo, otherwise an interactive picker on a TTY (bare + any repo
  `.ahjo/*.json` variants + bundled stacks), otherwise bare. Value accepts:
  - A **bundled stack name**: `node`, `python`, `go`, `rust`. Each is a
    curated `ahjocontainer.json` shipped inside the ahjo binary (see
    [internal/stacks/](internal/stacks/)).
  - A **repo-local basename**, resolved against `.ahjo/<value>.json` in
    the repo — so a repo can ship multiple variants alongside the
    canonical `ahjocontainer.json`.
  - An **absolute or relative path** to a `.json` file on the host. On
    macOS the shim transparently stages paths outside the Lima VM's
    reverse-mount (e.g. `/tmp/foo.json`) through the shared dir, so any
    Mac path works — not just paths under `~/`.
  - The literal `bare` for no toolchain.
- Nothing is written to the repo; the choice is applied to the repo base
  container only and persists until `ahjo repo rm`.
- The `node` stack honors `.nvmrc` if the repo carries one (via nvm in
  `postCreateCommand`, on top of the LTS the upstream Feature installed).

### Changed

- **Node + corepack removed from `ahjo-runtime`.** The base image no longer
  ships Node — Claude Code's native installer bundles its own runtime, and
  nothing else in `ahjo-runtime` used node. Repos that need a Node toolchain
  either declare `ghcr.io/devcontainers/features/node` in their own
  `.ahjo/ahjocontainer.json` or pass `--stack=node`. Behavior change for
  repos that implicitly relied on `node`/`npm`/`pnpm` being present without
  declaring it; this aligns with how every other language already worked.

### Migration

Run `ahjo update` to rebuild `ahjo-base` without Node. If a repo's
warm-install (`pnpm install`, `npm ci`, etc.) starts failing after the
update, either add `ghcr.io/devcontainers/features/node:1` to its
`.ahjo/ahjocontainer.json` or re-add the repo with `--stack=node`
(`ahjo repo rm <alias> && ahjo repo add <url> --stack=node`).

## Unreleased — embedded Feature reshuffle

### Changed

- **`rtk` relocated** from `ahjo-runtime` to `ahjo-default-dev-tools`. ahjo's
  Go code never invokes rtk — it's a Claude-side ergonomic, same category as
  `ripgrep`/`eza`. The reshuffle aligns with the criterion that `ahjo-runtime`
  contains only what ahjo's own runtime depends on.
- **Embedded Feature apply order flipped**: `ahjo-runtime` now applies before
  `ahjo-default-dev-tools` (was the other way around). `rtk init -g --auto-patch`
  in the dev-tools Feature needs the `claude` binary and `~/.claude/` tree that
  `ahjo-runtime` installs.
- **Apt duplicates removed**. `ahjo-runtime` no longer apt-installs `jq`, `curl`,
  `ca-certificates`, `gnupg` (all in `common-utils:2`); `ahjo-default-dev-tools`
  no longer apt-installs `unzip`, `ca-certificates`, `curl` (same reason).
  Apt's idempotent so this was always cosmetic, but the duplicates implied
  these Features were standalone-applicable when in practice the base-bake
  chain hardcodes their position after `common-utils:2`.

### Migration

Run `ahjo update` to rebuild `ahjo-base` with the new layering. Existing
branch containers continue to work on the old image until you recreate them
with `ahjo shell <alias> --update`.

## Unreleased — `.ahjo/ahjocontainer.json`

### Changed

- **Per-repo config path moved to `.ahjo/ahjocontainer.json`** (was
  `.devcontainer/devcontainer.json` / `.devcontainer.json`). Schema is
  identical — same honored/rejected fields, same `customizations.ahjo`
  block, same lifecycle semantics. Reason: sharing the spec path with VS
  Code Dev Containers / Codespaces / JetBrains Gateway meant those
  toolchains saw an ahjo-managed repo and tried to launch their own
  Docker-based flow against it. ahjo now owns its own path.
- `ahjo repo add` aborts with a migration error when it finds a legacy
  `.devcontainer/devcontainer.json` (or `.devcontainer.json`) in the repo,
  mirroring the existing `.ahjoconfig` posture. No runtime migration; move
  the file by hand. README documents the swap.
- Internal: per-repo config parsing moves from `internal/devcontainer/` to
  a new `internal/ahjocontainer/` package. Feature / OCI / trust /
  resolver code stays under `internal/devcontainer/` — those operate on
  the upstream Features ecosystem (OCI-addressed, spec-fixed
  `devcontainer-feature.json` filename) and remain devcontainer-shaped.

## Unreleased — per-repo GH_TOKEN forwarding

### Added

- `ahjo env {set,get,unset,list}` — generic KV access to `~/.ahjo/.env`.
  `set KEY` (no value) prompts with hidden input via `term.ReadPassword`
  so secrets never enter shell history; piping `echo $VAL | ahjo env set
  KEY` still works (with a stderr note about the non-TTY read).
- `ahjo repo set-token <alias>` — set/rotate a per-repo GitHub PAT stored
  at `~/.ahjo/repo-env/<slug>.env` (mode 0600). The PAT is forwarded into
  every container for the repo as `$GH_TOKEN`, with the per-repo file
  taking precedence over the global `~/.ahjo/.env`.
- `ahjo repo add` now prompts for a fine-grained GitHub PAT after clone,
  before container shutdown. Skipped on `--yes`, non-TTY stdin, or empty
  paste; an existing PAT for the slug is detected and the prompt is
  silently skipped on re-runs. Permissive validation (warns + accepts on
  non-canonical prefixes so enterprise hosts work).
- `GH_TOKEN` added to the default `forward_env`. Existing `~/.ahjo/
  config.toml` users get it via a union-with-defaults migration on
  `Load()` — no manual edits required.
- `ahjo doctor` gains a `checkAnyGHToken()` warn-level check surveying
  per-repo PATs and the global fallback; lists slugs missing a PAT. Two
  follow-up checks per registered repo with a per-repo PAT: whether
  `environment.GH_TOKEN` is actually set on the default-branch container
  (probed via `incus config get`) and whether
  `credential.https://github.com.helper` is configured in the in-container
  `/home/ubuntu/.gitconfig` (probed via `incus exec git config --global
  --get`). Both surface a warn with a one-shot fix when missing.
- Raw `git clone/fetch/push/pull` over HTTPS inside containers now
  authenticates via `gh auth setup-git`'s credential helper. `ahjo repo
  add` runs it once on the default-branch container after the PAT prompt;
  the resulting `credential.https://github.com.helper = !gh auth
  git-credential` line in `/home/ubuntu/.gitconfig` rides into every COW
  branch via `incus copy` (same property `seedGitIdentity` already relies
  on). SSH remotes are unaffected — the helper is a no-op for them, and
  ahjo never auto-rewrites SSH ↔ HTTPS.
- `GH_TOKEN` (and `GITHUB_TOKEN` for legacy tooling) is now promoted from
  attach-time-only env to container-level `environment.*` config keys, so
  every `incus exec` against a repo's container picks the PAT up — not
  just the helpers that built env maps via `branchEnv`. `ahjo repo
  set-token` re-applies these to the default container plus every branch
  container; already-running shells need a restart to see the new value,
  but new `incus exec` invocations get it immediately. The success
  message calls this out.

### Internal

- `internal/tokenstore` promoted from "Claude OAuth token writer" to a
  generic KV store. New: `Set/Get/Unset/List` (operates on `~/.ahjo/
  .env`) plus `SetAt/GetAt/UnsetAt/ListAt/LoadInto` taking an explicit
  path so per-repo `.env` files share the same machinery. `SetToken` and
  `TokenEnv` retained as the Claude shim.
- `internal/paths`: `RepoEnvDir()` and `SlugEnvPath(slug)` added;
  `EnsureSkeleton` creates `~/.ahjo/repo-env/`.
- `branchEnv` (shell/claude attach path) now layers `~/.ahjo/repo-env/<
  slug>.env` over the process env, with the slug resolved from the
  container name via the registry.
- `ahjo repo rm <alias>` removes the per-repo `.env` file as part of
  cleanup.

## Unreleased — adopt-devcontainer-spec Phase 3: drop COI residue

### Changed

- `ahjo --help` short description now reads "via Incus" (no longer "via
  coi/Incus") — COI is no longer involved in any code path.
- `ahjo init`'s onboarding-marker step now attributes the host →
  container `~/.claude.json` push to ahjo's own `pushClaudeConfig`
  instead of COI.
- `internal/mirror`: dropped `.coi` from the rsync skip list and the
  inotify watch skiplist — nothing creates a `.coi/` directory anymore.

### Removed

- `incus.FindMountDevice` — dead since worktrees were retired (no
  callers).

### Internal

- Stale COI-vs-ahjo comments swept across `internal/cli/repo.go`,
  `internal/cli/shell.go`, `internal/devcontainer/build.go`,
  `internal/tokenstore/tokenstore.go`, `internal/idmap/idmap.go`,
  `internal/lima/lima.go`, `internal/incus/incus.go`,
  `internal/ahjoruntime/embed.go`, `internal/ahjoruntime/feature/install.sh`.
- README, Makefile, RELEASING.md, guidelines/lima-ssh-master.md updated
  to drop or correct stale COI references. The `ahjo nuke` cleanup of
  any leftover `coi-default` image is intentionally retained for users
  upgrading from a pre-Phase-1 install.

## Unreleased — adopt-devcontainer-spec Phase 1: build pipeline rewrite

### Breaking

- **COI removed from the build pipeline.** `ahjo init` and `ahjo update` no
  longer install COI or call `coi build`. Image baking moves to a
  devcontainer-Features pipeline: pull `images:ubuntu/24.04` once into the
  local image store as alias `ahjo-osbase`, launch a transient container,
  apply the embedded `ahjo-runtime` Feature (sshd-as-a-service, the
  ahjo-claude-prepare hook, Node + corepack), publish the result as
  `ahjo-base`, delete the transient. Existing dev installs must `ahjo
  nuke -y && ahjo init` — there is no migration code.
- **In-image user `code` → `ubuntu`.** `ahjo-base` now uses the upstream
  Ubuntu cloud-image's canonical `ubuntu` user (UID 1000) instead of
  ahjo's bespoke `code` (also UID 1000). Bind-mount paths move from
  `/home/code/...` to `/home/ubuntu/...`; `ssh-config` writes
  `User ubuntu`; `raw.idmap` continues to target UID 1000:1000. No
  per-Feature edits — the Feature contract reads `_REMOTE_USER` from env.

### Changed

- **Image stack collapses two layers.** `coi-default → ahjo-base` becomes
  `images:ubuntu/24.04 (alias ahjo-osbase) → ahjo-base`. The
  upstream-mirror layer pulls once per ahjo version bump; only `ahjo-base`
  rebuilds on `ahjo update`.
- **`ahjo doctor` no longer checks for `coi` or the `coi-default` image.**
  COI is no longer a runtime dependency.
- **`ahjo nuke` now also clears the `ahjo-osbase` image** (and still
  clears any leftover `coi-default` from a pre-Phase-1 install).

### Added

- `internal/ahjoruntime/` — embeds the `ahjo-runtime` devcontainer Feature
  (`devcontainer-feature.json` + `install.sh`) plus a `Materialize(dir)`
  helper that writes the Feature into a host tempdir for the runner to
  push into the build container.
- `internal/devcontainer/` — Phase 1 of the devcontainer Feature runner.
  `Apply(container, feature, env, out)` validates the Feature metadata
  (rejecting Docker-flavored fields), pushes the dir into the container,
  and runs `install.sh` as root with `_REMOTE_USER` / `_REMOTE_USER_HOME`
  / `_CONTAINER_USER` / `_CONTAINER_USER_HOME` set. `BuildAhjoBase(out,
  force)` orchestrates the full image-bake recipe.
- `incus.Launch`, `incus.ImageCopyRemote`, `incus.FilePushRecursive`,
  `incus.Publish` wrappers.

### Removed

- `internal/coi/` package (assets + binary wrapper + Go embed) —
  deleted outright; nothing in ahjo's runtime touches `coi` anymore.
  The `ahjo init` step that installed `coi` and the `coi-default` image
  also goes.
- `internal/profile/` package — no longer needed; the embedded Feature
  is materialized by `internal/ahjoruntime/embed.go` directly into a
  tempdir per build.
- `internal/initflow/assets/coi-config.toml` and the
  `initflow.CoiOpenNetworkConfig` helper — only the COI install step
  consumed it.
- `paths.CoiProfilesSubdir`, `paths.CoiProfilesDir`, `paths.CoiProfilePath`,
  `paths.ProfilesDir`, `paths.ProfilePath` — there is no on-disk profile
  layout anymore; the Feature lives entirely embedded in the binary.
- `--build-coi` flag on `ahjo init` / `ahjo update`.

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
