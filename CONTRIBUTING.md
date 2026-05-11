# Contributing to ahjo

Two ways to set up a development environment. Pick one.

## Option 1 — develop inside an ahjo container (recommended)

If you already use ahjo, this is hands-off: the repo ships a
`.devcontainer/devcontainer.json` that installs Go, `golangci-lint`,
and activates the git hooks the first time the container is created.
Nothing to install on the host beyond ahjo itself.

Bootstrap once (per the [Quick start](README.md#quick-start)) so `ahjo`
is on your PATH, then:

```sh
ahjo repo add git@github.com:lasselaakkonen/ahjo --as ahjo
ahjo new ahjo master                    # creates worktree alias ahjo@master
ahjo shell ahjo@master                  # drops you into the container at /repo
```

That's it — you can `make build`, `go test ./...`, and commit. The hooks
already run.

What's provisioned automatically:

| Tool          | Source                                             |
| ------------- | -------------------------------------------------- |
| Go            | `ghcr.io/devcontainers/features/go:1`              |
| golangci-lint | `postCreateCommand` (upstream installer)           |
| git hooks     | `postCreateCommand` (runs `make hooks`)            |
| make, rg, fd, jq, yq, ast-grep, eza, httpie | `ahjo-default-dev-tools` Feature |
| git, gh       | `ghcr.io/devcontainers/features/{git,github-cli}`  |

## Option 2 — develop on the host

Install the toolchain yourself.

| Tool                      | macOS                              | Linux                              |
| ------------------------- | ---------------------------------- | ---------------------------------- |
| Go (matches `go.mod`, currently `1.26.2`) | `brew install go`      | distro package                     |
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

## Style

- Run the hooks. They reflect what CI gates.
- Conventional-commits subject lines: `fix(claude): …`, `feat(repo rm): …`, `style: …`. Match the recent `git log`.
- One concern per commit. The recent history splits hooks from formatting; keep that habit.
