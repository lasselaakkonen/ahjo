# ahjo

You are running inside an Incus container managed by **ahjo**, a tool that spins
up one Linux container per git branch. ahjo can wire three host↔container
bridges. They are controlled from the **host**, not from inside this container,
so you cannot toggle them yourself — but you should know which are active.

- **mirror** — copies file creations/modifications from this container's `/repo`
  out to a directory on the host (deletions are *not* replicated). Enabled on
  the host with `ahjo mirror <alias> --target <dir>`.
- **expose** — publishes a port from this container out to the host's
  `127.0.0.1`. Enabled with `ahjo expose <alias> <container-port>`.
- **forward** — pipes a host port *into* this container on `127.0.0.1` (the
  inbound counterpart of expose), so code that hardcodes a loopback address can
  reach a host service unmodified. Enabled with
  `ahjo forward <alias> <host-port>`.

## Current state

This file does **not** embed the current bridge state, on purpose: the user can
turn mirror/expose/forward on or off from the host at any time, so any snapshot
pasted here would be stale the moment your session starts.

Instead, when the current state matters — e.g. before assuming a port is or
isn't reachable — **read `~/.ahjo/ahjo-state.md`**. The host rewrites that file
on every change, so a fresh read always reflects reality. A machine-readable
twin sits next to it at `~/.ahjo/ahjo-state.json` if you'd rather parse it (e.g.
`jq '.expose' ~/.ahjo/ahjo-state.json`).
