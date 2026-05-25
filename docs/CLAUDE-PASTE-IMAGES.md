# CLAUDE-PASTE-IMAGES.md

How `Ctrl+V` image paste works inside `ahjo claude`, and why ahjo bridges it
manually instead of leaving it to the terminal.

## TL;DR

Claude Code on Linux reads clipboard images by shelling out to `xclip` /
`wl-paste`. Inside an ahjo container neither exists, and even if they did
there's no graphical clipboard to read from. ahjo solves this with:

1. A **macOS host daemon** that reads `NSPasteboard` and serves PNG bytes
   over loopback HTTP (`127.0.0.1:18340`).
2. An **Incus TCP proxy device** per container that forwards
   `container:127.0.0.1:18340` to the Mac's daemon (via `host.lima.internal`).
3. Shell **shims at `/usr/local/bin/{xclip,wl-paste}`** inside each
   container that `curl` the proxy on image MIME requests.

Claude's existing paste path — first `xclip -selection clipboard -t TARGETS -o`
to enumerate MIME types, then `xclip -t image/png -o` for bytes — just works,
with no Claude patches, no typed paths, no Accessibility prompt.

## Why this is needed

The TTY chain is `Ghostty → limactl shell → incus exec --force-interactive
→ claude`. Image bytes never enter the terminal stream — paste data is
text-only over PTYs. Three known failure modes:

- **No native bridge.** The macOS pasteboard isn't reachable inside the
  Lima VM or the Incus container. OSC 52 only carries text.
- **Claude doesn't read its own clipboard on Linux.** Per [Claude Code
  issue #29204](https://github.com/anthropics/claude-code/issues/29204):
  *"Both `xclip` and `wl-paste` fail silently (due to `2>/dev/null`), so
  the grep finds nothing"*. No env var to override.
- **Drag-and-drop is partial.** Ghostty drop types host paths, but ahjo
  containers don't see those paths and screenshots taken with
  `Cmd+Shift+Ctrl+4` only land on the clipboard — never on disk.

## Architecture

```
┌──────────────────────────── macOS host ────────────────────────────┐
│                                                                    │
│  Ghostty ──► limactl shell ─────────────────────────────────────┐  │
│                                                                 │  │
│  ahjo paste-daemon (127.0.0.1:18340, launchd KeepAlive)         │  │
│    └─ JXA: NSPasteboard.dataForType('public.png')               │  │
│           fallback NSBitmapImageRep TIFF→PNG                    │  │
│    └─ HTTP: GET /image.png → 200 PNG bytes | 204 No Content     │  │
│                                                                 │  │
└─────────────────────────────────────────────────────────────────┼──┘
                                                                  │
┌──────────────────────────── Lima VM ────────────────────────────┼──┐
│                                                                 │  │
│  host.lima.internal:18340 ◄── Lima writes /etc/hosts            │  │
│                                                                 │  │
└─────────────────────────────────────────────────────────────────┼──┘
                                                                  │
                Incus proxy device (per container):               │
                  listen=tcp:127.0.0.1:18340  (bind=container)    │
                  connect=tcp:<resolved-IP>:18340 ────────────────┘
                  │
┌─────────────────┼──────────── Container ───────────────────────────┐
│                 ▼                                                  │
│  /usr/local/bin/xclip                                              │
│  /usr/local/bin/wl-paste                                           │
│    └─ curl -fsS http://127.0.0.1:18340/image.png                   │
│                                                                    │
│  claude ── Ctrl+V ─► xclip -selection clipboard -o -t image/png    │
│                                                                    │
└────────────────────────────────────────────────────────────────────┘
```

## Components

### Host daemon (`ahjo paste-daemon`)

- Source: `internal/paste/daemon_darwin.go`.
- Hidden Cobra-less subcommand on macOS-side `ahjo`. Launchd's plist
  invokes `ahjo paste-daemon`; users never call it directly.
- HTTP server on `127.0.0.1:18340` (one off from cc-clip's `18339` so the
  two coexist). Port is defined once as
  `incus.PasteDaemonContainerPort` and reused by every Go caller.
- Endpoints:
  - `GET /healthz` → 200 `ok\n`
  - `GET /image.png` → 200 + `Content-Type: image/png` if pasteboard has
    an image, **204 No Content** otherwise.
- Every request is logged to `~/Library/Logs/ahjo-paste-daemon.log` as
  `METHOD path remote -> status (bytes, latency)`. Cheap and lets
  `tail -f` show exactly what an in-container shim asked for after a
  `Ctrl+V`.
- Pasteboard read uses `osascript -l JavaScript`. Prefers `public.png`,
  falls back to `public.tiff` → `NSBitmapImageRep` → PNG.
- **No cgo, no `pngpaste` dependency.** Survives the
  `CGO_ENABLED=0` darwin dist build (see `Makefile`).
- SIGTERM / SIGINT → graceful `http.Server.Shutdown` with a 2s deadline.

### Launchd lifecycle (`internal/paste/lifecycle_darwin.go`)

Two public entry points:

- `paste.EnsureRunning()` — called from `cmd/ahjo/main_darwin.go` before
  every VM relay (`limactl shell ahjo …`). Hot path: 200 ms `GET /healthz`
  probe returns immediately when launchd already has the daemon up. Cold
  path: writes
  `~/Library/LaunchAgents/net.ahjo.paste-daemon.plist`, runs
  `launchctl bootstrap gui/<uid> <plist>`, re-probes for up to ~2s.
- `paste.Unload()` — called from `runMacNuke` in `cmd/ahjo/main_darwin.go`.
  `launchctl bootout` + delete the plist. Tolerant of already-unloaded.

Plist keys:

- `RunAtLoad=true` — daemon spawns after each login without needing an
  explicit `ahjo` invocation.
- `KeepAlive=true` — crashes respawn automatically.
- `ProcessType=Background` — macOS power scheduler treats it as background.
- Logs: `~/Library/Logs/ahjo-paste-daemon.log` (stdout+stderr).

Stale-binary detection: each EnsureRunning compares the existing plist's
`ProgramArguments[0]` against the current `os.Executable()`. If they
differ (e.g. user moved or upgraded ahjo), the plist is rewritten and the
service re-bootstrapped. No manual reload step.

### Incus proxy device (`internal/incus/paste.go`)

`EnsurePasteDaemonProxy(container)` adds:

```
device:   ahjo-paste-daemon
type:     proxy
listen:   tcp:127.0.0.1:18340     # inside the container
connect:  tcp:<resolved-IP>:18340  # macOS, via Lima's host gateway
bind:     container
```

Incus rejects hostnames in `connect=`, so ahjo resolves
`host.lima.internal` to an IPv4 address (Lima 1.x writes
`192.168.5.2 host.lima.internal` to `/etc/hosts` in the VM) at every
call. If the cached device already points at the current IP, the call
no-ops; otherwise it's stripped and re-added. Lima gateway changes
self-correct on the next `ahjo shell` / `ahjo claude`.

`bind=container` means the listen socket materializes inside the
container's network namespace, so the in-container shims can call
`localhost:18340` with no awareness of the VM topology. This is the
same pattern ahjo uses for the SSH agent socket
(`EnsureSSHAgentProxy`).

**Auto-expose interaction.** `reconcileAutoExpose` (`internal/cli/autoexpose.go`)
walks `ss -tlnH` inside the container and forwards every loopback
listener to a host port. The paste-daemon proxy creates exactly such a
listener — without an explicit skip, every `ahjo shell` would mint an
`ahjo-auto-18340` device and a burned host-port allocation. autoexpose
hard-skips `incus.PasteDaemonContainerPort` alongside `22` (sshd), so
the paste port is never exposed externally.

### Shell shims (`internal/incus/paste_shim_{xclip,wlpaste}.sh`)

Embedded via `//go:embed`, pushed at attach time with mode `0755`. Each
is ~40 lines of POSIX sh and handles three invocations:

| Invocation | Behavior |
|---|---|
| `xclip -t TARGETS -o` / `wl-paste --list-types` | Probe daemon. If `GET /image.png` returns 200, print `image/png\n` + exit 0. Otherwise exit 1 (matches real tool's "no targets / no image"). |
| `xclip -t image/* -o` / `wl-paste --type image/*` | `curl --max-time 5 …/image.png` to a tempfile; pass through stdout on 200, exit 1 otherwise. Any `image/*` MIME maps to the PNG payload (daemon only serves PNG). |
| Anything else | exit 1. |

Two-step is critical: Claude's Linux paste path **probes for available
targets first** and only requests bytes when the target list contains
`image/png`. A shim that exits 1 on the TARGETS query made Claude
report "No image found in clipboard" without ever fetching bytes — fixed
in commit-after-initial-landing.

Every invocation appends to `/tmp/ahjo-paste-shim.log` inside the
container (`argv` plus the resulting `status=` / `bytes=`). Pair with
the daemon log to trace any failed `Ctrl+V`:

```sh
# inside container — what claude asked for
limactl shell ahjo -- incus exec <ctr> -- tail /tmp/ahjo-paste-shim.log
# on Mac — what the daemon returned
tail ~/Library/Logs/ahjo-paste-daemon.log
```

### Wire-up points

`attachPasteShim(container)` (`internal/cli/repo.go`) calls both
helpers; invoked **post-start** from:

- `repoAddSetup` (`internal/cli/repo.go`) — base container created by
  `ahjo repo add`.
- `prepareBranchContainer` (`internal/cli/shell.go`) — every
  `ahjo shell` / `ahjo claude` against a branch container.

Both are best-effort: a failure logs `warn: paste shim: …` and
proceeds. Image paste won't work, every other ahjo command still does.

## Lifecycle (when does what happen)

| Event | What runs |
|---|---|
| First `ahjo <cmd>` after install | `EnsureRunning` cold path: writes plist + `launchctl bootstrap`. |
| Subsequent `ahjo <cmd>` | `EnsureRunning` hot path: 200ms healthz probe → return. |
| Mac reboot / login | `RunAtLoad=true` spawns the daemon. Next `ahjo <cmd>` confirms via probe. |
| `ahjo update` / binary moved | Next `EnsureRunning` detects stale `ProgramArguments[0]`, rewrites plist, re-bootstraps. |
| `ahjo repo add <url>` | New base container: post-start, `attachPasteShim` adds the proxy device + pushes both shims. Inherited by every COW-clone via `incus copy`. |
| `ahjo create / shell / claude <alias>` | Post-start, `attachPasteShim` runs again — idempotent. Old containers from before this feature self-heal here. |
| Lima VM restart (new gateway IP) | Next `attachPasteShim` resolves `host.lima.internal` to the new IP and re-adds the device. |
| `ahjo nuke -y` | `paste.Unload()` boots the service out and deletes the plist. |

## Verifying it works

End-to-end from the macOS host:

```sh
# 1. Daemon is running
curl -sS http://127.0.0.1:18340/healthz                 # → ok
launchctl print gui/$(id -u)/net.ahjo.paste-daemon \
  | head -5                                              # → state = running

# 2. Container has the proxy device + shims
limactl shell ahjo -- incus config device show <ctr> \
  | grep -A4 ahjo-paste-daemon
limactl shell ahjo -- incus exec <ctr> -- \
  ls -l /usr/local/bin/xclip /usr/local/bin/wl-paste

# 3. Simulate Claude's probe sequence end-to-end
osascript -e 'tell application "System Events" to \
  set the clipboard to (read POSIX file "/tmp/x.png" as «class PNGf»)'

#   step 1: targets — expect a line "image/png", exit 0
limactl shell ahjo -- incus exec <ctr> --user 1000 -- \
  xclip -selection clipboard -t TARGETS -o

#   step 2: bytes — expect identical SHA to the host file
limactl shell ahjo -- incus exec <ctr> --user 1000 -- \
  xclip -selection clipboard -t image/png -o | sha1sum
sha1sum /tmp/x.png

#   step 3: with empty clipboard, both queries exit 1
printf '' | pbcopy
limactl shell ahjo -- incus exec <ctr> --user 1000 -- \
  xclip -selection clipboard -t TARGETS -o; echo "exit=$?"
```

Inside `ahjo claude`: take a screenshot with `Cmd+Shift+Ctrl+4` (region,
clipboard mode), press `Ctrl+V` at the Claude prompt — Claude should show
the image attached.

## Troubleshooting

**`ahjo claude` runs but Ctrl+V image paste does nothing.**

Start with the logs — both sides record every interaction:

```sh
# inside container — what Claude actually invoked
limactl shell ahjo -- incus exec <ctr> -- tail /tmp/ahjo-paste-shim.log
# on Mac — what the daemon returned
tail ~/Library/Logs/ahjo-paste-daemon.log
```

If the shim log shows a TARGETS line followed by an `image/png` line and
both `ok bytes=…`, Claude got the bytes — anything past this point is
on Claude's side. If only TARGETS shows and `miss status=204`, the
pasteboard didn't have a PNG-representable image when Claude probed
(e.g. macOS pasted text, or the screenshot landed as a file path
instead of clipboard bytes).

If the shim log is empty after a `Ctrl+V` attempt, Claude isn't reaching
the shim. Walk down the chain:

1. `curl http://127.0.0.1:18340/healthz` on the Mac.
   - Connection refused → daemon not running. `launchctl print
     gui/$(id -u)/net.ahjo.paste-daemon` to see state; tail
     `~/Library/Logs/ahjo-paste-daemon.log`.
2. Inside the container: `curl http://127.0.0.1:18340/healthz`.
   - Connection refused → proxy device missing or pointing at a stale
     IP. `incus config device show <ctr> | grep -A4 ahjo-paste-daemon`.
     Run `ahjo shell <alias>` (no-op for a healthy container) to refresh.
3. `which xclip` inside the container → should print
   `/usr/local/bin/xclip`. If it prints `/usr/bin/xclip` someone
   installed real xclip; the shim is shadowed.

**Updated `ahjo` binary but the daemon still serves the old code.**
launchd keeps the existing process alive via `KeepAlive=true`; rewriting
the binary on disk doesn't replace what's in memory. Force a respawn:

```sh
launchctl bootout "gui/$(id -u)/net.ahjo.paste-daemon"
./ahjo ls > /dev/null   # any subcommand triggers EnsureRunning → re-bootstrap
```

**macOS 14+ prompts for "Paste from Other Apps" the first time.**

Expected. Grant access to `ahjo`. Unavoidable for any pasteboard reader.

**"Image attached" but Claude rejects it.** Daemon serves PNG even when
the source was JPEG (TIFF→PNG conversion via `NSBitmapImageRep`). If
Claude refuses the format, it's likely an animated/corrupted source —
re-screenshot from `Cmd+Shift+Ctrl+4`.

## Non-goals

- **Text paste.** Native terminal paste (Cmd+V in Ghostty) is untouched.
- **Linux / Windows host.** The daemon is darwin-only; `daemon_other.go`
  is a stub so the linux ahjo binary still builds.
- **Multi-image clipboard.** First PNG-representable image wins.
- **Daemon-side auth.** Loopback-only, single user — same trust model as
  cc-clip.

## Prior art

- [cc-clip](https://github.com/ShunmeiCho/cc-clip) — original "fake xclip
  shim" pattern. Transports over SSH `RemoteForward`; ahjo swaps that for
  the Lima/Incus proxy path because there's no SSH between host and
  container.
- [Matthew Tse's Hammerspoon writeup](https://substack.matthewtse.com/p/fixing-cmdv-pasted-images-in-claude)
  — Cmd+V keystroke-intercept approach. Rejected for ahjo because it
  needs Accessibility access and types in-container paths into the
  prompt, both of which the cc-clip pattern avoids.
