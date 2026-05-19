#!/bin/bash
# ahjo/pre-commit built-in Feature: ensures python3+pipx are present,
# installs pre-commit for the remote user via pipx, then warms the hook
# cache so the user's first `git commit` doesn't pay the per-hook fetch
# cost. Runs as root with the spec-defined _REMOTE_USER env var.
#
# The Feature install IS the warm-up — there is no companion
# warm-install command on the detect row. That keeps warm-install
# independent of whether the user accepted any python stack alongside.
#
# `pre-commit install-hooks` (NOT `pre-commit install`) is the
# warm-only command: it downloads each hook's repo and creates its
# virtualenv under ~/.cache/pre-commit, but does NOT write to
# .git/hooks. Installing the git hook is the user's call (a Feature
# that silently mutates .git/hooks would surprise anyone who scripts
# their own pre-push or expects the repo working tree to be untouched
# after a container build).
set -euo pipefail

: "${_REMOTE_USER:?ahjo/pre-commit: _REMOTE_USER must be set by the runner}"

# Install python3 + pipx as system packages if missing. Modern Ubuntu
# (22.04+) ships pipx in the main archive; if a future base image goes
# older, apt will fail loudly here rather than silently skipping.
need_apt=0
command -v python3 >/dev/null 2>&1 || need_apt=1
command -v pipx >/dev/null 2>&1 || need_apt=1
if [ "$need_apt" = 1 ]; then
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq python3 python3-pip pipx
fi

# pipx install as the remote user so pre-commit lands at
# ~/.local/bin/pre-commit. Ubuntu's default .profile adds ~/.local/bin
# to PATH when the directory exists, so the user's interactive shells
# pick it up automatically. `pipx ensurepath` is deliberately skipped
# — it mutates rc files, which we don't want to do silently.
sudo -iu "$_REMOTE_USER" -- pipx install pre-commit

# Warm the hook cache. Guard on the config still being present: the
# user accepted the prompt at `ahjo repo add` time, but the file could
# have been deleted between accept and install. Treat missing as a
# silent skip (pre-commit will still be installed; warming is the
# nice-to-have).
if sudo -iu "$_REMOTE_USER" -- test -f /repo/.pre-commit-config.yaml; then
    sudo -iu "$_REMOTE_USER" -- bash -c 'cd /repo && pre-commit install-hooks'
fi
