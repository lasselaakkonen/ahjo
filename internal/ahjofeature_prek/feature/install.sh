#!/bin/bash
# ahjo/prek built-in Feature: installs prek (a dependency-free, Rust-based
# reimplementation of pre-commit) for the remote user, then warms the hook
# cache so the user's first `git commit` doesn't pay the per-hook fetch
# cost. Runs as root with the spec-defined _REMOTE_USER env var.
#
# Why prek over pre-commit: prek ships as a single static binary with no
# Python runtime, so this Feature no longer stages python3+pip+pipx — a
# node-only / go-only repo carrying a .pre-commit-config.yaml gets its
# hooks warmed without dragging in a python surface. prek reads the
# existing .pre-commit-config.yaml unchanged.
#
# The Feature install IS the warm-up — there is no companion warm-install
# command on the detect row. That keeps warm-install independent of
# whether the user accepted any language stack alongside.
#
# `prek prepare-hooks` (NOT `prek install`) is the warm-only command: it
# downloads each hook's repo and builds its environment under the prek
# cache, but does NOT write a shim into .git/hooks. Installing the git
# hook is the user's call (a Feature that silently mutates .git/hooks
# would surprise anyone who scripts their own pre-push or expects the repo
# working tree untouched after a container build). Note `prek install
# --prepare-hooks` would warm AND write the shim — that is exactly why the
# verb here is the standalone prepare-hooks, guarded by a test.
set -euo pipefail

: "${_REMOTE_USER:?ahjo/prek: _REMOTE_USER must be set by the runner}"

# Pinned prek release. Bump deliberately — the standalone installer pulls
# this exact tag's binary rather than floating to latest, so container
# builds stay reproducible.
PREK_VERSION="0.3.13"

# The standalone installer needs curl + CA certs to fetch the release.
# Install them only if the base image lacks curl; this is the entire
# system dependency now (pre-commit needed python3+python3-pip+pipx).
if ! command -v curl >/dev/null 2>&1; then
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates
fi

# Install prek as the remote user so the binary lands in ~/.local/bin.
# Ubuntu's default .profile adds ~/.local/bin to PATH when the directory
# exists, so the user's interactive shells — and our subsequent
# login-shell invocation below — pick it up automatically.
# --no-modify-path stops the installer from editing rc files: we don't
# mutate shell config silently (same stance as skipping `pipx ensurepath`
# under the old pipx-based pre-commit setup).
sudo -iu "$_REMOTE_USER" -- sh -c \
    "curl --proto '=https' --tlsv1.2 -LsSf \
        https://github.com/j178/prek/releases/download/v${PREK_VERSION}/prek-installer.sh \
        | sh -s -- --no-modify-path"

# Warm the hook cache. Guard on the config still being present: the user
# accepted the prompt at `ahjo repo add` time, but the file could have
# been deleted between accept and install. Treat missing as a silent skip
# (prek is still installed; warming is the nice-to-have).
if sudo -iu "$_REMOTE_USER" -- test -f /repo/.pre-commit-config.yaml; then
    sudo -iu "$_REMOTE_USER" -- bash -c 'cd /repo && prek prepare-hooks'
fi
