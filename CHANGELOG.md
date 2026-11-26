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
- `security.nesting` and `customizations.ahjo.nested_incus` now need host-side consent. A repo can no longer enable them by declaring `ahjo/docker` or `nested_incus` in its own config: `ahjo repo add` prompts `[y/N]` on a terminal and fails without one unless `--allow-nesting` (Feature-requested nesting) or `--allow-nested-incus` (nested Incus, implies nesting) is passed. `--yes` approves neither.
- `nested_incus` no longer passes every `/dev/loop0..7` into the container. Only loop devices with no backing file are passed, so the device backing ahjo's btrfs storage pool (and other bound loop devices) is no longer exposed read-write. `repo add` fails if no unbound device exists.
- Containers with loop devices get `boot.autostart=false`, and ahjo re-checks the devices before each ahjo-initiated start, dropping any that are now bound on the host.
- Residual risk, not fixed: a passed device that the host or another container binds while the container is running is still readable and writable from inside it, and `incus start` run by hand skips the re-check.
- Closed the bypasses of the SSH-agent auto-suppression. The agent
  is now withheld from an HTTPS+PAT repo on every route, not just the proxy
  device attached by `ahjo shell`/`ahjo claude`:
  - `ahjo ssh` and `ahjo ide` no longer get the agent through a hardcoded
    `ForwardAgent yes` in the generated ssh-config; each host follows the
    policy, and `forward_ssh_agent = false` now applies there too.
  - `ahjo repo add` no longer attaches the agent during setup (Features,
    warm-install, `onCreate`/`postCreate` hooks), and the stopped base container
    no longer keeps the device.
  - The `ssh-agent` device is stripped before every container start, and
    `ahjo repo set-token` removes it from the repo's existing containers.
  - `https://GitHub.com/...`, `https://github.com:443/...` and
    `https://user@github.com/...` origins are recognised as HTTPS GitHub.
  - macOS: `ahjo create` on a registered repo now passes the Keychain PAT, and
    `--container-config <value>` is no longer mistaken for the repo alias.
- The suppression decision is recorded per repo as `has_token` in the registry.
  Repos added before this change pick it up on their next `ahjo shell`/`claude`/
  `create` or `ahjo repo set-token`; until then their ssh-config entry keeps
  `ForwardAgent yes`. Set `forward_ssh_agent = true` to restore the old
  behaviour (agent in `ahjo ssh`/`ahjo ide` sessions and during `repo add`).

- The per-repo GitHub PAT is no longer persisted in the container's Incus
  config (`environment.GH_TOKEN` / `environment.GITHUB_TOKEN`). It is injected
  per exec into `ahjo shell`, `ahjo claude`, lifecycle hooks, warm install and
  ahjo's own git operations, and devcontainer Feature `install.sh` scripts no
  longer receive it. On macOS the shim now also relays it for `ahjo repo pull`
  and `ahjo rm` (background base refresh).
- Containers created by older versions are cleaned on the next `ahjo shell` /
  `ahjo claude` / `ahjo create` / `ahjo repo set-token` for the repo, and
  `ahjo doctor` reports any container that still stores the PAT. A running
  container's PID 1 keeps the old value in its environment until it restarts,
  so restart long-running containers, and rotate PATs that older versions
  stored.
- Known limitation on macOS: `ahjo top` does not relay the PAT, so PR status
  for private repos shows an error there. It used to work through the stored
  key.
- `incus config set` errors no longer include the value, and `environment.*`
  values are redacted from incus's own error output.
- macOS: the shim no longer puts the PAT on the `limactl shell` command line
  (visible in `ps` for the whole session). It writes it to a 0600 file under
  `~/.ahjo-shared/.relay-env/` and passes only the file name; the in-VM ahjo
  reads and deletes it. Storing a PAT in the Keychain feeds `security -i` on
  stdin instead of passing it as `-w <value>`. Run `ahjo update` after
  upgrading: an older in-VM binary ignores `AHJO_RELAY_ENV` and runs without
  the PAT.
- Token files (`~/.ahjo/.env`, `~/.ahjo-shared/repo-env/<slug>.env`) are now
  replaced atomically and always end up mode 0600, even when an existing file
  was wider. Keys and values containing a newline, carriage return or NUL are
  rejected instead of being written as extra lines.
- `ahjo init` reads the pasted Claude OAuth token with masked input, and its
  "wrong prefix" error no longer echoes the first characters of the token.

### Fixed

- `ahjo top` on macOS showed gh's "not logged in" error in the `pr:` row for repos with a stored PAT. Since the PAT stopped being persisted on the container, the PR lookup ran without it. `top` now reads the repo's PAT from the Keychain (once per session) and relays it to the PR lookup the same way `ahjo shell` does.

### Changed (mirror)

- Upgrade: the first ahjo command after updating stops any mirror of the old design, removes its `mirror` device and unit, and reverts the target. Run `ahjo mirror <alias>` again to resume. A process in that container that still holds a file open under `/mirror` may keep it until the container restarts.
- A stored mirror target that the new rules refuse must be replaced with `--target`.
- Files over 512 MiB and files whose names are not valid UTF-8 are not mirrored. A `HEAD` file next to an `objects/` or `refs/` directory is not mirrored.
- `ahjo mirror logs` shows the host daemon's log (`~/.ahjo/mirror/daemon.log`) instead of the container journal.
- When the watcher restarts (a `.gitignore` edit, a container restart) it re-reads the whole non-ignored tree; unchanged files are not rewritten on the host.

### Breaking (nesting consent)

- Non-interactive `ahjo repo add` of a repo that declares `ahjo/docker` or `nested_incus` now fails without `--allow-nesting` / `--allow-nested-incus`.
- A detected Dockerfile or compose file is no longer auto-applied as `ahjo/docker` on `--yes` / non-interactive runs unless `--allow-nesting` is passed.
- Containers created before this change are not fixed in place. They keep every loop device they were given, including a bound one, until they are stopped and then started through ahjo; the re-check only runs on a stopped container. `boot.autostart=false` is applied to them the first time ahjo starts or attaches to them. To close the gap now, stop each `nested_incus` container (`incus stop <container>`) and start it again with ahjo.
