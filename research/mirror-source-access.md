# Mirror source access — findings & options

**Status:** Phase 2 implementation complete on the activation/lifecycle side; source access blocked. Decision needed before merge.

## What we set out to do

Phase 2 of `no-more-worktrees` repoints `ahjo mirror`'s fsnotify+rsync daemon at the container's rootfs subvolume on the VM filesystem:

```
/var/lib/incus/storage-pools/<pool>/containers/<container>/rootfs/repo
```

Pool resolved per-container via `incus query /1.0/instances/<name>` → `expanded_devices.root.pool`.

## What works (verified)

- Per-container pool resolution. Test VM has both `default` btrfs (unused) and `zfs-pool` (the e2e-sandbox containers live there). Hardcoding `default` would have failed; dynamic resolution returned `zfs-pool` correctly.
- Container existence + running state checks.
- Cold-start refusal: stopped default container yields *"container is stopped; run `ahjo shell …` first"*.
- Auto-stop hooks in `ahjo rm` and `ahjo shell --update`.
- Path layout is correct — the rootfs path exists and contains `/repo`.

## What fails

```
$ ls /var/lib/incus/storage-pools/zfs-pool/containers/<name>/rootfs/repo
ls: Permission denied
$ namei -l <path>
…
d--x------ 1074266112 root <container-name>
                            rootfs - Permission denied
```

The container directory on the host is mode `d--x------` owned by an idmap-shifted uid (host uid 1000 + container uid 0 = 1074266112). Only root can traverse. This is Incus's protection model for idmapped containers, not a bug.

`sudo rsync` works. Lima virtiofs translates VM-root writes back to Mac-user files, so destination ownership is fine.

The design doc called this "officially internal Incus territory, but stable in practice." The path *is* stable — it's just unreadable as a regular user, which the doc didn't anticipate.

## Probed alternatives

### `incus file mount <name>/repo <local-path>` (supported primitive)

```
incus.<name>:/repo on <local-path> type fuse.sshfs (rw,…,user_id=501,group_id=1000)
```

- Mounts as user-readable FUSE/sshfs. Bootstrap rsync works as the user.
- **Critical limitation: inotify does not see container-side writes.** Probed: wrote a file via `incus exec` inside the container; `ls` on the FUSE mount surfaced it (no caching), but `inotifywait -m -r` fired only for client-side reads, never for the in-container create. Standard FUSE-sshfs inotify limitation, not specific to Incus.
- Conclusion: usable for bootstrap or for polling-based rsync; **not a drop-in for fsnotify-driven live sync**.
- Operational: long-running subprocess per mirror; teardown via `fusermount -u` + kill.

### `sudo` the existing daemon on the storage-pool path

- Smallest delta. Code already works; only privilege escalation is missing.
- Requires passwordless sudo for the ahjo binary, or running `ahjo mirror` itself elevated.
- Daemon then has full root on the VM.
- Mac-side ownership is fine (Lima virtiofs uid translation already verified).

### In-container daemon (Doc B territory)

- Watcher runs inside the container against `/repo` natively. fsnotify works without restriction.
- Push changes to Mac via the existing SSH proxy (rsync over SSH from container to Mac).
- Largest scope: in-container packaging, lifecycle (start with container, stop on exit), authentication, and a hop the user would otherwise see in their SSH agent.

## Compromise options worth weighing

| Option | fsnotify works? | Privilege | Code delta | Direction |
|---|---|---|---|---|
| **A. sudo + storage-pool path** | yes | root | small | interim |
| **B. `incus file mount` + polling rsync** | no (timer-driven) | user | medium | interim |
| **C. `incus file mount` + sudo fsnotify on storage-pool path, rsync from FUSE** | yes (sudo half) | partial root | medium | hybrid |
| **D. In-container daemon, rsync over SSH** | yes | user | large | Doc B |

Option C is too clever to recommend — half-root, two source paths.

## Recommendation

**B for shipping interim, D for the eventual replacement.**

B's polling cadence (e.g., 2s timer instead of 200ms fsnotify debounce) is a noticeable downgrade vs. Phase 0's worktree fsnotify, but it ships without escalating privileges, exercises the supported primitive, and the daemon code change is minimal (replace the watcher loop with a ticker; keep `incus file mount` as a managed subprocess of the daemon).

D is the right end state. It removes the FUSE limitation, eliminates the storage-pool path entirely, and aligns with the planned `in-container-mirror.md` architecture.

A ships fast but escalates privileges silently and locks us into the storage-pool path that Doc B was meant to retire — net: more work to reverse than B.

## Open questions

1. Polling cadence for option B — 1s, 2s, 5s? rsync on a 30k-file repo with `--filter=:- .gitignore` takes ~150–400ms warm; 2s feels safe.
2. If we go straight to D, does Phase 2 ship at all, or does the next merge land Doc B directly?
3. `incus file mount` requires `sshfs` on the VM — currently absent? Need to check, and add to the install path if so.

## Decision needed

Pick A / B / C / D (or "abandon Phase 2 and go straight to Doc B"). Implementation cost rises with letter; long-term carrying cost falls.
