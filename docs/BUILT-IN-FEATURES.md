# Built-in Features (`ahjo/<name>`)

ahjo honors the upstream devcontainer Features spec — `features:` keys in `.ahjo/ahjocontainer.json` are normally OCI refs like `ghcr.io/devcontainers/features/node:1`, fetched at `ahjo repo add` time and run inside the container as `install.sh`. **Built-in Features** extend that addressing with an `ahjo/<name>` prefix that resolves to a Feature embedded in the ahjo binary rather than fetched from a registry.

```jsonc
// .ahjo/ahjocontainer.json
{
  "features": {
    "ahjo/docker": { "version": "latest" }
  }
}
```

## Why ship Features in the binary

Some toolchains assume Docker-runtime semantics that ahjo's profile provides differently. The canonical case is Docker: the upstream `docker-in-docker` / `docker-outside-of-docker` Features declare `mounts:` and `privileged: true` in their `devcontainer-feature.json` because they target a Docker runtime, where those are the only way to grant the kernel surface Docker needs. ahjo's runner rejects both fields (`internal/devcontainer/features.go::rejectDockerFields`) — `security.nesting=true`, the mknod/setxattr syscall intercepts, the btrfs rootfs, and systemd-as-PID 1 already provide that surface on an Incus system container, so neither field is needed. But there's no upstream Feature that *omits* them, so the install path is blocked.

The workaround — pasting `postCreateCommand: "curl get.docker.com | sh; …"` into every repo — pushes ahjo-shaped install logic into every repo's config file, can't be versioned with the binary, and gives no per-option surface (storage driver, channel, daemon args). Built-in Features fix that: the install script ships in the same release as the runtime profile it depends on, gets a real options block, and looks like any other Feature in the repo's config.

## Trust posture

`ahjo/*` is auto-trusted under `BuiltinTrustedGlob` — no `[y/N]` prompt, no entry in the Repo's `FeatureConsent` map. A Feature shipped with the binary the user installed has the same trust posture as ahjo itself; the user already consented to running ahjo when they installed it.

The curated upstream namespace (`ghcr.io/devcontainers/features/*`) is auto-trusted for the same reason and the prompt only fires for third-party sources. Built-in Features sit in the same bucket as curated — see `internal/devcontainer/trust.go::PartitionFeatureSources`.

## What's the same as the OCI path

Downstream of source resolution, **everything**. The dispatch only swaps the fetch step:

- `ReadMetadata` runs on the embedded `devcontainer-feature.json` and still rejects `mounts` / `privileged` — the `embed_test.go` for each built-in calls it as a guard, so a typo that adds a forbidden field fails CI before it ships.
- Options flow through `ApplyOptionDefaults` + `NormalizeOptions`, so `{ "version": "latest" }` becomes `VERSION=latest` in `install.sh`'s env.
- `install.sh` runs as root via `incus exec`, with the spec's `_REMOTE_USER` / `_REMOTE_USER_HOME` envelope.
- `containerEnv` (e.g. `DOCKER_BUILDKIT: "1"` for `ahjo/docker`) persists to Incus `environment.*` so every later `incus exec` inherits it.

No built-in Feature declares `dependsOn` today, and the `ahjo/*` path is untested with it — built-in Features that would chain on a curated OCI Feature should ship a self-contained `install.sh` instead. (Mechanically, dependency resolution runs in `Resolve`, which re-parses each `dependsOn` ref through `parseRef` — which falls back to the OCI parser — so an OCI dependency would in principle resolve via the normal fetch path; it simply hasn't been exercised for `ahjo/*` keys.)

## Adding a new built-in Feature

The pattern is one Go package per Feature, mirroring `internal/ahjofeature_docker/`:

```
internal/ahjofeature_<name>/
  embed.go                              # const FeatureID = "<name>"; func Materialize(dst string) error
  embed_test.go
  feature/
    devcontainer-feature.json
    install.sh
```

Then add one line to `internal/ahjofeatures/registry.go`:

```go
var table = map[string]Materializer{
    ahjofeature_docker.FeatureID:   ahjofeature_docker.Materialize,
    ahjofeature_<name>.FeatureID:   ahjofeature_<name>.Materialize,  // new
}
```

That's the whole change — addressing, trust, dispatch, env envelope, and Incus persistence are all reused.

## Existing built-in Features

| Name | What it installs | Options |
| --- | --- | --- |
| `ahjo/docker` | Docker Engine via `get.docker.com` + compose plugin. Leaves `/etc/docker/daemon.json` at dockerd's default (>=26: containerd snapshotter using the overlayfs driver, xattr whiteouts — covered by the profile's `setxattr` intercept). | `version` (default `latest`), `channel` (`stable`), `daemon_args` (JSON fragment merged into daemon.json; if set, dockerd is restarted to pick it up) |
| `ahjo/prek` | [`prek`](https://github.com/j178/prek) (a dependency-free, Rust-based pre-commit reimplementation) installed for the remote user, then warms the hook cache from `/repo/.pre-commit-config.yaml` via `prek prepare-hooks`. Leaves `.git/hooks` untouched — pure warm-up, not a git-hook installer. | none |

## How this relates to `ahjo-runtime` / `ahjo-default-dev-tools`

Those are also binary-embedded Features, but they sit in a different lane: `embeddedBaseFeatures` (`internal/devcontainer/build.go`) is applied at image-build time inside the transient `ahjo-build-*` container to produce `ahjo-base`. They're not addressable from repo config — every `ahjo-base` container already has them. Built-in Features are the **repo-add lane**: opt-in per-repo, addressable as `ahjo/<name>`, applied to the per-(repo, branch) container only when the repo's config asks for them.
