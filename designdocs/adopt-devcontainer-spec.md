# Adopt devcontainer.dev spec; drop COI

**Status:** draft · **Scope:** ahjo build pipeline; `repo add`, `new`, `shell` lifecycle; per-repo config schema; COI dependency removal · **Supersedes:** prior draft of this doc that proposed `mcr.microsoft.com/devcontainers/base:ubuntu` as the base image (rejected — see "Why this shape")

## Goal

Retire COI as a build dependency and ahjo's bespoke `.ahjoconfig` schema in favor of two open mechanisms:

- **devcontainer Features** as the ahjo-internal way of expressing image build steps *and* the per-repo way of declaring container deps. A Feature is just a tarball (distributed via OCI registry / HTTPS / local dir) containing `devcontainer-feature.json` + `install.sh`. The script runs as root in any Linux container with a small set of env vars. ahjo fetches and runs them via `incus exec` — no Docker, no `@devcontainers/cli`, no BuildKit.
- **`devcontainer.json`** as the per-repo config schema. ahjo honors a runtime-neutral subset: `features`, `onCreateCommand`, `postCreateCommand`, `postStartCommand`, `postAttachCommand`, `containerEnv`, `forwardPorts`, `remoteUser`, `containerUser`, plus `customizations.ahjo` (ahjo-only extensions). Image-related and Docker-flavored fields (`image`, `build`, `dockerComposeFile`, `mounts`, `runArgs`, `secrets`) are rejected with explicit errors — they assume Docker image semantics and ahjo runs Incus system containers.

Runtime stays Incus + Lima — system containers with systemd and sshd-as-a-service. The base image stays an upstream Incus image: `images:ubuntu/24.04` (distrobuilder-built, multi-arch, comes with systemd) instead of the COI-built `coi-default`. Today's `coi-default` + `ahjo-base` contents — openssh-server, sshd config, `ahjo-claude-prepare`, the `ubuntu` user (replacing today's bespoke `code` account; adopt the upstream image's canonical user), corepack — become an `ahjo-runtime` Feature applied on top of `images:ubuntu/24.04`.

End state: zero custom-bash schemas, two image layers (upstream + ahjo-runtime), Incus + Lima runtime untouched, `.devcontainer/devcontainer.json` as the per-repo schema (with ahjo's bits under `customizations.ahjo`), COI removed.

## Why now

- ahjo's image stack today is four layers (`coi-default` → `ahjo-base` → implicit per-repo bake → branch container) and three custom schemas (COI profile bash, `.ahjoconfig` TOML, ahjo's per-repo bake conventions). None readable by any external tool.
- Phase 1 of `no-more-worktrees.md` is implemented; the codebase is squarely "container holds the repo." That topology lines up with how devcontainer.dev expects to operate (workspace-in-container, lifecycle hooks executed via exec).
- Devcontainer Features are an open, mature vocabulary with a maintained `ghcr.io/devcontainers/features/*` set covering Node, Python, Go, kubectl, AWS CLI, common-utils, and dozens more. ahjo would otherwise be reinventing this catalog one `.ahjoconfig run` string at a time.

## Why this shape

**Devcontainer images are Docker images.** `image:` / `build:` / `dockerComposeFile:` in the spec mean *Docker artifacts*. Microsoft's `mcr.microsoft.com/devcontainers/base:ubuntu` is built `FROM buildpack-deps:noble-curl`, has no `/sbin/init`, no systemd, no CMD/ENTRYPOINT — it's designed for `docker exec`. Importing it into Incus produces an *application container* (`CONTAINER (APP)`), where Incus's incusd is PID 1 by design ("OCI images don't run an init system" per the Incus maintainer). Layering ahjo's sshd-as-a-service expectations on top would force one of: distrobuilder repack (heavy host dep), drop systemd (real topology shift, much wider blast radius than this doc's scope), or reimplement Docker semantics on Incus. None are improvements.

**Devcontainer Features are runtime-agnostic.** Per the spec, `install.sh` runs as root in any Linux container with `_REMOTE_USER`, `_CONTAINER_USER`, `_REMOTE_USER_HOME`, `_CONTAINER_USER_HOME`, and options as `ALL_CAPS` env vars. Nothing about Docker. ahjo can fetch the artifact and run it against an Incus system container with a few hundred lines of Go.

**`images:ubuntu/24.04` already gives us what `coi-default` did.** Distrobuilder-built upstream Incus image, full Ubuntu userspace with systemd, multi-arch. That's the foundation `coi-default` was wrapping. Use it directly; layer `ahjo-runtime` on top via the same Feature runner that user repos use.

**Why not just keep `.ahjoconfig`?** The TOML schema is bespoke; its `run` field is a per-repo bash one-liner. devcontainer.json's `features` brings a vocabulary plus an ecosystem of community install scripts addressed by stable IDs. Repos that already have a `devcontainer.json` (Codespaces / VS Code Remote users) get partial out-of-the-box support. `.ahjoconfig` is retired entirely: `run` becomes `postCreateCommand`; `forward_env` and `auto_expose` (host→container concerns with no devcontainer equivalent) move to `customizations.ahjo` inside `devcontainer.json`, alongside `customizations.vscode` and `customizations.codespaces` — the spec's blessed extension namespace. Users self-migrate per ahjo's no-runtime-migration convention; presence of a legacy `.ahjoconfig` triggers an explicit migration error pointing at the docs.

## Non-goals

- **No runtime swap.** ahjo does not adopt Docker, `@devcontainers/cli`, BuildKit, or any container runtime other than Incus + Lima. The spec is parsed and Features are executed against Incus directly.
- **No `image:` / `build:` honoring.** Repos that declare these abort with an explicit error pointing at this doc. They imply a Docker image-build runtime ahjo doesn't have. Phase 4 sketches a future where they're allowed against an opt-in path; not delivered here.
- **No `dockerComposeFile:` support.** Multi-container repos require Docker; rejected with explicit error.
- **No `mounts:` or `runArgs:` honoring.** Both Docker-flavored; rejected with explicit error rather than silent ignore.
- **No `secrets:` support.** Spec's secret-injection model is security-sensitive; rejected with explicit error rather than silent ignore. Revisit when there's a concrete user need and a vetted design.
- **No non-ahjo `customizations.*` honoring.** ahjo isn't a VS Code host or Codespaces; only `customizations.ahjo.*` is read. Other tools' blocks are ignored without warning.
- **No `.ahjoconfig` parsing path.** Per ahjo convention (no runtime migration), users self-migrate. Presence of a legacy `.ahjoconfig` triggers an explicit error pointing at the migration doc; the parser is deleted, not preserved as a deprecated alias.
- **No greenfield rewrite.** ~5-10% of the codebase is touched; SSH wiring, branch CoW, raw.idmap, registry, mirror, and the CLI surface stay unchanged.
- **No further username changes beyond `ubuntu`.** Phase 1 switches the in-image user from `code` to `ubuntu` (adopt the upstream image's canonical name; stop maintaining a parallel ahjo-only account). raw.idmap target stays UID 1000:1000 — only the name moves. ahjo's `incus exec` invocations pass `_REMOTE_USER=ubuntu`. Features read `_REMOTE_USER` from env rather than hardcoding, so future renames (Phase 4) still work without per-Feature edits.

## Topology

Before:

```
coi-default                  (built by COI tool, custom profile bash)
  └─ ahjo-base               (COI profile build.sh: sshd + ahjo-claude-prepare + corepack + code user)
       └─ ahjo-<repo>        (incus launch ahjo-base; clone /repo; warm install; .ahjoconfig run)
            └─ ahjo-<repo>-<branch>   (incus copy + git checkout)
```

After:

```
images:ubuntu/24.04          (upstream Incus system container, distrobuilder-built; systemd; multi-arch)
  └─ ahjo-base               (Incus image; built by applying ahjo-runtime Feature to images:ubuntu/24.04)
       └─ ahjo-<repo>        (incus launch ahjo-base; clone /repo; apply repo Features per devcontainer.json; lifecycle commands)
            └─ ahjo-<repo>-<branch>   (incus copy + git checkout) — unchanged
```

Schemas retired: COI profile bash, `.ahjoconfig` TOML (deleted, not aliased), per-repo bake bash. Schema added: `.devcontainer/devcontainer.json` (subset honored) with ahjo extensions under `customizations.ahjo`. Tool retired: COI itself. Runtime: Incus + Lima system containers, unchanged.

## Spec → ahjo execution mapping

| Spec construct                | ahjo execution                                                                                                                                       |
|-------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------|
| `image: foo/bar`              | **Rejected** with explicit error referencing Phase 4                                                                                                  |
| `build.dockerfile: ...`       | **Rejected** with explicit error referencing Phase 4                                                                                                  |
| `dockerComposeFile: ...`      | **Rejected** — multi-container repos require Docker                                                                                                   |
| `mounts: [...]` / `runArgs: [...]` | **Rejected** with explicit error                                                                                                                 |
| `secrets: { ... }`            | **Rejected** with explicit error — security-sensitive; needs separate design                                                                          |
| `features: { ... }`           | Resolve dep graph (`dependsOn` hard recursive, `installsAfter` soft conditional) → fetch each artifact via a stdlib OCI client (`net/http` + tar) → `incus exec install.sh` as root, options as `ALL_CAPS` env |
| `initializeCommand`           | Ignored with one-time deprecation notice (host-side concept; means "Lima VM" under ahjo, awkward; defer)                                              |
| `onCreateCommand`             | Run during `repo add`, after user creation, before `postCreateCommand`, as `ubuntu` in `/repo`                                                        |
| `updateContentCommand`        | Ignored with one-time deprecation notice (no ahjo refresh hook yet)                                                                                   |
| `postCreateCommand`           | `incus exec <container> -- bash -c "<cmd>"` as `ubuntu`, in `/repo`, during repo-add                                                                  |
| `postStartCommand`            | Same, run during `prepareBranchContainer` (every container start)                                                                                     |
| `postAttachCommand`           | Same, run when ahjo attaches a user shell                                                                                                             |
| `waitFor`                     | Ignored — ahjo runs lifecycle commands sequentially; semantics implicit                                                                               |
| `containerEnv: { K: V }`      | Merged into env at launch                                                                                                                             |
| `remoteEnv: { K: V }`         | Ignored without warning (no remote-tool concept)                                                                                                      |
| `forwardPorts: [3000, ...]`   | Seed `auto_expose` allowed-ports list                                                                                                                 |
| `portsAttributes`             | Ignored without warning                                                                                                                               |
| `remoteUser` / `containerUser`| Validated against ahjo's `ubuntu`; warn loudly if mismatched (silently switching users breaks `git config` / keys)                                    |
| `hostRequirements`            | Ignored without warning (Lima VM is fixed-size)                                                                                                       |
| `customizations.ahjo.*`       | Honored — see "`customizations.ahjo` schema" below                                                                                                    |
| `customizations.vscode.*`     | Ignored without warning                                                                                                                               |
| `customizations.<other>.*`    | Ignored without warning                                                                                                                               |

**Feature-level Docker fields.** A Feature's own `devcontainer-feature.json` is filtered in two tiers:

- **Hard-reject** (errors out at metadata-parse, before fetch): `mounts`, `privileged`. The Feature genuinely relies on these at runtime (docker-in-docker needs its `/var/lib/docker` volume; nix needs `/nix` bound; minikube needs `/home/vscode/.minikube`). Letting `install.sh` succeed without them would leave a Feature that looks installed but breaks on first use.
- **Warn-and-ignore** (one `warn:` line per field, then proceed): `capAdd`, `securityOpt`, `init`, `entrypoint`. These are Docker-runtime hints with no Incus equivalent under ahjo's profile, or already provided by systemd. Known values get value-specific notes (e.g. `SYS_PTRACE` → "in-container ptrace already works"; `seccomp=unconfined` → "Incus seccomp is profile-managed"; `label=disable` → "no SELinux on ahjo"; `init: true` → "systemd is PID 1"); unknown values fall through to a generic "no Incus equivalent, ignoring" note. This is what lets curated language Features (`go:1`, `rust:1`, …) install on ahjo — they declare debugger-related caps that the Incus topology already satisfies differently.

The original draft of this doc rejected all six fields uniformly, citing "curated Features don't use these in practice." That assumption broke as soon as the curated `go:1` Feature (which declares `init: true`, `capAdd: [SYS_PTRACE]`, `securityOpt: [seccomp=unconfined]` purely for delve support) was tried — most curated language Features now ship the same trio. The two-tier split keeps the hard guard where it matters (real semantic dependencies) and turns the rest into informative warnings rather than a wall.

### Feature runner contract

The Feature runner is the new code. The fetch + dependency-graph layers are
deferred to Phase 2 (no Phase 1 caller exercises them — only the embedded
`ahjo-runtime` Feature is applied at image-bake time). Phase 1 ships only
Apply; Phase 2 adds Fetch + Resolve on top.

1. **Fetch (Phase 2).** Hand-rolled OCI Distribution v2 client over `net/http` (no Docker daemon dep, no `go-containerregistry` dep): bearer-token handshake from `WWW-Authenticate`, manifest fetch with `Accept: application/vnd.oci.image.manifest.v1+json`, blob fetch via the manifest's single layer descriptor. Anonymous reads work for `ghcr.io/devcontainers/features/*`. Tar extraction enforces path safety: reject entries with `..`, absolute paths, or symlinks pointing outside the Feature dir.
2. **Resolve (Phase 2).** Build dependency graph from `dependsOn` (hard, recursive) and `installsAfter` (soft, conditional — only enforced if both Features are in the install set). Topological sort with cycle detection. Restart-between-rounds is not supported; if a Feature requires it, ahjo errors with "Feature X requires restart between install rounds; ahjo doesn't support this."
3. **Apply (Phase 1).** For each Feature in resolved order (Phase 1: a single Feature, ahjo-runtime):
   - `incus file push --recursive` the unpacked Feature dir into `/tmp/feature-<id>/`
   - `incus exec <container> --env _REMOTE_USER=ubuntu --env _REMOTE_USER_HOME=/home/ubuntu --env _CONTAINER_USER=ubuntu --env _CONTAINER_USER_HOME=/home/ubuntu --env <OPTION_KEY>=<value> ... -- bash /tmp/feature-<id>/install.sh`
   - 10-minute default timeout per Feature, configurable via ahjo flag
   - Stream stdout/stderr to ahjo's log; buffer per-Feature for error reporting
   - Cleanup `/tmp/feature-<id>/` on success or failure

**Failure semantics.** A failed Feature aborts the rest of the install set. The container is left in a partial state and the calling command (`ahjo repo add`, `ahjo update`) fails. ahjo doesn't auto-rollback; the user retries after fixing the cause.

User identity flows through env vars, not through hardcodes. The user is `ubuntu` (the upstream image's canonical user; replaces today's bespoke `code` per the reuse-canonical-names rule). Every Feature — including `ahjo-runtime` — reads `_REMOTE_USER` rather than naming a user. ahjo's `incus exec` invocations always pass `_REMOTE_USER=ubuntu`; future renames (Phase 4) change both the image *and* what ahjo passes (neither alone is sufficient).

## Phase 0 — Spike (timeboxed, 1-2 days)

Three checks against committed implementation choices (see "Locked-in decisions" below). The spike validates the choices, not selects between alternatives.

1. **Feature runner round-trip.** Launch a fresh `images:ubuntu/24.04` container on Lima. Hand-roll a trivial Feature locally (`devcontainer-feature.json` + `install.sh` that creates `/marker` and writes `_REMOTE_USER` to it). Run via the prototype Feature runner. Validate: install completes, env vars land, `/marker` shows `ubuntu`.
2. **Curated upstream Feature.** Pull `ghcr.io/devcontainers/features/common-utils:2` via the stdlib OCI client. Apply to a fresh `images:ubuntu/24.04` container with `_REMOTE_USER=ubuntu _REMOTE_USER_HOME=/home/ubuntu INSTALLZSH=true` (and friends). Validate: install completes, declared options take effect, the resulting userspace has zsh.
3. **`installsAfter` ordering.** Apply two Features where one declares `installsAfter` the other (e.g., `node` after `common-utils`). Validate: the runner installs in the declared order, not in declared-set order.

If all three pass, the design is proven. If #2 fails because the Feature assumes the user already exists, the ordering rule is already encoded: `ahjo-runtime` Feature ensures `ubuntu` exists at image-bake time; user Features at repo-add always run against a container with `ubuntu` present.

### Locked-in decisions (no longer open)

- **OCI client:** hand-rolled stdlib client (`net/http` + `archive/tar` + `compress/gzip`). The OCI Distribution v2 read path is small enough (~200 lines) that adopting `go-containerregistry` for it would pull ~40 transitive deps (OpenTelemetry, logrus, docker/cli, moby/moby, image-spec, oauth2) for what amounts to two HTTP GETs and a tar extraction. Anonymous read works for the curated `ghcr.io/devcontainers/features/*` set and any other public repo via the standard `WWW-Authenticate` → token-endpoint → bearer handshake.
- **JSONC parser:** `tailscale/hujson` configured to allow both comments **and** trailing commas. Real-world devcontainer.json files (especially Codespaces-targeting) use trailing commas; spec-strict parsing rejects ~10% of in-the-wild files. Lax dialect is an intentional deviation from spec for compatibility.
- **`images:ubuntu/24.04` `ubuntu` user:** keep it. The `ahjo-runtime` Feature uses the upstream `ubuntu` user (UID 1000) as-is, or creates one at UID 1000 if absent. ahjo's previous `code` user is retired — adopting the upstream image's canonical user instead of maintaining a parallel ahjo-only account. Verified across both arches in spike #1.
- **Feature trust persistence:** per-Feature-ID-pattern. `ghcr.io/devcontainers/features/*` is auto-trusted (curated upstream). Other patterns require one-time consent, persisted in registry (`FeatureConsent map[string]bool` keyed by glob, not hostname).

## Phase 1 — Build pipeline rewrite; `ahjo-runtime` as a Feature

### What ahjo does

1. **`ahjo init` and `ahjo update` build pipeline becomes:**
   1. Verify Incus + Lima are present and running. (No COI; per ahjo's "no silent host installs" rule, missing prereqs warn + prompt rather than auto-install.)
   2. `incus image copy images:ubuntu/24.04 local: --alias ahjo-osbase` — pull once per ahjo version bump.
   3. `incus launch ahjo-osbase ahjo-build-<rand>` — transient container. systemd boots normally.
   4. Apply the `ahjo-runtime` Feature using the Feature runner. The Feature's `install.sh` is today's `internal/coi/assets/profiles/ahjo-base/build.sh`, ported: ensure `ubuntu` user exists at UID 1000 (use upstream's if present; create otherwise), `apt install openssh-server`, write `/etc/ssh/sshd_config.d/00-ahjo.conf`, create `/etc/ssh/ahjo-host-keys/`, install `ahjo-claude-prepare` (Claude Code preinstall + onboarding marker), set up corepack, `systemctl enable ssh`. Reads `_REMOTE_USER`/`_REMOTE_USER_HOME` from env rather than hardcoding the user.
   5. `incus stop ahjo-build-<rand>`; `incus publish ahjo-build-<rand> --alias ahjo-base`.
   6. `incus delete ahjo-build-<rand>`.

2. **`ahjo repo add` and `ahjo new`** are unchanged in shape. They still launch from / copy from `ahjo-base`. The image's contents are functionally equivalent to today's `ahjo-base` (sshd as a service, `ubuntu` user at UID 1000, claude-prepare, corepack); only the build mechanism moves to standard format and the username changes from `code` to `ubuntu`.

3. **In-image user is `ubuntu`** (canonical Ubuntu cloud-image name; replaces today's bespoke `code`). raw.idmap continues to target UID 1000:1000 — only the name moves. The `ahjo-runtime` Feature reads `_REMOTE_USER` from env, so future renames work without Feature edits.

### What disappears

| Removed                                             | Location                                                 |
|-----------------------------------------------------|----------------------------------------------------------|
| COI profile build script                            | `internal/coi/assets/profiles/ahjo-base/build.sh`        |
| COI profile config                                  | `internal/coi/assets/profiles/ahjo-base/config.toml`     |
| `coi build --profile ahjo-base` invocation          | `internal/cli/init.go`, `internal/cli/update.go`         |
| The `coi-default` image as ahjo's base              | implicit; replaced by `images:ubuntu/24.04`              |

### What changes shape

| File                                                            | Change                                                                                                                  |
|-----------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------|
| `internal/ahjoruntime/feature/devcontainer-feature.json` (new)  | Feature metadata: `id: ahjo-runtime`, version, options (none yet), `containerEnv` for corepack vars                     |
| `internal/ahjoruntime/feature/install.sh` (new)                 | Today's `build.sh` ported. Uses upstream `ubuntu` user (or creates at UID 1000 if absent); reads `_REMOTE_USER`/`_REMOTE_USER_HOME`; no hardcoded user account name |
| `internal/ahjoruntime/embed.go` (new)                           | `go:embed` the Feature dir; expose as in-memory artifact for the Feature runner                                         |
| `internal/devcontainer/features.go` (new)                       | Feature execution against an Incus container — `Apply(container, feature, env, out)`. Validates the Feature's metadata (rejects Docker-flavored fields) before running install.sh. Phase 2 adds dependency resolution on top. |
| `internal/devcontainer/build.go` (new)                          | `BuildAhjoBase(out, force)` orchestrator: image-copy upstream → launch transient → apply embedded ahjo-runtime → publish → delete. Called by both init.go and update.go.                                                       |
| `internal/devcontainer/oci.go`                                  | Deferred to Phase 2. No Phase 1 caller exercises OCI fetch — only the embedded ahjo-runtime Feature is applied. Phase 2b lands a hand-rolled stdlib OCI client there (`net/http` + `archive/tar`) when user repos start declaring `features:` from the OCI ecosystem. |
| `internal/cli/init.go`                                          | Replace COI install + `coi build` with: verify Incus+Lima, image-copy upstream, launch transient, apply Feature, publish, delete (single step calling `devcontainer.BuildAhjoBase`)                                            |
| `internal/cli/update.go`                                        | Same replacement for the rebuild path; force-replace ahjo-base alias                                                                                                                                                          |
| `internal/coi/`                                                 | Phase 1 deletes the package outright (was only retained transitionally for preflight; preflight's COI checks go away in this phase too)                                                                                       |
| `CONTAINER-ISOLATION.md`                                        | Note: `ahjo-runtime` Feature install runs at image-build time inside transient container; same trust posture as today's COI build                                                                                            |

### Lifecycle hazards

1. **Image-pull network dep at `ahjo init`/`update`.** Today: COI fetches `coi-default`. Tomorrow: ahjo fetches `images:ubuntu/24.04` from `images.linuxcontainers.org`. Same posture; document the new registry.
2. **`ahjo-runtime` Feature must run before any user Feature** that assumes `ubuntu` exists. Encoded by sequencing: `ahjo-runtime` always first when building `ahjo-base`; user Features at `repo add` always run after, against a container with `ubuntu` already present at UID 1000.
3. **Existing dev installs need re-init.** `ahjo init` will pull a different base; existing `ahjo-base` becomes stale. Per ahjo dev-mode convention, users `ahjo nuke && ahjo init`.

### Open questions

1. **Where does `incus publish` write images?** Default storage; confirm size budget on the 50 GB Lima disk after both `ahjo-osbase` and `ahjo-base` exist.
2. **`ahjo-claude-prepare` portability.** Bash today, references `/home/code/.claude.json` (under the legacy `code` user). With the user switching to `ubuntu`, kill the hardcoded path entirely: read `_REMOTE_USER_HOME` from env, which resolves to `/home/ubuntu`.

## Phase 2 — `devcontainer.json` as per-repo schema

### What ahjo does

1. **`ahjo repo add foo/bar`** (added to existing flow):
   1. After `git clone /repo`, refuse if a legacy `.ahjoconfig` exists with an explicit migration error; otherwise parse `.devcontainer/devcontainer.json` (lax JSONC: comments + trailing commas) using spec precedence: `.devcontainer/devcontainer.json` → `.devcontainer.json` → `.devcontainer/<name>/devcontainer.json`. ahjo-specific knobs (`forward_env`, `auto_expose`) live under `customizations.ahjo` — same JSON, no second file.
   2. If `image:` / `build:` / `dockerComposeFile:` / `mounts:` / `runArgs:` / `secrets:` present, abort with explicit error linking to this doc and the README "honored / rejected / ignored" tables.
   3. If `features:` present and any source doesn't match a trusted pattern in `FeatureConsent` (curated `ghcr.io/devcontainers/features/*` is auto-trusted), prompt once for trust. Persist consent in registry as a glob pattern.
   4. Resolve Feature dep graph (`dependsOn` hard recursive, `installsAfter` soft conditional). Reject Features whose `devcontainer-feature.json` declares Docker-flavored fields (`mounts`, `privileged`, `capAdd`, `securityOpt`, `init`, `entrypoint`) with explicit error citing the Feature ID.
   5. Apply each Feature via the Feature runner with `_REMOTE_USER=ubuntu _REMOTE_USER_HOME=/home/ubuntu _CONTAINER_USER=ubuntu _CONTAINER_USER_HOME=/home/ubuntu`.
   6. Run lockfile-detection warm install (existing logic, unchanged).
   7. Run lifecycle hooks in spec order, all as `ubuntu` in `/repo`. Each supports the spec's three forms: string, array of args, or object map (parallel commands). A failed command aborts the chain.
      a. `onCreateCommand`
      b. `postCreateCommand`
   8. `incus stop`, register branch, done.

2. **`ahjo shell` / `ahjo new`** (small additions):
   - `prepareBranchContainer` runs `postStartCommand` if defined (every container start).
   - `postAttachCommand` runs at the moment ahjo attaches the user shell.

3. **`customizations.ahjo`** is the ahjo-only extension block inside `devcontainer.json`, following the spec's convention for tool-specific config (`customizations.vscode.*`, `customizations.codespaces.*`). Two fields: `forward_env` and `auto_expose`. See "`customizations.ahjo` schema" below.

### `customizations.ahjo` schema

ahjo's small per-repo extension lives under the spec's `customizations` namespace, adjacent to `customizations.vscode` and `customizations.codespaces`. No second file.

```jsonc
{
  // Standard devcontainer.json fields...
  "features": {
    "ghcr.io/devcontainers/features/node:1": { "version": "20" }
  },
  "postCreateCommand": "echo hi",

  // ahjo-only extensions:
  "customizations": {
    "ahjo": {
      "forward_env": ["CLAUDE_CODE_OAUTH_TOKEN"],
      "auto_expose": {
        "enabled": true,
        "min_port": 3000
      }
    }
  }
}
```

| Field | Type | Purpose |
|---|---|---|
| `forward_env` | `string[]` | Host env vars to forward into the container at launch. Static list; values resolved at container start. |
| `auto_expose.enabled` | `bool` | Gate for the auto-expose machinery. When false, `forwardPorts` from devcontainer.json is ignored. |
| `auto_expose.min_port` | `int` | Floor for auto-exposed ports. Ports below this in `forwardPorts` are silently dropped. |

These fields also live in ahjo's global `~/.ahjo/config.toml` (under `[mac]` / `[linux]` per platform-namespacing convention); per-repo `customizations.ahjo` overrides global. Features that declare their own `customizations.ahjo` are merged per the spec's customizations rules.

### What disappears

| Removed                                            | Location                                          |
|----------------------------------------------------|---------------------------------------------------|
| `.ahjoconfig` TOML schema and parser               | `internal/ahjoconfig/` — entire package deleted   |
| `.ahjoconfig` `run` as the primary lifecycle hook  | merged into `postCreateCommand`                   |
| `.ahjoconfig` `forward_env` / `auto_expose` fields | moved to `customizations.ahjo` inside `devcontainer.json` |

The TOML parser is deleted, not preserved as an alias. Presence of a legacy `.ahjoconfig` triggers an explicit migration error.

### What changes shape

| File                                                | Change                                                                                                              |
|-----------------------------------------------------|---------------------------------------------------------------------------------------------------------------------|
| `internal/devcontainer/config.go` (new)             | Lax JSONC parser (`tailscale/hujson` with comments + trailing commas), spec-precedence file lookup, `Config` struct with the honored subset (incl. `Customizations.Ahjo`), validation/rejection of disallowed fields |
| `internal/devcontainer/lifecycle.go` (new)          | `RunOnCreate`, `RunPostCreate`, `RunPostStart`, `RunPostAttach` thin wrappers around `incus exec`; supports string/array/object forms |
| `internal/ahjoconfig/`                              | **Deleted.** Remaining `forward_env` / `auto_expose` consumers re-route to `cfg.Customizations.Ahjo` from the devcontainer parse |
| `internal/cli/repo.go:repoAddSetup()`               | Insert: legacy `.ahjoconfig` check (error) → devcontainer parse → reject Docker-only fields → trust prompt → features apply → lifecycle commands (onCreate, postCreate) |
| `internal/cli/shell.go:prepareBranchContainer()`    | Run `postStartCommand` if cached config has it                                                                       |
| `internal/cli/claude.go`, other attach paths        | `postAttachCommand` hook                                                                                             |
| `internal/registry/registry.go`                     | Add `FeatureConsent map[string]bool` per `Repo` (key = glob pattern, e.g. `ghcr.io/foo/*`)                          |
| `README.md`                                         | `.devcontainer/devcontainer.json` example; honored / rejected / ignored field tables; `customizations.ahjo` schema; `.ahjoconfig` migration note |
| `CONTAINER-ISOLATION.md`                            | Document Features run with internet access during repo-add; trust-prompt policy                                       |

### Lifecycle hazards

1. **First non-curated Feature source = blocking prompt.** Persist consent so re-adds don't re-prompt. Skip prompt for `ghcr.io/devcontainers/features/*` (curated upstream); other glob patterns require one-time user confirmation.
2. **Features install during `repo add`.** Captured by `incus copy` for branch containers via btrfs CoW (one-time cost per repo). Confirm reflinks survive the install layer.
3. **Legacy `.ahjoconfig` presence.** If a repo has `.ahjoconfig`, ahjo errors with a migration message. No silent merging, no fallback — users self-migrate.
4. **`remoteUser` mismatch.** If devcontainer.json declares `remoteUser: vscode` (common in Codespaces-targeting repos), warn and use `ubuntu` anyway. Fail loudly if mismatched — silently switching users breaks `git config` / keys.
5. **Docker-only fields in user repos.** Many existing `devcontainer.json` files (especially Codespaces-targeting) declare `image:` / `build:` / `mounts:`. The reject-with-explicit-error must point to a doc page explaining the subset and the Phase 4 escape hatch — otherwise users assume ahjo is broken.
6. **Mirror still in flight.** Phase 2 of this doc must wait for `in-container-mirror.md` Phase 2 to stabilize. Don't compound regressions.

### Open questions

1. **Where to cache the resolved devcontainer config?** For `postStartCommand`, we need it at branch-container start time. Lean: re-parse on each start (one extra `incus exec ... cat`), always fresh, no cache invalidation problem. Cost is one shell exec per shell start; cheap.

(`forwardPorts` → `auto_expose` mapping and `containerEnv` vs `customizations.ahjo.forward_env` separation are documented decisions in the mapping table; no longer open.)

## Phase 3 — Drop COI

### What ahjo does

After Phase 1 builds `ahjo-base` without COI, COI's runtime contribution to ahjo is gone. Audit for residual usage:

- Image management: replaced by direct `incus image copy` and the Feature pipeline.
- Container lifecycle: ahjo already drives this via `internal/incus/incus.go`.
- `internal/coi/template.go` and the `coi.RenderConfig` callers: already retired in `no-more-worktrees.md` Phase 1.
- Anything that survives Phase 1: refactor to direct Incus calls.

If clean, delete `internal/coi/` entirely.

### What disappears

| Removed                                  | Location                                                          |
|------------------------------------------|-------------------------------------------------------------------|
| Entire COI integration                   | `internal/coi/`                                                   |
| COI install step in init flow            | `internal/cli/init.go` (the section ensuring COI is at `v0.8.1`)  |
| COI version pin                          | `internal/coi/coi.go:39`                                          |
| COI Lima auto-detect workaround docs     | reduce/inline; no longer relevant                                 |
| Any `coi` binary references              | repo-wide grep                                                    |

### What changes shape

| File                                                | Change                                                                                          |
|-----------------------------------------------------|-------------------------------------------------------------------------------------------------|
| `internal/cli/init.go`                              | Remove COI install/update; init now ensures Incus + Lima only                                    |
| `internal/cli/update.go`                            | Remove COI rebuild step; only rebuild `ahjo-base` via the Phase 1 pipeline                       |
| `internal/cli/nuke.go`                              | Remove COI artifacts from the teardown list                                                     |
| `README.md`, `CLAUDE-SETTING.md`, install docs      | Drop COI references; `CLAUDE-SETTING.md`'s "COI overwrites build.sh edits" warning becomes stale and is removed |

### Lifecycle hazards

1. **Existing installs have COI installed.** Harmless — the binary just sits there unused. `ahjo nuke` removes it as part of teardown; otherwise a manual `apt remove` works.
2. **CLAUDE-SETTING.md.** Today's "COI copies host→container and overwrites build.sh edits" warning becomes stale. Rewrite to describe Feature-based image building; the host-side `hasCompletedOnboarding` wiring stays unchanged (it edits the host VM's `~/.claude.json`, not a build artifact).
3. **Rolling-current toolchains.** With COI gone, `ahjo-runtime` is the only place tools could be pinned. Keep using upstream Feature defaults (no `version:` pins) to preserve rolling-current behavior.

### Open questions

1. **Anything in `internal/coi/` still imported elsewhere?** Grep before deleting; expect zero, but verify.

(Claude bundling resolved: `ahjo-runtime` includes Claude prep. Splitting into a separate `ahjo-claude` Feature is deferred until a concrete opt-out need materializes.)

## Phase 4 (deferred, demand-driven) — Honor repo-supplied images

Allow `image:` and `build:` in repo `devcontainer.json`. Skipped here to keep scope tight.

The hard part isn't Feature application — that's already built in Phases 1–2. The hard part is bridging Docker image semantics to Incus system containers. Sketch of options when demand materializes:

- **Distrobuilder repack.** Add distrobuilder as a host dep. For a repo declaring `image: foo/bar`, run distrobuilder to produce an Incus system-container image from the OCI rootfs (installs systemd, ensures `/sbin/init`). Apply `ahjo-runtime` + repo Features on top.
- **Drop-systemd opt-in.** Honor `image:` as-is; accept the resulting container is an Incus app container with no systemd. `ahjo-runtime` Feature runs sshd as a foreground process under Incus's fallback init. Real topology shift — the repo opts into it; default `ahjo-base` path stays system-container.
- **Document non-support.** `image:` / `build:` permanently rejected; `ahjo-base` is the only base. Acceptable if the curated `ahjo-runtime` Feature plus user-declared Features cover all real demand.

The right answer is "wait for a concrete repo that needs it" before deciding.

## What's deliberately omitted

- **`mcr.microsoft.com/devcontainers/base:ubuntu` as ahjo's base.** Considered and rejected. It's an OCI app-container image (no `/sbin/init`, no CMD, `FROM buildpack-deps:noble-curl`). Importing into Incus produces `CONTAINER (APP)` where Incus's incusd is PID 1; sshd-as-a-service doesn't fit the model. Forcing it would mean either distrobuilder repack, dropping systemd, or reimplementing Docker semantics on Incus. We use upstream `images:ubuntu/24.04` (system container with systemd) instead.
- **`@devcontainers/cli` as a build driver.** Adds Node + Docker assumptions. ahjo runs Features directly via `incus exec`.
- **VS Code `customizations.vscode.*` and other tools' customizations.** Not relevant; ignored without warning. Only `customizations.ahjo.*` is read.
- **`initializeCommand`.** Spec defines it as host-side; under ahjo this means "on the Lima VM," which is awkward. Defer until demand.
- **`updateContentCommand`.** No ahjo "refresh content" hook today; ignore until demand.
- **`waitFor`.** ahjo runs lifecycle commands sequentially; the spec's wait semantics are implicit.
- **`portsAttributes`, `hostRequirements`, `remoteEnv`.** Tool-side or out-of-scope; ignored without warning.
- **`dockerComposeFile`, `mounts`, `runArgs`, `secrets`.** Docker-flavored or security-sensitive; reject with clear error rather than silent ignore.
- **`.ahjoconfig` parsing or migration code.** The TOML parser is deleted, not preserved as an alias. Presence of a legacy file errors with a migration link. Per ahjo dev-mode convention, users self-migrate.
- **Username change to `vscode`.** The previous draft adopted `vscode` because the Microsoft base ships it. With `images:ubuntu/24.04` as the base, ahjo adopts that image's canonical `ubuntu` user instead — see Phase 0 locked-in decisions. Today's bespoke `code` user is retired (parallel name for the same role; canonical wins per the reuse-canonical-names rule). Features are user-agnostic by design (read `_REMOTE_USER` from env), so no Feature-internal duplication of the username.
- **`ahjo-claude` as a separate Feature.** Phase 1 bundles Claude prep into `ahjo-runtime`. Splitting is deferred until a concrete opt-out need materializes — premature otherwise.
- **A separate `ahjo-invariants.md`** capturing implicit contracts in the kept code (SSH wiring, raw.idmap, branch CoW, registry, mirror). Independently valuable but not gating; written incrementally as each phase touches the relevant subsystem.
