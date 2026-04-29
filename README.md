# ahjo

Sandboxed Claude Code branches on Incus, one container per `(repo, branch)`. On macOS, ahjo runs inside a Lima VM and a thin host-side shim relays every command into it, so the CLI feels native on either side.

Each worktree gets its own container, its own SSH port on `127.0.0.1`, and its own host keys. Open a Mac terminal, `ahjo ssh some-repo my-branch`, and you are in an isolated Linux box with the branch checked out and Claude Code's OAuth token forwarded in.

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

On macOS the same command does both the host setup (Homebrew check → `brew install lima` → `limactl start`) and the in-VM bring-up (Zabbly + Incus, `incus admin init`, COI install, `coi-default` build, `ahjo-base` image, `claude setup-token`). It pulls the matching `ahjo-linux-<arch>` from the GitHub release that built your host binary, verifies it against `SHA256SUMS`, drops it into the VM at `/usr/local/bin/ahjo`, and drives the rest by relaying through `limactl shell`. No second invocation, no shelling into the VM.

On Linux there's no VM — `ahjo init` runs the bring-up directly. After `usermod -aG incus-admin` it re-execs itself under `sg incus-admin` so the new group activates without a re-shell, then continues to COI, `ahjo-base`, and `claude setup-token` in the same pass.

ahjo detects whether it's running inside a Lima VM (via `/mnt/lima-cidata/lima.env`) and tunes the COI install accordingly. Under Lima the VM is already firewalled by macOS/vzNAT, so init disables ufw and runs COI's installer non-interactively, then sets COI's network mode to `open`. On bare-metal Linux it runs COI's installer interactively — you pick ufw vs firewalld and pre-built vs source — and leaves COI's network mode at the installer's default.

`claude setup-token` requires the `claude` CLI on PATH inside the VM. ahjo will not auto-install it — if it's missing the step fails with install instructions. The resulting `sk-ant-oat01-…` token is saved to `~/.ahjo/.env` (mode 0600) and loaded automatically on every ahjo invocation, so containers receive it via COI's `forward_env` without any shellrc edits.

### Verify

```sh
ahjo doctor             # green check on everything
```

## Use case example

You are reviewing two PRs against `acme-api` and prototyping a feature on `acme-web`, and you want each in its own clean container so they cannot collide on ports, dependencies, or `node_modules`.

```sh
# 1. Register the repos. ahjo bare-clones them into ~/.ahjo/repos/.
ahjo repo add acme-api git@github.com:acme/api.git
ahjo repo add acme-web git@github.com:acme/web.git --default-base develop

# 2. Spin up a worktree per branch. Each one gets its own container,
#    its own SSH port, its own host keys.
ahjo new acme-api pr-482-rate-limit
ahjo new acme-api pr-491-token-refresh
ahjo new acme-web feat/checkout-redesign

# 3. Drop into the first one. Container starts on demand.
ahjo shell acme-api pr-482-rate-limit
#   ... cd into the worktree, run `claude`, work normally ...

# 4. From the Mac, ssh straight in (e.g. for VS Code Remote-SSH):
ahjo ssh acme-web feat/checkout-redesign

# 5. Forward the dev server out of the container so you can hit it
#    from a Mac browser:
ahjo expose acme-web feat/checkout-redesign 3000
#   -> container :3000 -> 127.0.0.1:10042

# 6. See what is running.
ahjo ls

# 7. Done with a branch. Tear down the container, the worktree,
#    and free the ports.
ahjo rm acme-api pr-482-rate-limit

# 8. Sweep up anything older than a week.
ahjo gc --older-than 168h --prune
```

State lives under `~/.ahjo/` (registry, ports, host keys, profiles). The Mac shim reads `~/.ahjo-shared/ssh-config` on the host so `ahjo ssh` works without entering the VM first.

## Commands

| Command | What it does |
| --- | --- |
| `ahjo init [-y]` | One-time setup. Mac: Lima + VM, then drop `ahjo-linux-<arch>` into the VM and relay the in-VM bring-up. In VM (or directly on Linux): Incus + COI + `ahjo-base` image + `~/.ahjo/` skeleton. Resumable. |
| `ahjo doctor` | Read-only host check. Reports anything `init` would fix. |
| `ahjo repo add <name> <git-url> [--default-base <branch>]` | Register a repo and bare-clone it under `~/.ahjo/repos/`. |
| `ahjo repo ls` | List registered repos. |
| `ahjo repo rm <name> [--force]` | Drop a repo from the registry. Refuses if worktrees still exist. |
| `ahjo new <repo> <branch> [--base <ref>] [--no-fetch]` | Create the worktree and render `.coi/config.toml`. Idempotent. |
| `ahjo shell <repo> <branch>` | Start the container if needed, wire SSH proxy + sshd, attach via `coi shell`. |
| `ahjo ssh <repo> <branch>` | `exec ssh` into the container using the generated ssh-config (Mac-side or in-VM). |
| `ahjo expose <repo> <branch> <container-port>` | Add an Incus proxy device exposing a container port on `127.0.0.1`. |
| `ahjo ls` | Worktrees with slug, SSH port, container state, creation time. |
| `ahjo rm <repo> <branch>` | Stop + delete the container, remove the worktree, free ports, drop the registry entry. |
| `ahjo gc [--older-than DUR] [--prune] [--dry-run]` | Report (and optionally remove) stale worktrees. Defaults to dry-run. |
| `ahjo nuke [-y]` | Tear down everything `init` built so it can be rebuilt: containers, `ahjo-base`/`coi-default` images, worktrees, host keys, port allocations. On macOS this also stops + deletes the Lima VM. Keeps `~/.ahjo/{config.toml,profiles,repos}` and registered repos. |
| `ahjo version` | Print the version baked into the binary. |

Global config: `~/.ahjo/config.toml` (optional). See [`internal/config/config.go`](internal/config/config.go) for fields — currently `forward_env` and `port_range`.
