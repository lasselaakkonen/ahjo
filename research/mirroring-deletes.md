# Mirroring deletes — findings & options

**Status:** research note. Captures the trade-space behind v3's deliberate non-goal *"deletion tracking,"* the reasons naive event-driven delete propagation fails, the cost of a debounced reconcile pass, and the gitignore-parity blocker that makes "just add rsync back" the wrong shape. Companion to `designdocs/in-container-mirror.md` and `research/spike-gitignore/`.

## Context

`ahjo mirror` is one-way (container `/repo` → Mac `/mirror`) by design. v3 explicitly drops delete tracking: when a file disappears from `/repo`, its `/mirror` counterpart is left in place; recovery is `ahjo mirror off && rm -rf <target>/* && ahjo mirror on`. The naming question that prompted this note — *"is it really mirroring if it doesn't propagate deletes?"* — is fair; "one-way replica without delete tracking" is more accurate. The harder question is whether the omission is permanent or a v1 simplification that should be revisited.

## Why v3 omits deletes (recap)

From `designdocs/in-container-mirror.md` §"Non-goals":

> Eliminating delete-tracking removes the only thing that requires two-tree reconciliation in the live path; the simplification cascades.

The cascade: no two-tree diff → no rsync → no second gitignore implementation → no debouncer → no in-flight state to coordinate between bootstrap and the event drain → no PID/state file. The current daemon is ~200 lines because of this single omission.

## Why naive inotify-based delete propagation fails

inotify reports name-level events. The daemon cannot reliably distinguish "the user deleted this" from "something transient is happening to this name."

**Editor rename-on-save.** Save-via-rename in vim, JetBrains, VS Code, etc. produces:

```
IN_CREATE     .foo.txt.tmp.XYZ
IN_MODIFY     .foo.txt.tmp.XYZ
IN_MOVED_FROM foo.txt              ← looks like delete
IN_MOVED_TO   foo.txt~             ← (backup; may be in same or sibling dir)
IN_MOVED_FROM .foo.txt.tmp.XYZ
IN_MOVED_TO   foo.txt              ← canonical name re-published
```

Between events 3 and 6 the canonical name does not exist. An eager `rm /mirror/foo.txt` opens a window where host-side watchers (Vite, watchexec, Xcode indexing) see the file vanish, fire delete handlers, then re-fire create handlers when the rename completes — at best double work, at worst (with event reordering) a permanent missing file. A debounce window that "holds deletes for N ms and cancels on MOVED_TO of the same name" is the v1 architecture, and there is no N that's correct for all editors (swap files, `.~lock.*#`, `___jb_tmp___` patterns persist for variable durations).

**`git checkout` between branches.** A branch switch rewrites the working tree as a storm of CREATE / DELETE / MOVED events, often thousands within fractions of a second. Two distinct hazards:

1. The daemon cannot distinguish "user removed file" from "git is shuffling between branches." If Xcode/VS Code/JetBrains is open against `/mirror`, every branch hop invalidates indexes and aborts in-flight work. Without delete propagation, the host sees a stable union of recently-existing files — boring, friendly to long-running tools. The user reconciles explicitly when they choose to.

2. Storms risk `IN_Q_OVERFLOW`. On overflow the daemon doesn't know what it missed. Recovery is a `/repo` re-walk, which re-discovers files that should be on the mirror but **cannot discover files that should be deleted** — they are not in `/repo` anymore. Detecting orphans requires a destination walk and a source-vs-dest diff: rsync's job, by another name.

**Asymmetry of failure.** Copy semantics are idempotent (copy the same file twice → no harm). Delete semantics are destructive and unrecoverable (delete the wrong file → host editor session notices). The daemon trades correctness in the cheap direction for guarantees in the expensive direction.

## The "inotify for copies + debounced reconcile for deletes" idea

Live copies via inotify (already shipped); periodic reconciliation (after N seconds of quiescence) handles deletes. Architecturally sound — it matches what tools like Mutagen do internally — but the implementation choices matter.

### Cost of a periodic `rsync -a --delete /repo/ /mirror/`

rsync's work is two stat walks plus a diff:

1. **Source walk** — `/repo` is a real kernel filesystem inside the container; stat() is microseconds. 30k files ≈ **100–500ms** cold. Cheap.

2. **Destination walk** — `/mirror` is virtiofs (container disk-device → Lima → Mac). Each stat() crosses the guest/host boundary; observed Lima virtiofs stat latency is roughly **50–500µs/call** depending on cache and OS version.
   - **2k files** (ahjo-scale repo): ~200ms – 1s
   - **30k files** (monorepo): **3–15s per run**

3. **Quick-check** (default size+mtime) — no content hashing, so the steady-state cost is dominated by the two walks.

Performance alone isn't a blocker for ahjo-sized repos. The cost is a background tax on the Mac filesystem cache, noticeable as fan/battery on laptops for big monorepos.

### Gitignore parity is the blocker

`rsync --filter=:- .gitignore` does **not** honour `!` negation patterns. From the v3 spike (`research/spike-gitignore/`, 2026-05-10), disagreement vs `git check-ignore`:

| Fixture                                      | rsync `--filter=:- .gitignore` |
|----------------------------------------------|---------------------------------|
| `.env*` + `!.env.example`                    | **16.7%**                       |
| `*.log` + `!important.log`                   | **6.7%**                        |
| Negation chains                              | **20%**                         |

How this breaks the hybrid:

1. `.gitignore`: `.env*\n!.env.example\n`
2. Live daemon (go-git matcher, git-faithful) correctly copies `.env.example` on save.
3. 1s later, debounced rsync runs with its own filter, classifies `.env.example` as excluded, **`--delete` removes it from `/mirror`**.
4. Next save touches `.env.example`; live daemon re-copies it. Host IDE/build sees the file flicker in and out on every save cycle.

`.env*` + `!.env.example` is the canonical config pattern in Vite, Next.js, Rails, Django — not an edge case.

This is the same defect v3 cited to remove rsync entirely. Reintroducing rsync — even just for the periodic catchup — re-introduces the bug.

## Implementation options

### A. rsync with a daemon-generated `--exclude-from`

Before each rsync run, the daemon walks `/repo` using the go-git matcher and writes the enumerated verdict to `/tmp/ahjo-mirror-exclude.<rand>`. rsync receives a flat exclude list — no `!` semantics to disagree on:

```
rsync -a --delete --exclude-from=<file> /repo/ /mirror/
```

- **Pros:** single source of truth for ignore decisions (the daemon's matcher); rsync becomes a dumb copy/delete engine.
- **Cons:** adds a per-cycle `/repo` walk (cheap, container-local); rsync still exists as a runtime dependency the design doc was happy to be rid of; the embedded ignore list can be large for monorepos.

### B. Daemon-owned Mac-side delete sweep (no rsync)

inotify continues to handle copies. The only thing rsync would bring is "find orphans on the dest." Do that directly:

```go
filepath.WalkDir("/mirror", func(p, d) {
    src := strings.Replace(p, "/mirror", "/repo", 1)
    if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) || matcher.Match(src) {
        os.Remove(p)
    }
})
```

- **Pros:** no rsync, no second filter implementation; same go-git matcher governs every decision; ~30 lines of Go; same dest-walk cost as rsync (unavoidable price of finding orphans regardless of tool).
- **Cons:** still pays the virtiofs destination walk; adds a goroutine + quiescence timer + cancellation logic when new events arrive mid-sweep; needs care around in-flight tempfile-renames (skip names matching `*.ahjo-mirror.tmp.*`).

### C. `ahjo mirror off --clean` flag (existing open question #3)

No daemon changes. `mirror off` learns a `--clean` flag that `rm -rf`s the target after disabling. Codifies the documented recipe, doesn't try to be smart between branch switches.

- **Pros:** trivial to implement; preserves the v3 simplicity claim; lets the user opt in explicitly when they know they want a clean slate.
- **Cons:** still manual; doesn't help with the *gradual* accumulation case (e.g., agent generates a file, decides it was wrong, deletes it — host now has a stale file forever until next clean).

### D. Status quo

Document "no delete tracking" prominently; ship a `mirror status --stale` diagnostic that lists Mac-side files with no `/repo` counterpart, so the user can decide when to nuke. Half the safety of B, none of the implementation cost.

## Drawbacks of doing this at all

The v3 design rests on **"per-event copy, idempotent, no state."** Any option except (D) introduces state machinery:

- A debouncer (goroutine + timer + reset-on-event).
- A quiescence definition (1s? 5s? configurable?). Too short = fights bursts; too long = stale window grows.
- Cancellation of an in-flight sweep when new events arrive (otherwise long sweeps overlap with live writes).
- Handling editor-tempfile names that *correctly* exist on `/mirror` momentarily but should not be deleted.
- A new failure mode: sweep concludes "file is orphan" while a slow filesystem reports a stale ENOENT; mistakenly unlinks a live file. Mitigated by re-statting source under a lock, but lock contention is new complexity.

None of these are individually large. The aggregate is the kind of state machine v3 explicitly tore out. Whether it earns its keep depends on how often users hit the `rm -rf <target>` recipe in practice — which we don't currently measure.

## Naming

Independent of whether deletes get added: the user-facing word "mirror" is defensible (CPAN/Debian/distro mirrors are one-way), but the semantics map more accurately onto **replication** (per-file event push, no convergence) than mirroring (state convergence). If the behavior stays as-is, the help text already does the right thing — calling it *"one-way push; deletes are not tracked"* — and the word "mirror" is fine. A rename to `replicate` would be churn without an underlying behavior change.

## Recommendation

1. **Short term:** ship option **C** (`mirror off --clean`). Resolves open question #3, ~20 lines, no architecture change. Honest about what the daemon does and doesn't do.
2. **Medium term, only if user demand surfaces:** option **B** (daemon-owned delete sweep, no rsync). Preserves the single-matcher invariant; adds a bounded amount of state machinery rather than reintroducing rsync. Gate on whether real users actually hit the staleness problem.
3. **Do not** add rsync back via option A unless we accept maintaining the exclude-list generation forever; the simplification gained in v3 is worth more than the convenience of letting rsync compute the diff.
4. **Do not** rename "mirror" to "replicate" — naming churn without behavior change. Document the semantics in help text and the README; that's already done.

## Open questions

- How often do users actually hit the staleness problem in practice? No telemetry today. Could be cheap to add (`mirror status` already exists; have it count orphans).
- Does a Mac-side delete sweep need to respect `--no-skiplist`? Almost certainly yes (sweep must match the activation's filter set), which means the sweep reads the same systemd drop-in the live path consults.
- Should the sweep run on a fixed cadence or only after inotify quiescence? Quiescence is more elegant; cadence is simpler. Probably cadence (every 30s, configurable) — quiescence detection adds a second timer and a "have we been quiet long enough?" race.
