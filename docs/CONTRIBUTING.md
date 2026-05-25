# Contributing to ahjo

Two ways to set up a development environment. Pick one.

## Option 1 — develop inside an ahjo container (recommended)

If you already use ahjo, this is hands-off: the repo ships a
`.ahjo/ahjocontainer.json` that declares the Go toolchain and, on first
container create, runs `make generate-mirror && make hooks` to activate the
git hooks. The rest of the dev toolchain (CLI tools, `git`/`gh`, base
utilities) is baked into the `ahjo-base` image. Nothing to install on the
host beyond ahjo itself.

Bootstrap once (per the [Quick start](README.md#quick-start)) so `ahjo`
is on your PATH, then:

```sh
ahjo repo add git@github.com:lasselaakkonen/ahjo --as ahjo
ahjo create ahjo master                 # creates worktree alias ahjo@master
ahjo shell ahjo@master                  # drops you into the container at /repo
```

That's it — you can `make build`, `go test ./...`, and commit. The hooks
already run.

What's provisioned automatically:

| Tool          | Source                                             |
| ------------- | -------------------------------------------------- |
| Go            | `ghcr.io/devcontainers/features/go:1` (declared in `.ahjo/ahjocontainer.json`) |
| git hooks     | `postCreateCommand` (runs `make generate-mirror && make hooks`) |
| make, rg, fd, yq, ast-grep, eza, httpie, rtk | `ahjo-default-dev-tools` Feature (baked into `ahjo-base`) |
| jq, curl, unzip, gnupg, ca-certificates | `ghcr.io/devcontainers/features/common-utils:2` (baked into `ahjo-base`) |
| git, gh       | `ghcr.io/devcontainers/features/{git,github-cli}` (baked into `ahjo-base`) |

`golangci-lint` is **not** auto-installed in the container — the pre-commit
hook soft-skips it when it's missing (see [What the hooks check](#what-the-hooks-check)).
Install it inside the container with the upstream installer (see Option 2
below) if you want the full lint pass locally.

## Option 2 — develop on the host

Install the toolchain yourself.

| Tool                      | macOS                              | Linux                              |
| ------------------------- | ---------------------------------- | ---------------------------------- |
| Go (matches `go.mod`, currently `1.26.3`) | `brew install go`      | distro package                     |
| `golangci-lint`           | `brew install golangci-lint`       | distro package or upstream         |
| `make`                    | preinstalled (CLT)                 | distro package                     |

Upstream `golangci-lint` installer (works anywhere; lands the binary in
`$(go env GOPATH)/bin`, which must be on your `PATH`):

```sh
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
  | sh -s -- -b "$(go env GOPATH)/bin"
```

Then, from the repo root:

```sh
make generate-mirror                    # build the embedded linux daemon binaries (gitignored)
make hooks                              # activate pre-commit + pre-push
```

`generate-mirror` cross-compiles the two `ahjo-mirror` daemons into
`internal/ahjoruntime/feature/`, where the host CLI's `//go:embed` picks
them up. The bytes change on every build (Go embeds a fresh BuildID per
compile), so they're kept out of git — every `make build`, hook run, and
CI job regenerates them on demand.

## What the hooks check

`make hooks` points `core.hooksPath` at `.githooks/`. Both hooks short-circuit
on first failure.

| Hook         | Runs                                                                  | Cold time |
| ------------ | --------------------------------------------------------------------- | --------- |
| `pre-commit` | `make generate-mirror`, `gofmt -l`, `go vet`, `golangci-lint run`, `go test ./...` | ~5s |
| `pre-push`   | `make generate-mirror`, `go test -race ./...`                         | ~15s      |

Both hooks call `make generate-mirror` first so the gitignored
`ahjo-mirror.linux-{amd64,arm64}` daemons (embedded into the host CLI
via `//go:embed`) are present before vet/test compile. The make rule is
incremental — no-op when binaries are already newer than their Go sources.

Bypass:

- `SKIP_HOOKS=1 git commit ...` / `SKIP_HOOKS=1 git push ...` — graceful skip, prints a notice
- `git commit --no-verify` / `git push --no-verify` — hard skip

CI (`.github/workflows/ci.yml`) gates merges on `make generate-mirror`,
`go vet`, and `go test`. The pre-push hook catches everything CI would,
locally.

## Day-to-day commands

```sh
make build              # produces ./ahjo (host arch)
go test ./...           # full unit suite (~4.5s cold)
go test -race ./...     # what pre-push runs (~15s cold)
golangci-lint run       # what pre-commit runs
gofmt -w .              # format in place
```

For rebuilding the `ahjo-base` image or recreating containers after a
change, see the
[Rebuilding after a change](README.md#rebuilding-after-a-change) section
in the README.

## Full nuke — start completely fresh

When you want to wipe **everything** ahjo created — state, configs, the VM /
containers, and on Linux the system packages `ahjo init` installed — so you can
test `install.sh` and `ahjo init` from zero.

`ahjo nuke -y` is the first move, but it intentionally **keeps** your configs
and tokens (`~/.ahjo/{config.toml,profiles,.env}` and all of
`~/.ahjo-shared/`). It clears the branch and repo entries from
`~/.ahjo/registry.toml` but leaves the file itself in place. A full nuke
removes those too.

### macOS

Deleting the `ahjo` Lima VM takes all in-VM state with it — incus, images,
containers, the in-VM `ahjo`, the zabbly repo, sysctl, subuid/subgid, and the
`incus-admin` group all live inside the disposable VM. Only the host side is
left to clean.

```sh
ahjo nuke -y                    # deletes the "ahjo" Lima VM, ~/.ahjo/cache, the
                                # paste-daemon launchd agent, and the
                                # ~/.ssh/config Include block
rm -rf ~/.ahjo ~/.ahjo-shared   # the configs + tokens nuke keeps
```

If `ahjo nuke` can't run (broken build, half-applied change), do it by hand:

```sh
limactl stop -f ahjo && limactl delete -f ahjo
launchctl bootout gui/$(id -u)/net.ahjo.paste-daemon 2>/dev/null || true
rm -f ~/Library/LaunchAgents/net.ahjo.paste-daemon.plist
# in ~/.ssh/config, delete the block between
#   # >>> ahjo-managed >>>   and   # <<< ahjo-managed <<<
rm -rf ~/.ahjo ~/.ahjo-shared
```

Optionally drop the binary too (skip if you dev from a `make build` symlink):

```sh
sudo rm -f /usr/local/bin/ahjo
```

Verify: `limactl list` shows no `ahjo`, and `ls ~/.ahjo ~/.ahjo-shared` errors.

### Linux (standalone host)

incus runs directly on the host, so a full nuke must also undo the system
bootstrap `ahjo init` performed — `ahjo nuke -y` only removes containers,
images, and host-keys.

```sh
# 1. ahjo's own teardown: containers, ahjo-base/ahjo-osbase images, host-keys
ahjo nuke -y

# 2. ahjo state + configs/tokens in your home
rm -rf ~/.ahjo ~/.ahjo-shared

# 3. the system layer init installed
sudo apt-get purge -y incus
sudo apt-get autoremove --purge -y
sudo rm -rf /var/lib/incus                         # incusbr0, storage pool, profiles
sudo rm -f /etc/sysctl.d/99-ahjo.conf && sudo sysctl --system
sudo rm -f /etc/apt/sources.list.d/zabbly-incus-stable.sources \
           /etc/apt/keyrings/zabbly.asc
sudo apt-get update
sudo sed -i "/^root:$(id -u):1$/d" /etc/subuid     # grants init appended
sudo sed -i "/^root:$(id -g):1$/d" /etc/subgid
```

The `incus-admin` group is created and removed with the incus package, so
purging it drops your membership. If incus is too wedged for `ahjo nuke` to run,
skip step 1 — `apt-get purge incus` plus `rm -rf /var/lib/incus` in step 3 takes
every container and image with it.

### Reinstall to test

```sh
curl -fsSL https://raw.githubusercontent.com/lasselaakkonen/ahjo/master/install.sh | sh
ahjo init
```

## Style

- Run the hooks. They reflect what CI gates.
- Conventional-commits subject lines: `fix(claude): …`, `feat(repo rm): …`, `style: …`. Match the recent `git log`.
- One concern per commit. The recent history splits hooks from formatting; keep that habit.
