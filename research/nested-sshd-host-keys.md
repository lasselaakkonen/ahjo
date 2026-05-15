# Nested ahjo: sshd host-key delivery — findings & options

**Status:** Investigation only. Decided not to act in 2026-05. Document
kept so the constraint and options are retrievable if the use case
materializes later.

## Use case framing (read this first)

This only matters for `ssh ahjo-<slug>` from *inside* an ahjo container
into a *further-nested* ahjo container. Not a production scenario today.
Two theoretical drivers:

- "Container should be fully functional in nested mode" — aesthetic, not
  load-bearing.
- "Let Claude yolo inside an ahjo container with the ahjo repo to
  exercise all ahjo features." — testing dev loop. But Claude can drive
  every interactive flow via `ahjo shell` (which uses `incus exec` and
  doesn't need sshd). IDE remote-attach over SSH is the only thing
  `ahjo shell` doesn't cover.

Conclusion: not currently worth fixing. This doc captures the why and
the options so a future decision is one read away.

## What happens today

Inside an outer ahjo container, `ahjo repo add` succeeds (auto-keygen +
ancestor-pubkey relay landed in `bd0e6d1`). The inner container is
created with correct merged `authorized_keys`. Nesting recurses cleanly
in the sense that another nested `ahjo repo add` would work the same
way.

**But sshd inside the inner container fails to start.** Effects:

- `ssh ahjo-<slug>` from the outer container's shell: refused (no
  listener).
- IDE remote-dev tooling (VSCode Remote, JetBrains Gateway, Cursor):
  cannot attach.
- `ahjo shell <slug>`: works fine (uses `incus exec`, not sshd).
- All git / file-sync / build flows that go through `ahjo shell` or
  `incus exec`: unaffected.

## Root cause

Id-mapped bind mounts across nested user namespaces.

Setup recap (works in single-level userns, fails nested):

- ahjo bind-mounts `~/.ahjo/host-keys/<slug>/` (mode 0700, contains 0600
  private keys owned by host uid 1000) into the container at
  `/etc/ssh/ahjo-host-keys/` via `incus.AddDiskDevice`
  (`internal/cli/repo.go:526-532`).
- Single-level (Lima → ahjo container): root in the container has
  `cap_dac_read_search` effective against the mount because the mount
  is owned by an ancestor namespace. sshd starts. ✓
- Nested (Lima → outer container → inner container): the id-mapped
  mount is created by the *outer* container's `incusd`, so the mount
  is owned by the outer userns — not by the inner's ancestor chain.
  Inner-root's caps don't apply. 0600 files owned by host uid 1000 are
  unreadable to inner-root.

Empirically verified inside a nested setup:

```
$ incus exec inner --user 0 -- head -c 50 /etc/ssh/ahjo-host-keys/ssh_host_ed25519_key
head: cannot open ... : Permission denied
$ incus exec inner --user 1000 -- head -c 50 /etc/ssh/ahjo-host-keys/ssh_host_ed25519_key
(reads fine — uid 1000 has owner DAC)
$ incus exec inner --user 0 -- head -c 50 /etc/shadow
(reads fine — rootfs file, inner userns ancestor chain applies, caps work)
```

sshd's `ExecStartPre=/usr/sbin/sshd -t` runs as root → "Unable to load
host key" → systemd 5-restart limit → failed state. Per-connection sshd
processes (which would fork as uid 1000) would read `authorized_keys`
fine — but the main sshd never starts, so they never spawn.

## authorized_keys is fine

It's 0600 uid-1000 too, but sshd reads it only inside the
per-connection fork running as the target user (uid 1000), which has
owner DAC across the id-mapped boundary. No fix needed there. Only the
host private keys are affected, because only the main sshd (running as
root) opens them.

## Why not "just chown the host-keys dir to root"

Host-side, the dir is in `$HOME/.ahjo/host-keys/<slug>/` — a user-owned
tree. Making it root-owned would require sudo in normal ahjo flows and
fight the rest of the user-owned config tree. The id-map shift means
even if you did, the in-container view would be a non-root shifted uid
anyway. Wrong layer to fix.

## Option A — do nothing

**Cost:** zero code. Users who hit it see a confusing systemd failure.
Workaround: `ahjo shell`. Recoverable; no data loss.

**When this is the right call:** nobody actually uses nested ahjo for
real work. (Today: this is the right call.)

## Option B — graceful note on nested sshd failure

After the existing `systemctl start ssh` failure path
(`internal/cli/repo.go:422` and `internal/cli/create.go:128`), detect
nested-host (`systemd-detect-virt --container` ≠ `none`) and print:

```
note: sshd not available in nested mode (id-mapped bind-mount limitation);
      use 'ahjo shell <slug>' to enter the container.
```

**Cost:** ~20 lines, no helper extraction, no package-level
`IsNestedHost` API, no file-push, no test churn.

**Buys:** the failure mode is signposted instead of mysterious. Doesn't
fix anything, just stops the next person from reproducing this
investigation.

**When this is the right call:** if anyone (including future-you) hits
the failure once and wastes time on it.

## Option C — file-push host keys in nested mode (the full fix)

Three pieces:

1. `paths.IsNestedHost()` with `sync.Once`, probing
   `systemd-detect-virt --container`. Split across
   `internal/paths/platform_linux.go` / `platform_other.go` (mirror
   `MacHostHome()` pattern).
2. In `wireBranchContainer` (`internal/cli/repo.go:500`), gate the
   `ahjo-host-keys` `AddDiskDevice` call on `!IsNestedHost()`. The
   `ahjo-authorized-keys` and `ahjo-ancestor-pubkeys` devices stay in
   both modes (both work across the boundary).
3. After `incus.Start` in nested mode, `incus file push` four files
   into the inner rootfs as root:root:

   ```
   ssh_host_ed25519_key     mode 0600
   ssh_host_ed25519_key.pub mode 0644
   ssh_host_rsa_key         mode 0600
   ssh_host_rsa_key.pub     mode 0644
   ```

   Then `systemctl reset-failed ssh` (sshd already in failed state from
   the boot-time start) and `systemctl start ssh`.

   Requires extending `internal/incus/incus.go:627` `FilePush` (or
   adding `FilePushAs`) to pass `--uid/--gid/--mode`. Pattern exists at
   `internal/incus/paste.go:145` (`--mode 0755`).
4. Same flow in `cloneFromBase` (`internal/cli/create.go:249`): gate
   the existing `ConfigDeviceSet(..., "ahjo-host-keys", ...)`
   (line 266-268) on `!IsNestedHost()`, and after the branch container
   starts in `runCreate` (`create.go:128`), push the branch's own host
   keys.

**Cost:** ~150 lines + tests. New package-level detection API, new
incus helper, branch-aware control flow in two CLI commands.

**Trade-off:** host keys land in the *inner rootfs* instead of as a
live-updating bind mount. If host-side keys rotate, the in-container
copy goes stale until ahjo re-pushes. Host keys are generated once per
slug and don't rotate during normal use, so the trade is acceptable.

**Buys:** `ssh ahjo-<slug>` and IDE remote-attach work in nested mode.

**When this is the right call:** if nested ahjo becomes a real use case
— e.g. Claude (or anyone) is doing IDE-based dev work *inside* an outer
ahjo container against a further-nested one. Today: no.

## Option D — drop the bind mount unconditionally, always file-push

Skip detection entirely. Always push host keys into the container
rootfs at create time, never bind-mount. One code path.

**Cost:** ~80 lines. Simpler than C (no `IsNestedHost`, no branching),
but more invasive than B.

**Trade-off:** loses live-update for *all* modes, not just nested.
Since host keys don't rotate during normal use, this loss is
theoretical — but it does change established behavior for the 99%
single-level case to fix the 0% nested case.

**When this is the right call:** if option C is on the table *and*
you'd rather pay the simplification tax than maintain a nested-vs-not
branch. Probably never the right call unless C is already being
written.

## Detection signal (for B / C)

`systemd-detect-virt --container`:

- Inside the user's outer ahjo container: prints `lxc`, exits 0.
- On a Lima VM (no container around it): prints `none`, exits 1.
- Treat any non-`none` output OR exit 0 as nested; cache via
  `sync.Once`. Don't rely on exit code alone (older systemd versions
  vary).
- If the binary is absent on a minimal image, treat as not-nested and
  log a debug line. Don't break the Lima-direct path.

The detection is keyed on "am I inside a container?", not "did the
outer ahjo create the container I'm in?", because the constraint is
the kernel's id-mapped-mount-in-nested-userns behavior — it applies to
any container-in-container shape, not just ahjo-in-ahjo.

## Out of scope / follow-ups

- `TERM=xterm-ghostty` terminfo: orthogonal. Ghostty's terminfo isn't
  in ahjo-base; either install it, or remap `TERM` to a known fallback
  on shell entry.
- `sg incus-admin -c` wrapper for nested setups: separate ergonomic
  paper cut.
- Half-state cleanup on `repo add` failure: `incus init` runs before
  the legacy-devcontainer probe, leaving stray containers on failure.
  Separate concern.

## Decision log

- **2026-05-15:** investigated, plan written (option C). Reviewed.
  Decided not to act — no real use case, complexity unjustified.
  Document filed here for retrieval if needed. Original plan at
  `~/.claude/plans/review-this-plan-tmp-ahjo-nested-sshd-ho-prancy-stearns.md`.
