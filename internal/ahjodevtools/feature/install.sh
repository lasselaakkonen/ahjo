#!/bin/bash
# ahjo-default-dev-tools devcontainer Feature: small CLI utilities ahjo's
# preferred workflows assume. Applied at base-bake time after common-utils,
# git, and github-cli; before ahjo-runtime. Tools that already have a
# curated upstream Feature live there, not here.
#
# Install methods, by tool:
#   apt:             ripgrep, fd-find, eza, httpie, make, unzip, ca-certificates
#   GitHub release:  yq (Mike Farah), ast-grep
#
# fd-find on Debian/Ubuntu installs the binary as `fdfind` (name conflict
# with another package); we symlink /usr/local/bin/fd → /usr/bin/fdfind so
# ahjo's documented `fd` invocation just works.
#
# yq is Mike Farah's Go yq, NOT the Python jq-wrapper that apt's `yq`
# package provides. The two are very different tools; ahjo's "for YAML"
# preference means Mike Farah's.
#
# ast-grep ships a static binary in app-<target>.zip under the names `sg`
# and `ast-grep`; we install only `ast-grep` because the `sg` shortcut
# collides with util-linux's `sg` (an alias for `newgrp`). Debian/Ubuntu's
# ast-grep package and Homebrew's formula skip the `sg` name for the same
# reason; ahjo follows that convention so `sg incus-admin -c …` (and any
# other newgrp use) keeps working in nested ahjo-in-ahjo containers.
#
# Versions are intentionally unpinned (`/releases/latest/download/...`) per
# ahjo's rolling-current-toolchains convention: each `ahjo update` rebuilds
# ahjo-base with whatever's current upstream.
set -euo pipefail

apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    ripgrep fd-find eza httpie make unzip ca-certificates curl

# fd-find ships its binary as /usr/bin/fdfind on Debian/Ubuntu. Symlink to
# the upstream name so ahjo and users typing `fd` get what they expect.
ln -sf /usr/bin/fdfind /usr/local/bin/fd

# Map dpkg arch (arm64|amd64) to the naming conventions each upstream uses
# for its release assets. Set both forms once so the per-tool fetches stay
# readable.
arch="$(dpkg --print-architecture)"
case "$arch" in
    arm64)
        yq_arch=linux_arm64
        astgrep_target=aarch64-unknown-linux-gnu
        ;;
    amd64)
        yq_arch=linux_amd64
        astgrep_target=x86_64-unknown-linux-gnu
        ;;
    *)
        echo "ahjo-default-dev-tools: unsupported arch $arch" >&2
        exit 1
        ;;
esac

# yq (Mike Farah's) — single static Go binary, no tarball.
curl -fsSL "https://github.com/mikefarah/yq/releases/latest/download/yq_${yq_arch}" \
    -o /usr/local/bin/yq
chmod 0755 /usr/local/bin/yq

# ast-grep — distributed as app-${target}.zip with `sg` and `ast-grep`
# binaries at the zip's top level. Install only `ast-grep`; skipping `sg`
# preserves util-linux's `sg` (newgrp alias) which ahjo init relies on.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "https://github.com/ast-grep/ast-grep/releases/latest/download/app-${astgrep_target}.zip" \
    -o "$tmp/ast-grep.zip"
unzip -q -o "$tmp/ast-grep.zip" -d "$tmp/ast-grep"
install -m 0755 "$tmp/ast-grep/ast-grep" /usr/local/bin/ast-grep
