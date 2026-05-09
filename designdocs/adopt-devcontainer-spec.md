# Adopt devcontainer.dev spec; drop COI

**Status:** draft · **Scope:** ahjo image-build pipeline; `repo add`, `new`, `shell` lifecycle; per-repo config schema; COI dependency removal

## Goal

Retire ahjo's bespoke schemas (`.ahjoconfig` TOML, COI profile bash, the per-repo container-bake convention) in favor of the open devcontainer.dev specification. Adopt the spec as a *schema*, not a runtime — Incus + Lima stay. Replace `coi-default` with the Microsoft-maintained `mcr.microsoft.com/devcontainers/base:ubuntu`, re-express today's `ahjo-base` as a devcontainer Feature, and let repos declare dependencies via `.devcontainer/devcontainer.json` + Features. Drop the COI tool entirely once nothing else uses it.

End state: zero custom schemas, four image layers defined in one open vocabulary, Incus + Lima runtime untouched.

## Non-goals

- **No runtime swap.** ahjo does not adopt Docker, `@devcontainers/cli`, BuildKit-as-build-driver, or any container runtime other than Incus. The spec is parsed and executed against Incus directly.
- **No `dockerComposeFile` support.** Multi-container repos require Docker; rejected with explicit error.
- **No `customizations.vscode.*` honoring.** ahjo isn't a VS Code host; ignored without warning.
- **No auto-migration of `.ahjoconfig`.** Per ahjo convention (no runtime migration), users self-migrate; ahjo provides docs and parses `.ahjoconfig` as a deprecated alias.
- **Phase 4 (repo-supplied `image:` / `build:`) is deferred** until concrete demand. Phases 1-3 already deliver the harmonization win.
- **No greenfield rewrite.** ~5-10% of the codebase is touched; SSH wiring, branch CoW, raw.idmap, registry, mirror, and the CLI surface stay.

## Why now

- ahjo's image stack today has four layers (`coi-default` → `ahjo-base` → implicit per-repo bake → branch container) and three custom schemas (COI profile bash, `.ahjoconfig` TOML, ahjo's per-repo bake conventions). None of them are readable by any external tool.
- Phase 1 of `no-more-worktrees.md` is implemented; the codebase is now squarely "container holds the repo." That topology lines up with how devcontainer.dev expects to operate (workspace-in-container, lifecycle hooks executed via `exec`).
- Microsoft publishes `mcr.microsoft.com/devcontainers/base:ubuntu` (Ubuntu 22.04/24.04/26.04, multi-arch, `vscode` non-root user at UID 1000). Importable into Incus via OCI image import. Drops the maintenance burden of `coi-default`.
- Devcontainer Features are runtime-agnostic: each Feature is an OCI artifact containing `devcontainer-feature.json` + `install.sh`. The script runs as root inside any Linux container. ahjo can fetch and exec without touching Docker.

## Spec adoption is schema-only — runtime stays Incus + Lima

A devcontainer Feature is *just an OCI artifact*: a tarball with `devcontainer-feature.json` (metadata + options schema) and `install.sh` (root-executable shell script). Per the spec's Features page, the install script receives:

- User-provided options as `ALL_CAPS` env vars
- `_REMOTE_USER`, `_CONTAINER_USER`, `_REMOTE_USER_HOME`, `_CONTAINER_USER_HOME`
- `containerEnv` from the Feature's metadata

ahjo fetches the artifact (via a small Go OCI client; no `oras` binary dependency required), unpacks it, and runs `incus exec <container> -- bash install.sh` as root with options as env. That's the entire integration. No `@devcontainers/cli`, no BuildKit, no Docker.

The mapping from spec construct to ahjo execution:

| Spec construct | ahjo execution |
|---|---|
| `image: foo/bar` | `incus image import` from registry → use as launch image (Phase 4 only) |
| `build.dockerfile: ./Dockerfile` | Build via `buildah` on the VM → `incus image import` (Phase 4 only) |
| `features: { ... }` | Resolve dependency graph → fetch each artifact → `incus exec` `install.sh` as root, options as env vars |
| `postCreateCommand` | `incus exec <container> -- bash -c "<cmd>"` as `vscode`, in `/repo` |
| `postStartCommand` | Same, run during `prepareBranchContainer` |
| `containerEnv` | Merged into env at launch |
| `forwardPorts` | Seed `auto_expose` allowed-ports list |
| `remoteUser` / `containerUser` | Validated against ahjo's user (`vscode`); warn otherwise |
| `dockerComposeFile` | Rejected with clear error |

## Topology change

Before:

```
coi-default (built by COI tool, custom profile bash)
  └─ ahjo-base (COI profile: build.sh + config.toml; sshd + ahjo-claude-prepare + corepack + code user)
       └─ ahjo-<repo> default container (incus launch ahjo-base; clone /repo; warm install; .ahjoconfig run)
            └─ ahjo-<repo>-<branch> (incus copy + git checkout)
```

After:

```
mcr.microsoft.com/devcontainers/base:ubuntu (upstream OCI; vscode user at UID 1000)
  └─ ahjo-base (Incus image; built by applying ahjo-runtime Feature to base:ubuntu)
       └─ ahjo-<repo> default container (incus launch ahjo-base; clone /repo; apply repo's Features; lifecycle commands)
            └─ ahjo-<repo>-<branch> (incus copy + git checkout) — unchanged
```

Schemas retired: COI profile bash, `.ahjoconfig` TOML, per-repo bake conventions. Tool retired: COI itself. Runtime: Incus + Lima, unchanged.

## Phase 0 — Spike (timeboxed, ~1-2 days)

Three concrete checks before committing further. If any fails, revisit.

1. **Incus OCI import.** `skopeo copy docker://mcr.microsoft.com/devcontainers/base:ubuntu oci-archive:base.tar` (on Mac or VM), then `incus image import base.tar --alias ahjo-osbase`. Launch a container from it on Lima with `raw.idmap "both <hostUID> 1000"`. Validate: container starts, `vscode` user is at UID 1000:1000, `incus exec ... id vscode` returns expected, mounts work.
2. **Run a real Feature.** Pull `ghcr.io/devcontainers/features/common-utils:2` via a hand-rolled OCI fetcher (or `crane` for the spike). Unpack the tarball. `incus file push` `install.sh` into the container. `incus exec ... bash /tmp/install.sh` as root with `_REMOTE_USER=vscode _REMOTE_USER_HOME=/home/vscode INSTALLZSH=true` etc. Validate: install completes, declared options take effect.
3. **Round-trip a custom Feature.** Author a trivial Feature locally (`devcontainer-feature.json` + `install.sh` that touches `/marker`). Package via `oras push` to a local registry. Fetch + apply via the same code path as #2. Validates the artifact mechanics.

### What ahjo does in Phase 0

Pure validation; no production code paths change. Spike code lives in a scratch branch.

### Open questions

1. **OCI fetcher: handcrafted Go vs vendored library?** Look at `github.com/google/go-containerregistry`. If usable without Docker daemon assumptions, vendor it; else write a 200-LoC fetcher (registry auth, manifest pull, blob download, tar extract).
2. **`vscode` user UID — actually 1000 across all `base:ubuntu` variants?** Verify on 22.04, 24.04, 26.04 in the spike. The Dockerfile's `vscode` creation lives in a referenced script; pin to whatever variant we adopt.

## Phase 1 — ahjo-base as a Feature; replace `coi-default` with `base:ubuntu`

### What ahjo does

1. **`ahjo init` and `ahjo update` build pipeline becomes:**
   1. `incus image import` `mcr.microsoft.com/devcontainers/base:ubuntu` (cached locally; pull once per ahjo version bump).
   2. `incus launch <base:ubuntu> ahjo-build-<rand>` — transient container.
   3. Apply `ahjo-runtime` Feature using the same Feature-runner code path that user repos will use. The Feature's `install.sh` reproduces today's `internal/coi/assets/profiles/ahjo-base/build.sh`: `openssh-server`, `/etc/ssh/sshd_config.d/00-ahjo.conf`, host-keys directory at `/etc/ssh/ahjo-host-keys/`, `ahjo-claude-prepare` binary, corepack reconciliation. Reads `_REMOTE_USER` rather than hardcoding.
   4. `incus stop ahjo-build-<rand>`; `incus publish ahjo-build-<rand> --alias ahjo-base`.
   5. `incus delete ahjo-build-<rand>`.

2. **`ahjo repo add` and `ahjo new`** are unchanged in shape — they still launch from / copy from `ahjo-base`. The image's *contents* are bit-equivalent to today's `ahjo-base`; only the *definition* moves to standard format.

3. **In-image user becomes `vscode`** (matches `base:ubuntu`). raw.idmap continues to target UID 1000:1000. ahjo-runtime Feature reads `_REMOTE_USER` so a future Phase 4 can apply the same Feature against any image regardless of username.

### What disappears

| Removed | Location |
|---|---|
| COI profile build script | `internal/coi/assets/profiles/ahjo-base/build.sh` |
| COI profile config | `internal/coi/assets/profiles/ahjo-base/config.toml` |
| `coi build --profile ahjo-base` invocation | `internal/cli/init.go`, `internal/cli/update.go` |
| `code` user references in repo | various; replace with `vscode` |

### What changes shape

| File | Change |
|---|---|
| `internal/ahjoruntime/feature/devcontainer-feature.json` (new) | Feature metadata: id, version, name, options (none), `containerEnv` for corepack vars |
| `internal/ahjoruntime/feature/install.sh` (new) | Today's `build.sh` ported: apt installs, sshd config write, `ahjo-claude-prepare` install, corepack setup. Reads `_REMOTE_USER`. |
| `internal/ahjoruntime/embed.go` (new) | `go:embed` the Feature directory; expose as in-memory artifact for the Feature runner |
| `internal/devcontainer/features.go` (new) | Feature OCI fetch + dependency resolution + execution against an Incus container |
| `internal/devcontainer/oci.go` (new) | Minimal OCI client (or vendored `go-containerregistry`); manifest pull, blob fetch, tar extract |
| `internal/cli/init.go` | Replace `coi build` with the new build pipeline (import base, launch transient, apply Feature, publish, delete) |
| `internal/cli/update.go` | Same replacement |
| `internal/coi/template.go` | Stop using COI; `internal/coi/` becomes vestigial pending Phase 3 |
| `CONTAINER-ISOLATION.md` | Note: `ahjo-runtime` Feature install runs at image-build time inside transient container; same trust posture as today's COI build |

### Lifecycle hazards

1. **Image-pull network dep at `ahjo init`/`update`.** Today: COI fetches `coi-default`. Tomorrow: ahjo fetches `base:ubuntu` from `mcr.microsoft.com`. Same posture; document the new registry.
2. **`base:ubuntu` size.** Likely larger than `coi-default` (zsh, oh-my-zsh, sudo). One-time pull on Lima VM; acceptable.
3. **UID assumption baked into the Feature.** `_REMOTE_USER=vscode` for `base:ubuntu`. Phase 4 generalizes; Phase 1 hardcodes the call site to pass `vscode`.
4. **Existing dev installs need re-init.** `ahjo init` will pull a different base; existing `ahjo-base` becomes stale. Per ahjo dev-mode convention, users `ahjo nuke && ahjo init`.

### Open questions

1. **Where does `incus publish` write images?** Default storage; confirm size budget on the 50 GB Lima disk after both `ahjo-osbase` and `ahjo-base` exist.
2. **`vscode` shell config.** `base:ubuntu` ships zsh + oh-my-zsh. Today ahjo doesn't impose a multiplexer; per existing memory, we should also avoid imposing a shell prompt. Verify `ahjo shell` drops into bash by default unless user opted into zsh.
3. **`ahjo-claude-prepare` portability.** Bash today, references `/home/code/.claude.json`. Update for `vscode` and validate against `_REMOTE_USER_HOME`.

## Phase 2 — devcontainer.json as per-repo schema

### What ahjo does

1. **`ahjo repo add foo/bar`** (added to existing flow):
   1. After `git clone /repo`, parse `.devcontainer/devcontainer.json` (jsonc) using spec precedence: `.devcontainer/devcontainer.json` → `.devcontainer.json` → `.devcontainer/<name>/devcontainer.json`.
   2. If `image:` / `build:` / `dockerComposeFile:` present, abort with explicit error referencing Phase 4. Better than silent ignore.
   3. If `features:` present and any source isn't `ghcr.io/devcontainers/features/*`, prompt once for trust. Persist consent in registry.
   4. Resolve Feature dependency graph (spec algorithm: `dependsOn` hard, `installsAfter` soft).
   5. Apply each Feature via the runner from Phase 1.
   6. Run lockfile-detection warm install (existing logic, unchanged).
   7. Run `postCreateCommand` if present; treat as concatenated with `.ahjoconfig` `run` (devcontainer.json wins on conflict, deprecation notice if both exist).
   8. `incus stop`, register branch, done.

2. **`ahjo shell` / `ahjo new`** (small additions):
   - `prepareBranchContainer` runs `postStartCommand` if defined in the cached devcontainer config.
   - `postAttachCommand` runs at the moment ahjo attaches the user shell.

3. **`.ahjoconfig` parsing path remains** as a deprecated alias. One-time deprecation notice when both `.ahjoconfig` and `devcontainer.json` exist in the same repo.

### What disappears

| Removed | Location |
|---|---|
| `.ahjoconfig`-only enforcement (no devcontainer fallback) | `internal/ahjoconfig/config.go` |
| `forward_env` static list as the only way to set env | `internal/ahjoconfig/config.go`, README |

(The TOML parser stays, just demoted.)

### What changes shape

| File | Change |
|---|---|
| `internal/devcontainer/config.go` (new) | jsonc parser, spec-precedence file lookup, `Config` struct with the honored subset |
| `internal/devcontainer/lifecycle.go` (new) | `RunPostCreate(ctx, container, cfg)`, `RunPostStart(ctx, container, cfg)`, `RunPostAttach(ctx, container, cfg)` thin wrappers around `incus exec` |
| `internal/ahjoconfig/config.go` | Read both files; merge with devcontainer-derived fields as primary; emit one-time deprecation notice when both exist |
| `internal/cli/repo.go:repoAddSetup()` | Insert devcontainer parse → trust prompt → features apply → lifecycle commands. Reject `image`/`build`/`dockerComposeFile` with explicit error. |
| `internal/cli/shell.go:prepareBranchContainer()` | Run `postStartCommand` if cached config has it |
| `internal/cli/claude.go`, other attach paths | `postAttachCommand` hook |
| `internal/registry/registry.go` | Add `FeatureConsent map[string]bool` per `Repo` (key = registry hostname or pattern) |
| `README.md` | `.devcontainer/devcontainer.json` example; ignored-fields list with rationale; `.ahjoconfig` migration note |
| `CONTAINER-ISOLATION.md` | Document Features run with internet access during repo-add; trust-prompt policy |

### Lifecycle hazards

1. **First non-curated Feature source = blocking prompt.** Persist consent so re-adds don't re-prompt. Skip prompt for `ghcr.io/devcontainers/features/*` (curated upstream).
2. **Features install during `repo add`.** Captured by `incus copy` for branch containers via btrfs CoW (one-time cost per repo). Confirm reflinks survive the install layer.
3. **`postCreateCommand` ordering vs `.ahjoconfig` `run`.** When both exist, devcontainer.json wins; `.ahjoconfig` `run` ignored with deprecation notice. Don't try to merge the lists — confusing.
4. **`remoteUser` mismatch.** If devcontainer.json declares `remoteUser: code`, warn and use `vscode` anyway. Fail loudly if mismatched — silently switching users breaks `git config`/keys.
5. **Mirror still in flight.** Phase 2 of this doc must wait for `in-container-mirror.md` Phase 2 to stabilize. Don't compound regressions.

### Open questions

1. **jsonc parser.** Does any well-maintained Go jsonc library exist? Or strip comments + parse as JSON?
2. **`forwardPorts` semantics.** Devcontainer's `forwardPorts` declares ports the user *wants exposed*; ahjo's `auto_expose` is *allowed to expose* + min_port floor. Map devcontainer's list to ahjo's allowlist; preserve auto_expose's gating.
3. **`containerEnv` vs ahjo's `forward_env`.** `containerEnv` is static values; `forward_env` is "forward this from the host." Two different mechanisms — keep both, document the distinction. ahjo continues to forward `CLAUDE_CODE_OAUTH_TOKEN` via `forward_env`.
4. **Devcontainer caches the resolved config where?** Today nothing is cached; everything reads at use time. For `postStartCommand`, we need the config available at branch-container start time. Options: re-parse on each start (one extra `incus exec ... cat`), or persist the parsed config in registry. Lean: re-parse — it's an exec call, cheap, always fresh.

## Phase 3 — Drop COI

### What ahjo does

After Phase 1 builds `ahjo-base` without COI's profile mechanism, COI's runtime contribution to ahjo is gone. Audit for residual usage:

- Image management: replaced by direct `incus image import` and the Feature pipeline in Phase 1.
- Container lifecycle: ahjo already drives this via `internal/incus/incus.go`.
- `internal/coi/template.go` and the `coi.RenderConfig` callers: already retired in `no-more-worktrees.md` Phase 1.
- Anything that survives Phase 1: refactor to direct Incus calls.

If clean, delete `internal/coi/` entirely.

### What disappears

| Removed | Location |
|---|---|
| Entire COI integration | `internal/coi/` |
| COI install step in init flow | `internal/cli/init.go` (the section that ensures COI is installed and at version `v0.8.1`) |
| COI version pin | `internal/coi/coi.go:39` |
| COI Lima auto-detect workaround documentation | reduce/inline; no longer relevant if COI isn't a dependency |
| Any `coi` binary references in tests, fixtures, install docs | repo-wide grep |

### What changes shape

| File | Change |
|---|---|
| `internal/cli/init.go` | Remove COI install/update; init now ensures Incus + Lima only |
| `internal/cli/update.go` | Remove COI rebuild step; only rebuild ahjo-base via the Phase 1 pipeline |
| `internal/cli/nuke.go` | Remove COI artifacts from the teardown list |
| `README.md`, `CLAUDE-SETTING.md`, install docs | Drop COI references |

### Lifecycle hazards

1. **Existing installs have COI installed.** Harmless — the binary just sits there unused. `ahjo nuke` removes it as part of teardown; otherwise a manual `apt remove` works.
2. **CLAUDE-SETTING.md.** Today notes that "COI copies host→container and overwrites build.sh edits." That entire warning becomes stale after Phase 1; rewrite.
3. **The "rolling-current toolchains" memory** — confirm this still applies. With COI gone, ahjo-runtime Feature is the only place tools could be pinned; keep using upstream Feature defaults to preserve rolling-current behavior.

### Open questions

1. **Anything else in `internal/coi/` that other packages still import?** Grep before deleting; expect zero, but verify.
2. **Does `coi-default`'s Claude Code preinstall move to the ahjo-runtime Feature, or to a separate Claude-specific Feature?** Lean: separate Feature (`ahjo-claude`) so users can mix-and-match if a future variant doesn't want Claude. ahjo init applies both `ahjo-runtime` and `ahjo-claude` to build `ahjo-base`.

## Phase 4 (deferred, demand-driven) — Honor repo-supplied images

Allow `image:` and `build:` in repo's devcontainer.json. Skipped here to keep scope tight; design when a concrete repo needs a base `ahjo-base` can't accommodate. Sketch:

- Pull/build the declared image; `incus image import`.
- Launch container from it.
- Apply `ahjo-runtime` Feature as the final layer (provides sshd, host-keys dir, claude-prepare; reads `_REMOTE_USER` so any user account works).
- Trust prompt once per repo when image isn't `ahjo-base`.
- UID handling: ahjo-runtime Feature's `install.sh` ensures the container has a user at UID 1000 — create, rename, or accept depending on the source image. raw.idmap math unchanged.

## What's deliberately omitted

- **`@devcontainers/cli` as a build driver.** Adds Node + Docker assumptions. ahjo runs Features directly via `incus exec`.
- **VS Code `customizations.vscode.*`.** Not relevant; ignored without warning.
- **`initializeCommand`.** Spec defines it as host-side; under ahjo this would mean "on the Lima VM," which is awkward. Defer until demand.
- **`dockerComposeFile`.** Multi-container repos require Docker semantics. Reject with clear error.
- **Auto-migration of `.ahjoconfig`.** Per ahjo dev-mode convention, users self-migrate. Provide migration docs in README.
- **A separate `ahjo-invariants.md`** capturing implicit contracts in the kept code (SSH wiring, raw.idmap, branch CoW, registry, mirror). Independently valuable but not gating; written incrementally as each phase touches the relevant subsystem.
