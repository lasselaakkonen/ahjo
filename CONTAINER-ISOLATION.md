# Container isolation

ahjo's job is to give each `(repo, branch)` its own sandbox so an experiment, a `git pull`, an `npm install`, or a Claude action can't reach the host. This doc spells out the boundaries — what they isolate, and the one place they intentionally leak.

## The stack

```
┌──────────────────────────────────────────────────────────────────────┐
│ macOS host                                                           │
│  • shells, 1Password (SSH agent), browser, $HOME                     │
│  • ahjo CLI (thin shim)                                              │
│                                                                      │
│  ┌────────────────────────────────────────────────────────────────┐  │
│  │ Lima VM "ahjo"  (Ubuntu, vz + vzNAT)                           │  │
│  │  • Incus host, COI, ahjo-base image                            │  │
│  │  • bare clones at ~/.ahjo/repos/                               │  │
│  │  • worktrees at ~/.ahjo/worktrees/<slug>/                      │  │
│  │                                                                │  │
│  │  ┌─────────────────────┐  ┌─────────────────────┐  ┌────┐     │  │
│  │  │ Incus container A   │  │ Incus container B   │  │ …  │     │  │
│  │  │ <repo>@<branchA>    │  │ <repo>@<branchB>    │  │    │     │  │
│  │  │ own sshd, own port  │  │ own sshd, own port  │  │    │     │  │
│  │  └─────────────────────┘  └─────────────────────┘  └────┘     │  │
│  └────────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────────┘
```

On Linux hosts the middle layer collapses — Incus runs directly on the host, no Lima.

## What each boundary protects

**Mac → Lima VM.** The VM is a separate kernel under `vz`. Container/Incus state lives only in the VM. Code running in any container can't touch your Mac filesystem, browser session, keychain, or the rest of your home directory. `vzNAT` networking means containers can't dial Bonjour services or `localhost` apps on the Mac. The VM disk is 50 GB — runaway dependencies stop there.

**Lima VM → Incus container.** Each worktree is its own Linux container with its own filesystem, process tree, and user (`code`). Containers cannot see each other. They share only what COI maps in: the worktree directory (read-write), generated SSH host keys (read-only), and the env vars listed in `forward_env` (e.g. `ANTHROPIC_AUTH_TOKEN`). A container has no view of the VM's `~/.ahjo/`, no view of other containers, and no Incus admin privileges.

**Per-worktree state.** Each container has its own `127.0.0.1:<port>` exposed by Incus' proxy device, its own SSH host keys under `~/.ahjo/host-keys/<slug>/`, and its own port allocations in `~/.ahjo/ports.toml`. Two branches of the same repo cannot collide on ports, sockets, host-key fingerprints, or `node_modules`.

## What crosses the boundaries

These are the *intentional* leaks — every other path is closed.

| Path | Direction | Contents |
| --- | --- | --- |
| `~/.ahjo-shared/` (Lima 9p mount) | VM → Mac, read-write | generated `ssh-config` + `aliases` map so `ahjo ssh <alias>` works from the Mac |
| `forward_env` | VM → container | only the names listed in `~/.ahjo/config.toml` (default: `ANTHROPIC_AUTH_TOKEN` from `~/.ahjo/.env`) |
| Worktree dir | VM → container, read-write | the git checkout for that branch — by design, this is what the container is for |
| Host keys dir | VM → container, read-only | per-worktree sshd keys + `authorized_keys` |
| SSH agent socket | Mac → VM → container | **see below** |

Nothing else crosses — no `~/.ssh/`, no `~/.gitconfig`, no shell history, no `~/Library`.

## Workspace UID mapping

Each container runs unprivileged in its own user namespace; Incus assigns it a non-overlapping UID range like `0 1074266112 1000000000`. The worktree on the VM, however, is owned by the Lima user (UID 1000 / GID 1000) — *outside* that range — so without intervention the bind mount surfaces inside the container as `nobody:nogroup` and the in-container `code` user (UID 1000) can only `r-x` it. Even namespace-root via `sudo` can't `chown` away from an unmapped owner.

COI ships its own fix for this — `raw.idmap "both <hostUID> 1000"` in `internal/session/setup.go` — but auto-disables it on Lima/Colima, on the assumption that the workspace path is a Mac directory virtiofs-mounted into the VM and UID translation already happens at the VM-level. ahjo's worktrees live at `~/.ahjo/worktrees/<slug>/` on the VM's local ext4/btrfs, not on virtiofs, so the assumption doesn't hold and we end up with no UID mapping at all. There is no `disable_shift = false` opt-out in the COI version ahjo pins — the override only goes one way.

ahjo therefore applies the mapping itself, with two pieces both wired into `ahjo init` / `ahjo update`:

1. **subuid/subgid grants for the Incus daemon**: a one-time idempotent append of `root:<hostUID>:1` to `/etc/subuid` and `root:<hostGID>:1` to `/etc/subgid` so the daemon (running as root) is allowed to delegate those IDs into a container's userns. Without this, `newuidmap` rejects the mapping at container start. The init/update step restarts the Incus daemon when (and only when) it actually appended a line.

2. **per-container `raw.idmap`**: every container ahjo creates gets `both <hostUID> 1000` set on it (matching COI's own format), mapping the host VM user onto the in-container `code` user. Applied in `internal/cli/shell.go::prepareWorktreeContainer` (first-shell, COI-managed path) and `internal/cli/new.go::runNew` (COW-copy path). Files written inside as `code` land on the VM owned by the Lima user; files owned by the Lima user on the VM appear inside as `code:code`. The boundary — Claude inside can't reach UID 0, can't touch other host files, can't escape devices — is unchanged; we widen the namespace by one user, the one we already share with the worktree by construction.

See the [Incus docs on `raw.idmap`](https://linuxcontainers.org/incus/docs/main/reference/instance_options/#instance-raw) for the kernel-level mechanism.

## The SSH-agent hole

`git clone git@github.com:…` inside a container needs an SSH key. ahjo does **not** copy your host keys into the VM or container. Instead, with `ssh.forwardAgent: true` set on both legs, the agent socket is forwarded:

```
agent on Mac (1Password, ssh-agent, …)  ──►  Lima VM  ──►  Incus container
       (signs)                          ($SSH_AUTH_SOCK on each hop)
```

Inside the container, `ssh` and `git` use `$SSH_AUTH_SOCK`, the request travels back across both hops, and the host agent prompts for unlock. Keys never leave the host agent.

The leak: **anything that runs in the container can ask the agent to sign**, for the lifetime of the shell. A malicious dev dependency, a hostile `git` hook, or an unintended Claude action can authenticate to *any* host your agent has keys for — typically every git remote you use. The host agent's per-key authorization prompts are the only check; once you've clicked "always allow" for a key, it signs silently.

If that's not acceptable for a given session, two mitigations:

- Disable forwarding for one shell: `limactl shell ahjo -- env -u SSH_AUTH_SOCK bash` and operate inside the VM without an agent.
- Disable forwarding wholesale: `limactl edit ahjo --set '.ssh.forwardAgent=false'` and restart the VM. `ahjo repo add` against private SSH remotes will then fail until you supply credentials another way (HTTPS + token, deploy key inside the VM, etc.).

### Setup

Lima forwards exactly one socket: whatever `$SSH_AUTH_SOCK` resolves to in the shell that ran `limactl start`. That sounds simple but trips most macOS users on first run, because:

- macOS pre-sets `$SSH_AUTH_SOCK` to a launchd-provided agent (`/private/tmp/com.apple.launchd.*/Listeners`) that is empty unless you explicitly opted into Keychain integration.
- 1Password (and Secretive, KeePassXC, gpg-agent, …) provide their own socket and tell `ssh` about it via `IdentityAgent` in `~/.ssh/config`. That works for host `git`, because host `ssh` reads `~/.ssh/config`. **Lima's ssh transport does not.** It only honors the env var.

Net effect: host `git clone` works, in-VM `git clone` fails with `Permission denied (publickey)`, and the VM's forwarded agent reports zero keys.

**For 1Password users**, add this to your shell rc:

```sh
export SSH_AUTH_SOCK="$HOME/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"
```

**For other agents**, find your socket the way `ssh` does:

```sh
ssh -G github.com | awk '/^identityagent / {sub(/^identityagent /, ""); print}'
```

If that prints a path (not the literal `SSH_AUTH_SOCK` or `none`), export it. If it prints `SSH_AUTH_SOCK`, your existing env var is already what `ssh` uses — confirm it's not the launchd default with `echo $SSH_AUTH_SOCK`.

After exporting, bounce the VM so its hostagent rebuilds the forwarding with the new socket:

```sh
limactl stop ahjo && limactl start ahjo
```

Verify end-to-end with `ahjo doctor` — the host-side block compares host and in-VM key counts and points at the fix when they diverge.

## Out of scope

ahjo isolates *workloads*, not the host CLI. Anything you `ahjo repo add` is code you've decided to bring in; ahjo doesn't sandbox `git clone` against a hostile remote. Treat container contents as you would any local checkout — review before running, and rely on the boundaries above to limit blast radius if you don't.
