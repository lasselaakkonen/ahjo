# ahjo

Isolated local containers with git tooling for **safe and easy yoloing** with Claude Code on macOS and Linux.

Blazing fast firing up of containers per feature branch.

- **Local Incus containers** running Ubuntu on both macOS and Linux hosts
- **Safely let Claude run Docker and any other tooling** inside the container without risking Claude escaping the container
- **Near-instant start up of new containers** for a 'container per feature branch with multiple features in development at once' workflow
- **SSH/AWS/etc secrets completely isolated** from the containers, minimally only a repo scoped GitHub PAT is exposed to whatever is running in the container

## Quick start

#### 1. Installation

Mac and Linux x86_64/arm64 possibly supported, only Mac arm64 tested :)

```sh
# Installs `ahjo` binary
curl -fsSL https://raw.githubusercontent.com/lasselaakkonen/ahjo/master/install.sh | sh

# Checks prerequisites are installed
# On macOS: Sets up Lima and creates 'ahjo' VM
ahjo init

# Validate installation
ahjo doctor
```

#### 2. Run Claude Code

##### In a container through CLI

This relies on automagicism in `ahjo claude`, which assumes you are using GitHub and is triggered when `ahjo repo add <repo>` and `ahjo create <repo> <branch>` have not yet been run:

```
# e.g. ahjo claude lasselaakkonen/ahjo@readme-quick-start --container-config node
# Or replace 'node' with other built-in base toolsets: 'python', 'go', 'rust'
# Later you will define your own, but you can use those built-in ones now.
ahjo claude <account>/<repo>@<branch> --container-config node
```

- **Creates base container for repo** without any specific tech stack support -- one time step, takes a few minutes. ⚠️  Copies `CLAUDE.md`, `settings.json`, `.claude.json`, `agents/`, `commands/`, `skills/`, `rules/` from `~/.claude` to the container, which moves them over the isolation boundary.
- **Asks for you to create a fine grained PAT for GitHub** -- the containers for that repo will have access ONLY to that repo
- **Asks you which tech stack you want** -- these can be configured extensively, but the prompt let's you set a basic set of tooling in to the container
- **Creates a feature container** -- takes only seconds, won't have your project tech stack or tooling in it

##### In a container through TUI

```
ahjo
```

1. Add repo
2. Add container
3. Press `a` to open your agent, only one available for now is Claude Code

#### 3. Domain concepts

**Ahjo base image** is

- Created by `ahjo init`
- Updated with `ahjo update`
- Configured to include:
  - [common-utils](https://github.com/devcontainers/features/tree/main/src/common-utils) devcontainer Feature (provides `jq`, `curl`, `unzip`, `gnupg`, `ca-certificates`, UID-1000 `ubuntu` user with sudo, en_US locale, and a bunch of other base CLI utilities)
  - [git](https://github.com/devcontainers/features/tree/main/src/git) devcontainer Feature (provides `git`)
  - [github-cli](https://github.com/devcontainers/features/tree/main/src/github-cli) devcontainer Feature (provides `gh`)
  - `claude`, plus sshd-as-a-service and the `ahjo-mirror` daemon from [install.sh](internal/ahjoruntime/feature/install.sh)
  - `rg`, `fd`, `eza`, `httpie`, `make`, `yq`, `ast-grep`, `rtk` from [install.sh](internal/ahjodevtools/feature/install.sh)

Language toolchains (Node, Python, Go, Rust, …) are NOT in the base image. They come from either your repo's `.ahjo/ahjocontainer.json` or `--stack=<name>` at repo-add time (see "Built-in stacks" above).


**Repo base container** is

- Intended as a long lived container, as a golden image for **feature containers**
- Not intended as a development environment in most workflows
- Created by `ahjo repo add <repo>`  
- Configured with `.ahjo/ahjocontainer.json` from your `origin/<default branch>`, this is where you install tooling needed for developing, running and testing your app, by default no additional configuration is done.
- All **feature containers** are created as copies of this container, so **feature containers** do not need to spend time installing tooling -or- fetching the repo -or- installing dependencies. 

**Feature container** is

- Intended as a short lived container, for the lifetime of a feature branch
- Intended as a development environment
- Created by `ahjo create <repo> <branch>`
- Configured already in the **repo base container**, since a new **feature container** is just a copy of the **repo base container**

## Use cases

Examples use the CLI for easier presentation of the steps, but TUI might be easier to use in practice, open TUI with plain `ahjo` command.

### Work on two PRs simultaneously in a single repo

Add repo once, which creates the repo base container:

```
ahjo repo add myacc/myrepo
```

Start work on first feature:

```
# `ahjo create` is optional
# `ahjo claude` creates the container if it does not exist already
# `ahjo create` sanitizes feature container names
ahjo create myacc/repo "JIRA-123 Add thingamajig"

ahjo claude myacc/repo@JIRA-123-Add-thingamajig

# ... let Claude loose in the container ...
```

Start work on the second feature:

```
ahjo claude myacc/repo feat/twiddle-with-ui

# ... let Claude loose in the container ...
```

After you exit the Claude sessions, if the git dir is clean and PR is merged, ahjo will ask you if you want to remove the containers. Otherwise you can remove them later yourself:

```
# Find the container you want to remove
ahjo ls

# Remove it
ahjo rm myacc/repo@JIRA-123-Add-thingamajig
ahjo rm myacc/repo@feat/twiddle-with-ui
```

### Modify code in container, mirror code changes to host

You haven't yet configured ahjo containers to run your app -or- setting up the dev env is complex -or- you need some services/data from your host machine for properly running the app -or- you need to build iOS apps and can't do it in the Linux container -or- whatever else.

You can mirror the changes from inside the repo to a dir on the host machine. You likely want to mirror the changes to a dir, which has the same git repo in it already.

Mirroring replicates ONLY created and changed files, it DOES NOT replicate deletions.

```
ahjo create myacc/myrepo@newfangled-thing
ahjo mirror myacc/myrepo@newfangled-thing --target /Users/lasse/github/myrepo
```

Now any changed files in `myacc/myrepo@newfangled-thing` will show up in `/Users/lasse/github/myrepo`.

To turn off mirroring, run:

```
ahjo mirror off
```

⚠️ For now ahjo DOES NOT do any clean up in `/Users/lasse/github/myrepo`, you need to do it yourself, perhaps with just `git checkout .`.

### View multiple versions of same app running in multiple containers

Create feature containers for each branch:

```
ahjo create myacc/myrepo feat-1
ahjo create myacc/myrepo feat-2
```

Then either `ahjo claude ...` and ask Claude to start the web app or `ahjo shell ...` and do it yourself.

Now you will have same port exposed inside both containers.

Then expose ports from the containers to your host:

```
ahjo expose myacc/myrepo@feat-1
ahjo expose myacc/myrepo@feat-2
```

### From a monorepo, run two different tech stacks in two different containers

Tech stacks are per **repository base container** in ahjo and automatic aliases are based on only account name + repo name, so you need to set custom aliases with `--as` and define which container config to use with `--container-config`.

`--container-config backendcontainer` expects to find `.ahjo/backendcontainer.json` in the repo.

```
ahjo repo add myacc/monorepoapp --as monorepoapp-backend --container-config backendcontainer
ahjo repo add myacc/monorepoapp --as monorepoapp-frontend --container-config frontendcontainer
```

Now you have two containers running with different tech stacks, eg:
- `monorepo-backend@main` using your backend tech stack
- `monorepo-frontend@main` using your backend tech stack

```
# Create feature containers
ahjo create monorepoapp-backend new-feat-x-apis
ahjo create monorepoapp-frontend new-feat-x-ui

# Launch claude sessions in both
ahjo claude monorepoapp-backend@new-feat-x-apis
ahjo claude monorepoapp-frontend@new-feat-x-ui
```

### Refresh dependencies in repo base container

⚠️ For now ahjo DOES NOT manage updating the repo base container for you.

You create the repo base container earlier, but now the dependencies for the project have changed. Creating new containers works, but each container has to always itself fetch and install the new dependencies.

Log in to the repo base container:

```
ahjo shell myacc/myrepo@main
```

Inside the container do whatever you need, eg `pnpm i`.

Now every feature container created for the repo will have the updated node modules ready in them immediately after creation.

### You do not want feature branches to branch off the default branch

By default this will branch the feature container from `origin/<default branch>`, so typically `origin/main`:

```
ahjo repo add myacc/myrepo
ahjo create myacc/myrepo feat-1
# -> container with `feat-1` branch branched off `origin/main`
```

To have all feature branches in `myrepo` branch off `develop` branch:

```
ahjo repo add myacc/myrepo --default-base develop
ahjo create myacc/myrepo feat-1
# -> container with `feat-1` branch branched off `origin/develop`
```

## Container tech stack setup

#### `.ahjo/ahjocontainer.json`

Primarily ahjo tries to read `.ahjo/ahjocontainer.json` from the default branch from remote and uses it to configure your repo base container.

`ahjocontainer.json` schema is a subset of devcontainers schema, for a Go project like ahjo, you might define it as:

```
{
  "name": "ahjo",
  "features": {
    "ghcr.io/devcontainers/features/go:1": {}
  },
  "postCreateCommand": "make hooks"
}
```

#### How ahjo picks the container configPicking a container config

When the repo carries no `.ahjo/ahjocontainer.json` — or you want to override the one it ships — pass `--container-config=<value>` to `ahjo repo add` or `ahjo claude`. Resolution order (first match wins):

1. **Explicit `--container-config <value>`** — overrides everything below.
2. **`.ahjo/ahjocontainer.json` in the repo** if present.
3. **Interactive picker** on a TTY (offers bare + any `.ahjo/*.json` the repo ships + the bundled stacks).
4. **Bare** (no toolchain beyond ahjo-base), used as the non-TTY fallback.

`--container-config <value>` accepts:

- A **bundled stack name**: `node`, `python`, `go`, `rust`. Each is a curated `ahjocontainer.json` shipped inside the ahjo binary — view the source under [internal/stacks/](internal/stacks/).
- A **repo-local basename**, resolved against `.ahjo/<value>.json` in the repo. Lets a repo offer multiple variants (`.ahjo/lite.json`, `.ahjo/ci.json`, …) alongside the canonical one.
- An **absolute or relative path** to a `.json` file on the host. Resolved against the directory you ran ahjo from. On macOS, paths outside the home directory (e.g. `/tmp/foo.json`) are transparently staged into the Lima VM through the shared dir — you don't need to move the file into `~/`.
- The literal `bare` to opt out of any container config (same as the picker's bare option).

Examples:

```
ahjo repo add myacc/some-go-repo --container-config=go
ahjo claude myacc/some-node-repo@main --container-config=node
ahjo repo add myacc/myrepo --container-config=ci         # uses .ahjo/ci.json from the repo
ahjo repo add myacc/myrepo --container-config=/abs/path/cfg.json
```

Nothing is written to the repo; the chosen config is applied to that repo's base container only. The choice persists in the repo base container until `ahjo repo rm` clears it.

## Git / GitHub auth

ahjo supports two GitHub auth paths:

- **Fine-grained PAT**: repo-scoped, forwarded as `GH_TOKEN`, used by `gh` and by HTTPS git through `gh auth setup-git`. This is the recommended least-privilege path.
- **SSH agent forwarding**: used only for `git@...` remotes. ahjo forwards the host agent socket; it does not copy keys or scope keys per container.

Scenarios:

- **HTTPS remote + fine-grained PAT**: best default. `git fetch/push` and `gh` both work with repo-scoped access.
- **HTTPS remote + no PAT**: public read may work; private repos, push, and `gh` auth fail.
- **SSH remote + fine-grained PAT + working SSH agent**: raw `git` uses SSH agent; `gh` uses PAT. Both work, but git access follows SSH key scope.
- **SSH remote + no PAT + working SSH agent**: raw `git` works; `gh` does not. Access follows whatever keys the forwarded agent exposes.
- **SSH remote + broken/missing SSH agent**: `ahjo repo add git@...` and later git operations fail, even if a PAT exists, because ahjo does not rewrite SSH remotes to HTTPS. The "working SSH agent" prerequisite is set up automatically by `ahjo init` (see [First run](#first-run)).

Unsupported auth methods:

- **GitHub Deploy Key** support is not built in. Deploy Keys would work only with `git`, Fine-grained PATs can be scoped to repos and they work with `git` and `gh`.

## Installing

One line, any supported platform (macOS x86_64/arm64, Linux x86_64/arm64) — detects your OS/arch, pulls the matching binary from the [latest release](https://github.com/lasselaakkonen/ahjo/releases/latest), verifies it against the release's `SHA256SUMS`:

```sh
curl -fsSL https://raw.githubusercontent.com/lasselaakkonen/ahjo/master/install.sh | sh
```

Pin a specific tag with `AHJO_VERSION=v0.0.1`, or install somewhere other than `/usr/local/bin` with `INSTALL_DIR="$HOME/.local/bin"`.

Or build from source:

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

On macOS, `ahjo init` auto-detects the host ssh-agent (1Password, Secretive, gpg-agent, or whatever `IdentityAgent` in `~/.ssh/config` points at), persists the chosen socket to `~/.ahjo/config.toml`, and overrides `SSH_AUTH_SOCK` on every `limactl` invocation — no shellrc edits required. If no agent with keys is reachable, the init step errors out: load a key into your agent and re-run `ahjo init`. `ahjo doctor` verifies this end-to-end. See [CONTAINER-ISOLATION.md](CONTAINER-ISOLATION.md#the-ssh-agent-hole) for why agent forwarding (not key copying) is the model.

On macOS the same command does both the host setup (Homebrew check → `brew install lima` → `limactl start`) and the in-VM bring-up (Zabbly + Incus, `incus admin init`, build the `ahjo-base` image by applying the embedded `ahjo-runtime` devcontainer Feature on top of `images:ubuntu/24.04`, `claude setup-token`). It pulls the matching `ahjo-linux-<arch>` from the GitHub release that built your host binary, verifies it against `SHA256SUMS`, drops it into the VM at `/usr/local/bin/ahjo`, and drives the rest by relaying through `limactl shell`. No second invocation, no shelling into the VM.

On Linux there's no VM — `ahjo init` runs the bring-up directly. After `usermod -aG incus-admin` it re-execs itself under `sg incus-admin` so the new group activates without a re-shell, then continues with the `ahjo-base` build and `claude setup-token` in the same pass.

`claude setup-token` requires the `claude` CLI on PATH inside the VM. ahjo will not auto-install it — if it's missing the step fails with install instructions. The resulting `sk-ant-oat01-…` token is saved to `~/.ahjo/.env` (mode 0600) and loaded automatically on every ahjo invocation, so containers receive it via the `forward_env` mechanism (applied with `incus exec --env`) without any shellrc edits.

## Use case example

You are reviewing two PRs against `acme-api` and prototyping a feature on `acme-web`, and you want each in its own clean container so they cannot collide on ports, dependencies, or `node_modules`.

```sh
# 1. Register the repos. ahjo bare-clones them into ~/.ahjo/repos/ and
#    auto-aliases each by <owner>/<repo>. Use --as to add a friendlier alias.
ahjo repo add git@github.com:acme/api.git           # alias: acme/api
#   → after clone, ahjo asks for a GitHub PAT to forward into containers
#     for this repo via $GH_TOKEN. Prefer a fine-grained PAT scoped to
#     JUST this repo (Contents + PRs + Issues + Metadata):
#         https://github.com/settings/personal-access-tokens/new
#     Press Enter to skip; set later with `ahjo repo set-token acme/api`.
#
#     Auth model in containers:
#       - SSH remotes (git@…) — host's ssh-agent forwarded into the
#         container; PAT is irrelevant for plain `git`.
#       - HTTPS remotes (https://…) — `gh auth setup-git` wires git's
#         HTTPS credential helper to read the per-repo PAT, so raw
#         `git clone/fetch/push/pull` works without per-call env juggling.
#       - `gh` itself — uses the per-repo PAT either way.
#     ahjo never auto-rewrites SSH ↔ HTTPS.
ahjo repo add git@github.com:acme/web.git \
  --default-base develop --as web                   # aliases: acme/web, web

# 2. Spin up a worktree per branch. Each one gets its own container,
#    its own SSH port, its own host keys. Auto alias is <repo-alias>@<branch>;
#    --as adds a second one.
ahjo create acme/api pr-482-rate-limit                 # alias: acme/api@pr-482-rate-limit
ahjo create acme/api pr-491-token-refresh              # alias: acme/api@pr-491-token-refresh
ahjo create web feat/checkout-redesign --as checkout   # aliases: acme/web@feat/checkout-redesign, checkout

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
| `ahjo repo set-token <alias>` | Set/rotate the GitHub PAT forwarded into containers for one repo. Hidden-input prompt; stored at `~/.ahjo/repo-env/<slug>.env` (mode 0600). Use a fine-grained PAT scoped to the repo so autonomous agents can't reach anything else. |
| `ahjo env set KEY [VALUE]` / `get` / `unset` / `list [--show]` | Read/write `~/.ahjo/.env`. Keys listed in `forward_env` (default: `CLAUDE_CODE_OAUTH_TOKEN`, `GH_TOKEN`) are forwarded into every container. Omit `VALUE` to prompt with hidden input. Per-repo `.env` (via `repo set-token`) takes precedence over the global file. |
| `ahjo create <repo-alias> <branch> [--as <alias>] [--base <ref>] [--no-fetch]` | Create a COW branch container by copying the repo's default container (`incus copy`) and checking out `<branch>` inside it. Auto alias is `<repo-primary-alias>@<branch>`; `--as` adds a second alias. Idempotent. |
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

A repo can override either field via its `.ahjo/ahjocontainer.json`
(per-repo, see "Per-repo config" below). When enabled, ahjo runs `ss -tlnH`
inside the container at `ahjo shell` start and on `ahjo expose --sync`, then
ensures one `ahjo-auto-<port>` Incus proxy device per qualifying listener
(allocating Mac-side host ports from the same `port_range` as `ahjo expose`).
Listeners that disappear get their proxy devices removed and their host
ports freed; manual `ahjo expose` entries are never touched.

## Per-repo config (ahjocontainer.json)

ahjo reads `.ahjo/ahjocontainer.json` from each repo. The schema is the
runtime-neutral subset of the [devcontainers.dev
spec](https://containers.dev/implementors/json_reference/); ahjo owns its
own file path so IDE / Codespaces / JetBrains Gateway toolchains don't try
to launch their own Docker-based flow against an ahjo-managed repo. Lax
JSONC: `//` and `/* */` comments and trailing commas are accepted.

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

## Rebuilding after a change

ahjo has three state layers: the host binary, the `ahjo-base` Incus image, and the live containers (each branch container holds its repo's `.ahjo/ahjocontainer.json`). Three commands cover everything — pick the smallest one that covers your change.

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
- **Inside an ahjo container**: nothing to do — `.ahjo/ahjocontainer.json` installs Go and golangci-lint on container create via the upstream Feature and `postCreateCommand`.

Bypass when you need to: `SKIP_HOOKS=1 git commit ...` (graceful, prints a notice) or `git commit --no-verify` (hard skip).

