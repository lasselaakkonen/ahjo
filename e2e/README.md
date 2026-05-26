# ahjo end-to-end / integration harness (attended, host-tier)

This is a small **operator-run** bash harness that drives the **real `ahjo`
binary** through its container-lifecycle flows and validates each step against
**ground truth** — `incus`, in-container `git`, and the shell — never trusting
ahjo's own stdout or `registry.toml` as proof.

It exists because ahjo's automated suite is 100% unit + real-binary-integration
with `git`/`ssh-keygen`/`bash`; **nothing in CI drives `incus` or the `ahjo`
binary**. The container flows (`repo add`, `create`, `forward`/`expose`,
`mirror`, `shell`, `claude`, `repo pull`, `repo rm`) and the Ctrl-C
cancellation path therefore have no automated coverage. Those flows can't run in
the dev container (no incus) and need inherently human auth (a GitHub PAT, a
Claude subscription, ssh-agent/keychain), so this is the **manual host tier**:
you run it attended on a host that has already done `ahjo init`.

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
| 1 | `ahjo repo add <repo>` (live PAT + stack prompt) | container exists & **Stopped**; `/repo` on the default branch (read from `.git/HEAD`); `ahjo-ssh` proxy `connect=tcp:127.0.0.1:22`; `environment.GH_TOKEN` set |
| 2 | `ahjo create <repo> <branch>` | branch container **Running**; `/repo` on the new branch; `ahjo-ssh` re-wired; the warm-install tool present (COW-inherited); `GH_TOKEN` carried by `incus copy` |
| 3 | `ahjo forward … :<p>` then `--off` | `ahjo-forward-<p>` proxy `listen=tcp:127.0.0.1:<p>`; absent after `--off` |
| 4 | `ahjo expose … :<p>` | `ahjo-expose-<p>` proxy `connect=tcp:127.0.0.1:<p>` |
| 5 | `ahjo mirror … --target` then `off` | `mirror` disk device + `ahjo-mirror.service` active + host target populated; device gone after `off` |
| 6 | `ahjo ls`, `ahjo top` | operator confirms the listing + TUI |
| 7 | `ahjo shell <branch>` | operator confirms attach + `CLAUDE_CODE_OAUTH_TOKEN` in-shell; harness asserts `environment.GH_TOKEN` |
| 8 | `ahjo repo pull <repo>` | default container Running and `HEAD == origin/<default>` |
| 9 | `ahjo repo rm <repo> --force` | repo + branch containers absent |

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
  corroborated by the operator running `printenv` inside `ahjo shell` (step 7).

## Out of scope

- No CI integration (needs a privileged incus runner + secrets).
- No re-testing of prompt-parsing logic (already unit-tested in
  `internal/cli/lockfile_detect_test.go`).
