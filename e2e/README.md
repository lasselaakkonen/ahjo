# ahjo end-to-end / integration harness (attended, host-tier)

This is a small **operator-run** bash harness that drives the **real `ahjo`
binary** through its container-lifecycle flows and validates each step against
**ground truth** — `incus`, in-container `git`, and the shell — never trusting
ahjo's own stdout or `registry.toml` as proof.

It exists because ahjo's automated suite is 100% unit + real-binary-integration
with `git`/`ssh-keygen`/`bash`; **nothing in CI drives `incus` or the `ahjo`
binary**. The container flows (`env`, `repo add`, `create`, `forward`/`expose`,
`mirror`, `shell`, `ssh`, `claude`, `ide`, `rm`, `repo pull`, `repo rm`) — with
their notable flags (`--as`, `--base`, `--default-base`, `mirror --no-revert`) —
and the Ctrl-C cancellation path therefore have no automated coverage. Those
flows can't run in the dev container (no incus) and need inherently human auth (a
GitHub PAT, a Claude subscription, ssh-agent/keychain), so this is the **manual
host tier**: you run it attended on a host that has already done `ahjo init`.

## Prerequisites

- A host with **`ahjo init` already done** and the global incus state present
  (the `ahjo-base` / `ahjo-osbase` images, subuid/subgid, incus-admin group):
  - **Native Linux**: incus running locally.
  - **macOS**: the Lima VM (default name `ahjo`) running; `limactl` on PATH.
    Validation reaches into the VM automatically (see "Platform" below).
- A **private sandbox repo** you control, cloneable over **HTTPS with a PAT**
  (default `lasselaakkonen/ahjo-e2e-sandbox`). It should carry a **lockfile**
  (e.g. `go.sum`, `package-lock.json`) so stack-detection + warm-install are
  exercised.
- A **GitHub PAT** with `repo` scope (pasted live at the `repo add` prompt).
- A **Claude subscription / token** on hand for the `ahjo claude` step.
- `jq` on the host (and `nc`/`curl` for the optional port checks). On macOS,
  `limactl`.

## The binary under test

The harness always calls **`$AHJO_BIN`** — point it at your working-tree build:

```sh
make build           # produces ./ahjo
export AHJO_BIN=./ahjo
```

## Running it

```sh
# 1. main attended lifecycle — follow the prompts
AHJO_BIN=./ahjo bash e2e/lifecycle.sh

# 2. Ctrl-C / cancellation checks
#    export GH_TOKEN=<pat> first to run the repo-add cancel unattended;
#    otherwise it's attended (you press Ctrl-C mid-clone)
GH_TOKEN=<pat> AHJO_BIN=./ahjo bash e2e/cancel.sh

# 3. OPT-IN, rebuilds your real ahjo-base
AHJO_BIN=./ahjo bash e2e/build.sh
```

Each checkpoint prints `✓`/`✗` from its `incus`/`git`/shell validation. Any
failure dumps the validation output and exits non-zero. The harness **pre-cleans
any leftover sandbox state on startup** and **tears down automatically on exit**
(success, failure, or Ctrl-C) — so a prior run that was killed before its
teardown can't poison the next one.

## What each script verifies

### `lifecycle.sh` — the main run

| Step | Real command | Ground-truth assertion |
|---|---|---|
| 1 | `ahjo env set/get/list[--show]/unset` | host-side CRUD round-trips; `list` masks the value, `--show` reveals it; `get` fails after `unset` (no container needed) |
| 2 | `ahjo repo add <repo> --as <alias>` (live PAT + stack prompt) | container exists & **Stopped**; `/repo` on the default branch (read from `.git/HEAD`); `ahjo-ssh` proxy `connect=tcp:127.0.0.1:22`; `environment.GH_TOKEN` set; the `--as` alias present in the generated alias map; `ahjo repo ls` lists the repo |
| 3 | `ahjo create <repo> <branch> --as <alias>` | branch container **Running**; `/repo` on the new branch; `ahjo-ssh` re-wired; the warm-install tool present (COW-inherited); `GH_TOKEN` carried by `incus copy`; the `--as` branch alias in the alias map |
| 3.1 | `ahjo doctor` | output carries the incus-backed checks — `incus on PATH` and `ahjo-base image present` (exit code not gated: the isolated HOME has no OAuth token / git identity, so a non-zero exit is expected). On macOS the relayed in-VM block supplies the same lines |
| 3.2 | `ahjo gc` / `ahjo gc --older-than 0` | default 24h window reports `no candidates` (the branch is seconds old); `--older-than 0` reports the branch as a **dry-run** candidate; the branch container still **Running** afterward (report mode never deletes; the default/COW-source branch is always excluded) |
| 4 | `ahjo forward … :<p>` then `--off` | `ahjo-forward-<p>` proxy `listen=tcp:127.0.0.1:<p>`; absent after `--off` |
| 5 | `ahjo expose … :<p>`, then `expose … --sync` | `ahjo-expose-<p>` proxy `connect=tcp:127.0.0.1:<p>`; `--sync` runs cleanly and **leaves the manual expose device intact** (sync only reconciles auto-expose entries, never manual ones) |
| 6 | `ahjo mirror … --target`, `status`, `logs`, `off --no-revert`, `revert` | `mirror` disk device + `ahjo-mirror.service` active + host target populated; **a file edited inside the container appears on the host** (live watch→push); `status` reports it active; `off --no-revert` removes the device but **keeps** the mirrored file; `mirror revert <target>` then restores the pre-mirror snapshot, **removing** that kept file (the orphan-recovery path) |
| 7 | `ahjo ls`, `ahjo top` | operator confirms the listing + TUI |
| 8 | `ahjo shell <branch>` | operator confirms attach + `CLAUDE_CODE_OAUTH_TOKEN` in-shell; harness asserts `environment.GH_TOKEN` |
| 9 | `ahjo ssh <as-alias>` | connects over the generated ssh-config and runs `id -un` → `ubuntu` (machine-asserted; `StrictHostKeyChecking=yes` proves it reached *this* container) |
| 10 | `ahjo claude <branch>` | operator confirms `claude` launched in the container |
| 11 | `ahjo ide <as-alias>` | operator confirms an IDE opened over ssh-remote (or ahjo's clean "no SSH-capable IDEs" error on a headless host) |
| 12 | `ahjo create <repo-as-alias> <branch2> --base <ref>` | second branch **Running** via the repo's `--as` alias; `/repo` HEAD == `<ref>` (defaults to `origin/<default>~1`, proving `--base` plumbs a ref through) |
| 13 | `ahjo rm <branch2-alias>` | branch2 container absent; repo + first branch container still present (standalone single-branch teardown) |
| 14 | `ahjo repo pull <repo>` | default container Running and `HEAD == origin/<default>` |
| 14.1 | `ahjo repo set-token <alias>` (Linux only) | a sentinel token piped on stdin lands as both `environment.GH_TOKEN` **and** `environment.GITHUB_TOKEN` on the repo container (read back via `incus config get`). Runs after `repo pull` (last token-dependent step) so the overwrite is harmless; **skipped on macOS** (the set-token path consumes the relayed Keychain value — operator territory) |
| 15 | `ahjo repo rm <repo> --force` | repo + branch containers absent |
| 16 | `ahjo repo add … --default-base <alt>` (opt-in) | repo container checked out on `<alt>`; **skipped unless `AHJO_E2E_ALT_BRANCH` is set** to a real non-default remote branch |

### `cancel.sh` — cancellation (primary check of `81fb1f5`)

- **`repo add`** interrupted mid clone/warm-install → ahjo exits **non-zero and
  promptly** (a poll-with-deadline catches a hang), `ahjo ls` then succeeds
  (lockfile released), and the half-built orphan container is cleanly removable.
- **`update`** interrupted mid-build → prompt non-zero exit and **no wedged
  `ahjo-build-<rand>`** container (cancels before publish, so `ahjo-base` is
  untouched; gated behind a confirm).

### `build.sh` — OPT-IN, rebuilds `ahjo-base`

`ahjo update -y`, then a throwaway container launched **straight from the
rebuilt `ahjo-base`** has its embedded-Feature tools probed with `command -v`
(`rg fd eza yq ast-grep http make` from ahjo-devtools; `claude
ahjo-claude-prepare ahjo-mirror sshd` from ahjo-runtime). Probing the image
directly tests exactly what `update` produced and needs no PAT.

## Platform abstraction

`lib.sh` provides `incusq`: validation commands run **locally** on native Linux,
and are wrapped with `limactl shell "$AHJO_VM" --` on macOS (where the real
`~/.ahjo` and all containers live inside the Lima VM). `ahjo` itself is always
invoked directly — the darwin launcher relays into the VM on its own. So every
assertion works on both hosts unchanged.

## Safety model (read this)

- **Isolated state on Linux**: the harness relocates `HOME` to a throwaway
  `mktemp` dir, so all of `~/.ahjo` + `~/.ahjo-shared` is fresh and your real
  state is untouched. The dir is `rm -rf`'d at teardown.
- **macOS**: the in-VM `~/.ahjo` is a shared singleton, so there's no per-run
  HOME isolation there. Isolation rests on the **unique sandbox slug** + the
  **`repo rm` / targeted sweep** teardown. `AHJO_HOST_HOME` is left untouched so
  the claude-config push still reads your real Mac home.
- **Targeted teardown**: `ahjo repo rm <repo> --force`, then a sweep that
  `incus delete --force`es only containers matching the sandbox slug prefix
  (`ahjo-<sandbox-slug>`). `safe_sweep` **refuses any prefix less specific than
  the sandbox slug** (and bare `ahjo-`), so a typo can never enumerate your real
  containers.
- **Pre-clean on startup**: the same `repo rm --force` + targeted sweep runs
  once *before* the run begins. The macOS in-VM `~/.ahjo` is a shared singleton
  and even the Linux throwaway HOME resets only the registry (not the global
  incus containers), so a container left by a previous run that never reached
  its teardown would otherwise make `repo add` suffix its slug to `-2` and
  desync every derived container name. Pre-cleaning makes each run idempotent.
- **NEVER `ahjo nuke`**: nuke deletes the global `ahjo-base`/`ahjo-osbase`
  images (see `internal/cli/nuke.go`). The harness never calls it. Only
  `build.sh` intentionally rebuilds `ahjo-base`, and only after a confirm.

## Configuration (environment overrides)

| Variable | Default | Meaning |
|---|---|---|
| `AHJO_BIN` | *(required)* | the ahjo binary under test |
| `AHJO_VM` | `ahjo` | Lima VM name (macOS only) |
| `AHJO_E2E_REPO` | `lasselaakkonen/ahjo-e2e-sandbox` | sandbox repo (HTTPS+PAT alias) |
| `AHJO_E2E_BRANCH` | `e2e-test-branch` | branch `create` makes |
| `AHJO_E2E_WARM_TOOL` | `go` | tool the lockfile's warm-install provides |
| `AHJO_E2E_EXPECT_GH_TOKEN` | `1` | assert per-repo `GH_TOKEN` env landed (set `0` if running `--yes`/no PAT) |
| `AHJO_E2E_FWD_PORT` | `8000` | `forward` port |
| `AHJO_E2E_EXPOSE_PORT` | `3000` | `expose` container port |
| `AHJO_E2E_MIRROR_DIR` | `$HOME/ahjo-e2e-mirror-<slug>` | host mirror target |
| `AHJO_E2E_SKIP_MIRROR` | `0` | skip the mirror checkpoint |
| `AHJO_E2E_REPO_AS` | `ahjo-e2e-sandbox-alt` | extra repo alias for `repo add --as` |
| `AHJO_E2E_BRANCH_AS` | `e2e-branch-alt` | extra branch alias for `create --as` (also the alias `ahjo ssh`/`ahjo ide` are driven through) |
| `AHJO_E2E_BRANCH2` | `e2e-base-branch` | second branch for the `create --base` checkpoint |
| `AHJO_E2E_BASE_REF` | *(unset → `origin/<default>~1`)* | explicit `--base` ref; the default lands on a non-tip commit (sandbox default branch needs ≥2 commits) |
| `AHJO_E2E_ALT_BRANCH` | *(unset)* | a real non-default remote branch; set it to run the opt-in `repo add --default-base` checkpoint (skipped otherwise) |
| `GH_TOKEN` | *(unset)* | exporting it runs the `cancel.sh` repo-add cancel unattended |
| `AHJO_E2E_CANCEL_DELAY` | `3` | seconds before SIGINT in the repo-add cancel |
| `AHJO_E2E_UPDATE_CANCEL_DELAY` | `20` | seconds before SIGINT in the update cancel |
| `AHJO_E2E_CANCEL_DEADLINE` | `30` | max seconds to wait for prompt exit before declaring a hang |

## Notes on fidelity (why a couple of checks differ from the obvious)

- **Stopped-container reads.** After `repo add` the repo container is **stopped**
  (it's the COW source), so `incus exec` can't inspect it. The branch assertion
  reads `/repo/.git/HEAD` via `incus file pull`, and the env assertion reads
  `environment.GH_TOKEN` via `incus config get` — both ground truth, both work
  on a stopped container.
- **`GH_TOKEN` vs `CLAUDE_CODE_OAUTH_TOKEN`.** `GH_TOKEN`/`GITHUB_TOKEN` are
  promoted to **container config** (`installRepoToken`), so the harness can
  assert them via `incus config get` on any container. `CLAUDE_CODE_OAUTH_TOKEN`
  is a **`forward_env`** var injected only by the `shell`/`claude` attach — it's
  not container config and isn't visible to a plain `incus exec`, so it's
  corroborated by the operator running `printenv` inside `ahjo shell` (step 8).
- **`ssh` vs `ide`.** `ahjo ssh` execs `ssh` against the generated ssh-config, so
  it *is* machine-assertable: the harness feeds a command on stdin (no TTY → ssh
  runs it non-interactively) and checks the remote `id -un` is `ubuntu`;
  `StrictHostKeyChecking=yes` against the pre-seeded known_hosts means a clean
  connect already proves it reached *that* container. `ahjo ide` launches a GUI
  editor — a side effect the harness can't observe — so it stays operator-eyeball
  (and accepts ahjo's clean "no SSH-capable IDEs" error on a headless host).
- **Mirror live propagation.** Beyond the activation-time bootstrap copy, step 6
  writes a file *inside* the container and polls the host target for it, so the
  daemon's watch→push path is exercised — not just the initial sync. `off
  --no-revert` is then asserted to *keep* that mirror-added file (a plain `off`
  would revert the target and remove it).
- **`mirror logs`.** Tails `journalctl --follow` and replaces the process, so it
  can't be cleanly attached unattended (the operator's Ctrl-C would also hit the
  script). The harness captures a short background window and only **warns** if
  the journal looks empty — it's a best-effort streaming check, not a hard gate.

## Out of scope

- No CI integration (needs a privileged incus runner + secrets).
- No re-testing of prompt-parsing logic (already unit-tested in
  `internal/cli/lockfile_detect_test.go`).
- The `ahjo ide` *happy path* (an editor actually opening) is operator-confirmed,
  not machine-asserted — it's a GUI launch.
- Still no driver-level coverage for: `nuke` and `init` (both destroy or
  rebuild the global `ahjo-base`/`ahjo-osbase` images — that's `build.sh`'s job,
  never the lifecycle's), and the `shell`/`claude --update` recreate path (also
  a base-rebuild flow). `doctor`, `gc`, `repo ls`, `repo set-token` (Linux),
  `expose --sync`, and `mirror revert` are now covered by `lifecycle.sh` (steps
  3.1, 3.2, 2, 14.1, 5, 6).
