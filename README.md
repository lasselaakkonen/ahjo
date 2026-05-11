# ahjo

Sandboxed Claude Code branches on Incus, one container per `(repo, branch)`. On macOS, ahjo runs inside a Lima VM and a thin host-side shim relays every command into it, so the CLI feels native on either side.

Each worktree gets its own container, its own SSH port on `127.0.0.1`, and its own host keys. Open a Mac terminal, `ahjo ssh acme/api@my-branch`, and you are in an isolated Linux box with the branch checked out and Claude Code's OAuth token forwarded in.

Repos and worktrees are addressed by aliases. A repo gets an auto alias of `<owner>/<repo>` derived from its git URL; a worktree gets `<owner>/<repo>@<branch>`. Pass `--as <alias>` to either `repo add` or `new` to add a second, friendlier alias — every command resolves all aliases uniformly.

## Quick start

### Install

Download the binary for your platform from the [latest release](https://github.com/lasselaakkonen/ahjo/releases/latest):

```sh
# macOS (Apple Silicon)
curl -fsSL -o /usr/local/bin/ahjo \
  https://github.com/lasselaakkonen/ahjo/releases/latest/download/ahjo-darwin-arm64
chmod +x /usr/local/bin/ahjo
```

Pick `darwin-amd64`, `linux-arm64`, or `linux-amd64` to match your host. Or build from source:

```sh
git clone https://github.com/lasselaakkonen/ahjo
cd ahjo && make build
sudo ln -sf "$PWD/ahjo" /usr/local/bin/ahjo
```

`make build` on macOS also drops `dist/ahjo-linux-<arch>` next to `./ahjo` — the in-VM companion. The symlink keeps the binary resolvable from `/usr/local/bin/ahjo` while leaving the companion next to its source, so `ahjo init` finds it locally without hitting GitHub. (Released binaries don't need the companion on disk; they fetch the matching one from the same release tag and verify it against `SHA256SUMS`.)

### First run

```sh
ahjo init
```

That's it. Step-by-step prompts walk you through every install. The flow is resumable: re-running `ahjo init` skips anything already done.

If you use 1Password (or any agent that requires `IdentityAgent` in `~/.ssh/config`), add the following to your shellrc *before* running `ahjo init`, otherwise the agent inside the VM will be empty and `ahjo repo add git@…` will fail:

```sh
export SSH_AUTH_SOCK="$HOME/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"
```

`ahjo doctor` verifies this end-to-end. See [CONTAINER-ISOLATION.md](CONTAINER-ISOLATION.md#the-ssh-agent-hole) for why.

On macOS the same command does both the host setup (Homebrew check → `brew install lima` → `limactl start`) and the in-VM bring-up (Zabbly + Incus, `incus admin init`, build the `ahjo-base` image by applying the embedded `ahjo-runtime` devcontainer Feature on top of `images:ubuntu/24.04`, `claude setup-token`). It pulls the matching `ahjo-linux-<arch>` from the GitHub release that built your host binary, verifies it against `SHA256SUMS`, drops it into the VM at `/usr/local/bin/ahjo`, and drives the rest by relaying through `limactl shell`. No second invocation, no shelling into the VM.

On Linux there's no VM — `ahjo init` runs the bring-up directly. After `usermod -aG incus-admin` it re-execs itself under `sg incus-admin` so the new group activates without a re-shell, then continues with the `ahjo-base` build and `claude setup-token` in the same pass.

`claude setup-token` requires the `claude` CLI on PATH inside the VM. ahjo will not auto-install it — if it's missing the step fails with install instructions. The resulting `sk-ant-oat01-…` token is saved to `~/.ahjo/.env` (mode 0600) and loaded automatically on every ahjo invocation, so containers receive it via the `forward_env` mechanism (applied with `incus exec --env`) without any shellrc edits.

### Verify

```sh
ahjo doctor             # green check on everything
```

## Use case example

You are reviewing two PRs against `acme-api` and prototyping a feature on `acme-web`, and you want each in its own clean container so they cannot collide on ports, dependencies, or `node_modules`.

```sh
# 1. Register the repos. ahjo bare-clones them into ~/.ahjo/repos/ and
#    auto-aliases each by <owner>/<repo>. Use --as to add a friendlier alias.
ahjo repo add git@github.com:acme/api.git           # alias: acme/api
ahjo repo add git@github.com:acme/web.git \
  --default-base develop --as web                   # aliases: acme/web, web

# 2. Spin up a worktree per branch. Each one gets its own container,
#    its own SSH port, its own host keys. Auto alias is <repo-alias>@<branch>;
#    --as adds a second one.
ahjo new acme/api pr-482-rate-limit                 # alias: acme/api@pr-482-rate-limit
ahjo new acme/api pr-491-token-refresh              # alias: acme/api@pr-491-token-refresh
ahjo new web feat/checkout-redesign --as checkout   # aliases: acme/web@feat/checkout-redesign, checkout

# 3. Drop into the first one. Container starts on demand.
#    `ahjo shell` opens an interactive shell; use `ahjo claude` to launch claude.
ahjo shell acme/api@pr-482-rate-limit
#   ... in the container's shell, work normally ...
ahjo claude acme/api@pr-482-rate-limit
#   ... in claude's TUI, with the worktree mounted at /workspace ...

# 4. From the Mac, ssh straight in (e.g. for VS Code Remote-SSH).
#    Any alias works — auto or --as.
ahjo ssh checkout

# 5. Any TCP loopback listener inside the container with port >= 3000 is
#    auto-exposed on 127.0.0.1 of the host (Mac via Lima auto-forward).
#    For pre-existing listeners ahjo wires this up at `ahjo shell` start.
#    For listeners that come up later (e.g. after `docker compose up`),
#    refresh from another terminal:
ahjo expose checkout --sync
#   -> auto-expose: container :3000 -> 127.0.0.1:10042
#      auto-expose: container :5432 -> 127.0.0.1:10043
#
# Or manually pin a single port (no threshold check, allocation persisted):
ahjo expose checkout 3000
#   -> container :3000 -> 127.0.0.1:10042

# 6. See what is running.
ahjo ls

# 7. Done with a branch. Tear down the container, the worktree,
#    and free the ports.
ahjo rm acme/api@pr-482-rate-limit

# 8. Sweep up anything older than a week.
ahjo gc --older-than 168h --prune
```

State lives under `~/.ahjo/` (registry, ports, host keys, profiles). The Mac shim reads `~/.ahjo-shared/ssh-config` on the host so `ahjo ssh` works without entering the VM first.

## Commands

| Command | What it does |
| --- | --- |
| `ahjo init [-y]` | One-time setup. Mac: Lima + VM, then drop `ahjo-linux-<arch>` into the VM and relay the in-VM bring-up. In VM (or directly on Linux): Incus + `ahjo-base` image (built from `images:ubuntu/24.04` by applying the embedded `ahjo-runtime` devcontainer Feature) + `~/.ahjo/` skeleton. Resumable. |
| `ahjo update [-y]` | Refresh in-place. Mac: push the current `ahjo-linux-<arch>` into the VM (no-op if the version already matches). VM: rebuild the `ahjo-base` image by force-replaying the `ahjo-runtime` Feature on top of the local `ahjo-osbase` mirror of upstream Ubuntu. Run after editing the host binary or the embedded Feature. |
| `ahjo doctor` | Read-only host check. Reports anything `init` would fix. |
| `ahjo repo add <git-url> [--as <alias>] [--default-base <branch>]` | Register a repo and bare-clone it under `~/.ahjo/repos/`. Auto alias is `<owner>/<repo>` from the URL; `--as` adds a second alias. On collision (e.g. github vs gitlab `acme/api`), ahjo suffixes `-2`/`-3`/… |
| `ahjo repo ls` | List registered repos with their aliases. |
| `ahjo repo rm <alias> [--force]` | Drop a repo by any of its aliases. Refuses if worktrees still exist. |
| `ahjo new <repo-alias> <branch> [--as <alias>] [--base <ref>] [--no-fetch]` | Create a COW branch container by copying the repo's default container (`incus copy`) and checking out `<branch>` inside it. Auto alias is `<repo-primary-alias>@<branch>`; `--as` adds a second alias. Idempotent. |
| `ahjo shell <alias> [--update]` | Start the container if needed, wire SSH proxy + sshd, attach an interactive bash via `incus exec --force-interactive` as the in-container `ubuntu` user. `--update` shuts down and deletes the existing container first so the next attach builds a fresh one from the current `ahjo-base` image; the host keys, registry entry, and ssh port are preserved. |
| `ahjo claude <alias> [--update]` | Same prep as `ahjo shell`, but launches `claude` inside the container instead of dropping to a shell. |
| `ahjo ssh <alias>` | `exec ssh` into the container using the generated ssh-config (Mac-side or in-VM). |
| `ahjo expose <alias> <container-port>` | Manually add an Incus proxy device exposing a container port on `127.0.0.1`. |
| `ahjo expose <alias> --sync` | Reconcile auto-expose proxy devices to the container's current TCP loopback listeners (skipping `:22` and ports below `[auto_expose].min_port`). Run after starting docker-compose / a dev server inside the container so newly-bound ports surface to the host. Manual `ahjo expose` entries are untouched. |
| `ahjo ls` | Worktrees with aliases, slug, SSH port, container state, creation time. |
| `ahjo rm <alias>` | Stop + delete the container, remove the worktree, free ports, drop the registry entry. |
| `ahjo gc [--older-than DUR] [--prune] [--dry-run]` | Report (and optionally remove) stale worktrees. Defaults to dry-run. |
| `ahjo nuke [-y]` | Tear down everything `init` built so it can be rebuilt: containers, `ahjo-base` + `ahjo-osbase` images (and any leftover `coi-default` from a pre-Phase-1 install), host keys, port allocations. On macOS this also stops + deletes the Lima VM. Keeps `~/.ahjo/{config.toml,profiles}` and registered repos. |
| `ahjo version` | Print the version baked into the binary. |

Global config: `~/.ahjo/config.toml` (optional). See [`internal/config/config.go`](internal/config/config.go) for fields — currently `forward_env`, `port_range`, and `auto_expose`.

The `[auto_expose]` section controls automatic forwarding of container TCP
loopback listeners to the host:

```toml
[auto_expose]
enabled  = true   # default; set false to opt out globally
min_port = 3000   # default; listeners below this are ignored
```

A repo can override either field via its `.devcontainer/devcontainer.json`
(per-repo, see "Per-repo config" below). When enabled, ahjo runs `ss -tlnH`
inside the container at `ahjo shell` start and on `ahjo expose --sync`, then
ensures one `ahjo-auto-<port>` Incus proxy device per qualifying listener
(allocating Mac-side host ports from the same `port_range` as `ahjo expose`).
Listeners that disappear get their proxy devices removed and their host
ports freed; manual `ahjo expose` entries are never touched.

## Per-repo config (devcontainer.json)

ahjo reads `.devcontainer/devcontainer.json` (or `.devcontainer.json`) from
each repo and honors a runtime-neutral subset of the [devcontainers.dev
spec](https://containers.dev/implementors/json_reference/). Lax JSONC: `//`
and `/* */` comments and trailing commas are accepted.

Minimal example:

```jsonc
{
  // Run after `git clone` lands inside the container.
  "postCreateCommand": "pnpm install",

  // Run on every `ahjo shell` / `ahjo claude` start.
  "postStartCommand": "echo container ready",

  // Per-process env visible to `incus exec` calls.
  "containerEnv": { "NODE_ENV": "development" },

  // ahjo's per-repo extension namespace, replacing the retired .ahjoconfig.
  "customizations": {
    "ahjo": {
      "forward_env": ["MY_API_TOKEN"],
      "auto_expose": { "enabled": true, "min_port": 3000 }
    }
  }
}
```

| Field | Status | Behavior |
| --- | --- | --- |
| `onCreateCommand` | honored | Runs at `ahjo repo add` after `git clone`, before `postCreateCommand`, as `ubuntu` in `/repo`. |
| `postCreateCommand` | honored | Same context as `onCreateCommand`; the user-facing one in most repos. |
| `postStartCommand` | honored | Runs every `ahjo shell` / `ahjo claude`, after the container is ready. |
| `postAttachCommand` | honored | Runs the moment ahjo execs into the user's shell. |
| `containerEnv` | honored | Applied via Incus `environment.<KEY>` and merged into the per-exec env. |
| `customizations.ahjo.forward_env` | honored | Appended to global `forward_env`; resolved against the host env per `incus exec`. |
| `customizations.ahjo.auto_expose` | honored | Overrides the global `[auto_expose]` block (per-repo). |
| `forwardPorts` | parsed | Captured for the future allowlist; not yet enforced. |
| `remoteUser` / `containerUser` | warn-only | ahjo runs as `ubuntu`; mismatch is logged and ignored. |
| `image`, `build`, `dockerComposeFile`, `mounts`, `runArgs`, `secrets` | rejected | Docker-flavored or security-sensitive. `ahjo repo add` aborts with an explicit error. |
| `features` | honored | OCI artifacts pulled from the declared registry (anonymous read; `ghcr.io/devcontainers/features/*` is auto-trusted, other source patterns trigger a one-time `[y/N]` prompt). Dep graph resolved from each Feature's `dependsOn` (hard) and `installsAfter` (soft); each `install.sh` runs as root inside the container, options pass through as `ALL_CAPS` env vars. A Feature's own `devcontainer-feature.json` is filtered: `mounts` and `privileged` are hard-rejected (the Feature relies on them at runtime, ignoring would silently break it); `capAdd`, `securityOpt`, `init`, and `entrypoint` are Docker-runtime hints that have no Incus equivalent under ahjo's profile (or are already provided by systemd) — ahjo prints a per-field `warn:` line explaining what was dropped and runs `install.sh` anyway. Known values get specific notes (`SYS_PTRACE` → debugger context; `seccomp=unconfined` → Incus seccomp policy; `label=disable` → no SELinux on ahjo). This is the path that lets curated Features like `go:1` and `rust:1` work — they declare debugger-related caps that don't apply here. |
| `customizations.vscode`, `customizations.codespaces`, etc. | ignored | ahjo isn't a VS Code host; only `customizations.ahjo` is read. |
| `initializeCommand`, `updateContentCommand`, `waitFor`, `portsAttributes`, `hostRequirements`, `remoteEnv` | ignored | No matching ahjo concept; the spec field is silently dropped. |

Lifecycle commands accept the spec's three forms: a string (`"pnpm install"`,
runs via `bash -c`), an array (`["echo", "hi"]`, runs argv directly), or an
object map (`{"a": "...", "b": "..."}`, runs each entry sequentially in
sorted key order). A failed step aborts the chain so half-set-up containers
surface a clear error.

### Migrating from `.ahjoconfig`

The retired TOML schema maps to the devcontainer.json fields above:

| Old `.ahjoconfig` | New `.devcontainer/devcontainer.json` |
| --- | --- |
| `run = ["..."]` | `"postCreateCommand": "..."` (or array form) |
| `forward_env = [...]` | `"customizations": { "ahjo": { "forward_env": [...] } }` |
| `auto_expose.enabled` / `auto_expose.min_port` | `"customizations": { "ahjo": { "auto_expose": { ... } } }` |

Per ahjo's no-runtime-migration convention, ahjo does not parse legacy
`.ahjoconfig` files. `ahjo repo add` fails fast when one is present, with a
pointer to this section. Existing branch containers continue to work but
silently lose their per-repo overrides until you migrate.

## Rebuilding after a change

ahjo has three state layers: the host binary, the `ahjo-base` Incus image, and the live containers (each branch container holds its repo's `.devcontainer/devcontainer.json`). Three commands cover everything — pick the smallest one that covers your change.

| Scenario | Command |
| --- | --- |
| Full reset (wipe everything, rebuild from scratch) | `ahjo nuke -y && ahjo init` |
| Host binary or any embedded asset changed (`internal/ahjoruntime/feature/install.sh`, `ahjo-claude-prepare`, anything under `internal/ahjoruntime/`) | `ahjo update` |
| Existing container should run on the new image | `ahjo shell <alias> --update` |

`ahjo update` is the brew-style "bring everything to current" verb: on macOS it pushes the matching `ahjo-linux-<arch>` into the VM (no-op when versions match) and then runs `ahjo update` inside the VM, which force-rebuilds `ahjo-base` by re-applying the embedded `ahjo-runtime` Feature on top of the local `ahjo-osbase` mirror. On Linux it skips the binary push and goes straight to the rebuild.

`ahjo shell --update` is granular by design — `ahjo update` rebuilds the image but leaves running containers alone, so you can decide per-worktree whether to recreate. The worktree, host keys, registry entry, and ssh port are preserved. Worktrees you don't recreate keep running on the old image until you do.

`ahjo nuke` is for the rare case when state itself is wrong (mismatched aliases, corrupt registry, etc.). For ordinary "I changed the code" iteration, `ahjo update` is what you want.

## Development

Working on ahjo itself. Skip if you just use it.

### Git hooks

Repo-tracked hooks under `.githooks/` gate commits and pushes against the same checks CI runs, so most failures surface locally. Activate once per clone:

```bash
make hooks
```

That points `core.hooksPath` at `.githooks/`. Idempotent; safe to re-run.

| Hook | Runs | Cold time |
| --- | --- | --- |
| `pre-commit` | `gofmt -l`, `go vet`, `golangci-lint`, `go test ./...` | ~5s |
| `pre-push`   | `go generate ./...` freshness check, `go test -race ./...` | ~15s |

`golangci-lint` is soft-skipped if it isn't on PATH so a fresh clone can still commit; install it for the full pre-commit pass:

- **Host (macOS)**: `brew install golangci-lint`
- **Inside an ahjo container**: nothing to do — `.devcontainer/devcontainer.json` installs Go and golangci-lint on container create via the upstream Feature and `postCreateCommand`.

Bypass when you need to: `SKIP_HOOKS=1 git commit ...` (graceful, prints a notice) or `git commit --no-verify` (hard skip).
