#!/bin/bash
# ahjo-default-dev-tools devcontainer Feature: small CLI utilities ahjo's
# preferred workflows assume, plus rtk (a Claude-side token-saving proxy).
# Applied at base-bake time AFTER ahjo-runtime, so rtk's `rtk init -g
# --auto-patch` finds the freshly-installed `claude` binary and the
# ~/.claude/ tree it laid down. Tools that already have a curated upstream
# Feature (common-utils, git, github-cli) live there, not here; we don't
# duplicate their apt installs.
#
# Install methods, by tool:
#   apt:             ripgrep, fd-find, eza, httpie, make
#   GitHub release:  yq (Mike Farah), ast-grep
#   user-installer:  rtk (curl|sh into ~/.local/bin)
#
# `curl`, `unzip`, and `ca-certificates` are needed by this script (rtk
# fetch, ast-grep zip extraction) but supplied by common-utils:2 applied
# earlier in the chain, so we don't apt them again here.
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

: "${_REMOTE_USER:?ahjo-default-dev-tools: _REMOTE_USER must be set by the runner}"
: "${_REMOTE_USER_HOME:?ahjo-default-dev-tools: _REMOTE_USER_HOME must be set by the runner}"

apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    ripgrep fd-find eza httpie make

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

# rtk (https://github.com/rtk-ai/rtk): token-saving CLI proxy that
# intercepts heavy command output before it reaches the model. Installed
# in this Feature (and not ahjo-runtime) because ahjo's own Go code never
# touches rtk — it's a Claude-side ergonomic, same category as ripgrep
# and eza. The base-bake apply order (ahjo-runtime first, then this
# Feature — see internal/devcontainer/build.go) guarantees `claude` and
# its ~/.claude/ tree exist by the time `rtk init -g --auto-patch` runs.
#
# Upstream installer drops the binary at $HOME/.local/bin/rtk (user-scoped,
# no root needed); we symlink into /usr/local/bin so `incus exec` (non-login
# shell, no PATH adjustment) resolves it — same pattern as claude.
#
# `rtk init -g --auto-patch` lays down ~/.claude/hooks/rtk-rewrite.sh
# and ~/.claude/RTK.md and patches ~/.claude/settings.json + CLAUDE.md
# with a hook registration. The hook script and RTK.md survive
# `ahjo repo add`'s host→container push (which intentionally excludes
# ~/.claude/hooks/ — see internal/cli/repo.go). settings.json and
# CLAUDE.md *are* overwritten from host on first repo add, so whether
# the hook is actually wired up at runtime is governed by the user's
# host-side `rtk init -g` state — which is the right authority order.
runuser -u "$_REMOTE_USER" -- bash -lc 'curl -fsSL https://raw.githubusercontent.com/rtk-ai/rtk/refs/heads/master/install.sh | sh'
ln -sf "$_REMOTE_USER_HOME/.local/bin/rtk" /usr/local/bin/rtk
runuser -u "$_REMOTE_USER" -- bash -lc 'rtk init -g --auto-patch'
