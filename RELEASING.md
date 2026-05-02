# Releasing

## Local builds

```sh
make build              # host binary → ./ahjo
make dist               # all four release binaries + SHA256SUMS → dist/
make clean              # removes ./ahjo and dist/
make print-version      # what `git describe` would stamp into the binary
```

The version string baked into the binary comes from `git describe --tags --always --dirty`, falling back to `dev` when there are no tags or no git checkout. Override it explicitly with `make build VERSION=v1.2.3-rc1`.

## Updating the in-VM binary

On macOS, the host binary is a thin shim that relays into a Lima VM where the real CLI lives at `/usr/local/bin/ahjo`. There are two flows:

- **End-user upgrade**: download the new Mac shim from a release, then run `ahjo init`. The install step's version check notices the mismatch and reinstalls the matching `ahjo-linux-<arch>` into the VM. No nuke needed.
- **Developer iteration**: after `make build`, run `make install-vm` to push `dist/ahjo-linux-<host-arch>` straight into the VM at `/usr/local/bin/ahjo`. This skips every other init step (Lima/Incus/COI/etc.) — use it when you've only changed Go code. Alternatively, `./ahjo init` works too: `*-dirty` and `dev` versions always reinstall, so a fresh `make build` followed by `./ahjo init` reliably refreshes the in-VM bytes even when the version string didn't change.

## Release artifacts

`make dist` produces, in `dist/`:

- `ahjo-darwin-arm64` — Apple Silicon Mac shim
- `ahjo-darwin-amd64` — Intel Mac shim
- `ahjo-linux-arm64` — in-VM CLI (Lima on Apple Silicon)
- `ahjo-linux-amd64` — in-VM CLI (Lima on Intel)
- `SHA256SUMS` — checksums for all four

All binaries are statically linked (`CGO_ENABLED=0`) and stripped (`-s -w`). Each one carries its own version via `-X main.version=$VERSION`; check it with `ahjo version`.

The Mac binaries do not embed the Linux binary. At `ahjo init` time the Mac shim resolves the matching `ahjo-linux-<arch>` by checking, in order: `$AHJO_LINUX_BIN`, sibling of self, `<self-dir>/dist/`, `~/.ahjo/cache/`, then downloads from `releases/download/<version>/` and verifies against the same release's `SHA256SUMS`. So shipping the four binaries + `SHA256SUMS` is what makes a release self-installing.

## Cutting a release

1. Confirm the commit you want to ship is on the default branch and CI is green.
2. Tag and push:
   ```sh
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```
3. The `release` workflow (`.github/workflows/release.yml`) fires on any `v*` tag and:
   - checks out the tagged commit,
   - installs the Go toolchain pinned by `go.mod`,
   - runs `make dist VERSION=$tag`,
   - creates a GitHub release with auto-generated notes,
   - uploads all four binaries plus `SHA256SUMS`.

If the workflow fails after the tag is pushed, fix forward: delete the tag (`git tag -d v0.1.0 && git push --delete origin v0.1.0`), commit the fix, re-tag.

## Verifying a downloaded binary

```sh
TAG=v0.1.0
BASE="https://github.com/lasselaakkonen/ahjo/releases/download/$TAG"
curl -fLO "$BASE/ahjo-linux-arm64"
curl -fLO "$BASE/SHA256SUMS"
shasum -a 256 -c SHA256SUMS --ignore-missing
```

## Pre-release sanity check

Before tagging, dry-run the full pipeline locally:

```sh
make clean && make dist VERSION=v0.0.0-test
file dist/ahjo-*           # confirm Mach-O vs ELF and arch for each
./dist/ahjo-darwin-arm64 version   # or whichever matches your host
```
