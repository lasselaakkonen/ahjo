# Changelog

## Unreleased

### Changed (behaviour change)

- Containers can no longer reach private networks, the host, or each other. `ahjo init` and `ahjo update` attach a network ACL (`ahjo-egress`) to `incusbr0` that rejects traffic to private, loopback, link-local and CGNAT ranges (including the Mac via `host.lima.internal`, the VM, your LAN, VPN and Tailscale, and cloud metadata), and enable port isolation on the default profile's `eth0`. The public IPv4 internet is unchanged.
- To let containers reach a private range, add it to `[network] allow_cidrs` in `~/.ahjo/config.toml` (IPv4, `/8` or longer). On macOS this is the file inside the Lima VM (`limactl shell ahjo -- sh -c 'nano ~/.ahjo/config.toml'`), not the Mac-side one. Even inside an opted-in range, every address on one of the host's own interfaces (on macOS, the Lima VM), the `incusbr0` subnet and, on macOS, the addresses at which the VM sees the Mac (`host.lima.internal` and the VM's default gateways) stay blocked. The Mac's own address on your LAN is not covered: if you opt in the range it sits in, containers can reach Mac services listening on all interfaces at that address, so prefer a narrow prefix. To reach a single host service, use `ahjo forward`.
- Existing installs: run `ahjo update` once to apply this. The firewall is expected to apply to running containers right away; the first time, Incus may re-attach each container's network interface and drop open connections, so run it when no container is mid-task and restart a container if it loses network. Later `allow_cidrs` changes apply on `ahjo update` with no restart. `ahjo doctor` warns while it is missing.

### Security

- **Breaking:** `customizations.ahjo.forward_env` names declared by a repo (read from the in-container `ahjocontainer.json`) are no longer forwarded unconditionally. Each name needs per-repo approval, stored host-side in `registry.toml`: a `[y/N]` prompt at `ahjo repo add` (`--yes` and non-TTY approve nothing), or `ahjo repo approve-env <alias> KEY...` (`--revoke` to withdraw). Unapproved names are skipped with a hint at attach. Existing repos start with no approvals: run `ahjo repo approve-env <alias> KEY...` once to restore previously forwarded names. Names in the global `forward_env` are unaffected.
- `~/.ahjo/.env` is no longer loaded into ahjo's process environment at startup (and so no longer inherited by every subprocess); it is read on demand.
- **`ahjo mirror` no longer mounts the host target into the container.** Previously the target was bind-mounted read-write at `/mirror`, so any process in the container could read it (including `.env` and `.git`) and write `.git/config` or `.git/hooks`, which let it run code on the host the next time git ran there. Now a watcher in the container streams changes to a host-side daemon, which is the only writer and validates every path: nothing outside the target, nothing through a symlink, no `.git` component in any spelling (also with `--no-skiplist`), nothing that gives a directory the shape of a git repository.
- Every git command ahjo runs on a mirror target disables `core.fsmonitor` and hooks and sets `safe.bareRepository=explicit`. `mirror off` trusts the snapshot marker kept under `~/.ahjo` before anything found in the target.
- Mirror targets are validated after resolving symlinks. The home directory itself, dot-directories directly under it and `~/Library` are refused; on bare-metal Linux the target must be inside `$HOME`.

### Changed (mirror)

- Upgrade: the first ahjo command after updating stops any mirror of the old design, removes its `mirror` device and unit, and reverts the target. Run `ahjo mirror <alias>` again to resume. A process in that container that still holds a file open under `/mirror` may keep it until the container restarts.
- A stored mirror target that the new rules refuse must be replaced with `--target`.
- Files over 512 MiB and files whose names are not valid UTF-8 are not mirrored. A `HEAD` file next to an `objects/` or `refs/` directory is not mirrored.
- `ahjo mirror logs` shows the host daemon's log (`~/.ahjo/mirror/daemon.log`) instead of the container journal.
- When the watcher restarts (a `.gitignore` edit, a container restart) it re-reads the whole non-ignored tree; unchanged files are not rewritten on the host.
