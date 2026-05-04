# TODO

## Port-slot env var (per-worktree base port)

Today auto-expose allocates one host port per detected listener from the shared
10000-10999 pool, so the Mac-side number is unpredictable across worktrees.

Idea: allocate a contiguous slot per worktree (e.g. 13000 for worktree-1, 14000
for worktree-2), forward it as `AHJO_PORT_BASE=13000` via the existing
`forward_env` mechanism, and let app code (compose templates, dev-server
configs) bind at `BASE+offset`. Then `localhost:13000` is always "app on
worktree foo" — stable across sessions.

Costs: ~100-1000 ports per worktree from a wider pool (expand range to e.g.
10000-59999) and requires the app to opt in by reading the env var. Worth doing
only if "stable Mac URLs across worktrees" becomes a real pain point.
