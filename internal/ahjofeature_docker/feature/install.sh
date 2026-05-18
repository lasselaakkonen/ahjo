#!/bin/bash
# ahjo/docker built-in Feature: installs Docker Engine + the compose plugin
# into an ahjo container. Runs as root with the spec-defined Feature env
# vars (_REMOTE_USER, VERSION, CHANNEL, STORAGE_DRIVER, DAEMON_ARGS).
#
# This Feature is shipped embedded in the ahjo binary; the upstream
# docker-in-docker / docker-outside-of-docker Features declare `mounts`
# and `privileged: true`, both of which ahjo rejects because the runtime
# profile (security.nesting=true + mknod/setxattr syscall intercepts,
# btrfs rootfs, systemd PID 1) already provides the kernel surface Docker
# needs. The install here just lays down get.docker.com's binaries and
# configures the daemon for that profile.
set -euo pipefail

: "${_REMOTE_USER:?ahjo/docker: _REMOTE_USER must be set by the runner}"

VERSION="${VERSION:-latest}"
CHANNEL="${CHANNEL:-stable}"
STORAGE_DRIVER="${STORAGE_DRIVER:-}"
DAEMON_ARGS="${DAEMON_ARGS:-}"

if command -v docker >/dev/null 2>&1; then
    echo "ahjo/docker: docker already present at $(command -v docker); skipping install"
    exit 0
fi

apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates curl jq

# get.docker.com honors $VERSION and $CHANNEL via env. The script also
# installs `docker-buildx-plugin` and `docker-compose-plugin` from
# Docker's apt repo, which is what every workflow that says "docker
# compose up" actually needs.
#
# `VERSION=latest` is the natural string for a user typing in
# devcontainer.json, but the upstream script literally greps apt-cache
# madison output for it ("ERROR: 'latest' not found amongst apt-cache
# madison results"). Translate "latest" / empty to an unset VERSION so
# the script picks the newest available release; otherwise pass through.
get_docker_args=(-u VERSION CHANNEL="$CHANNEL")
case "$VERSION" in
    ""|latest) ;;
    *) get_docker_args=(CHANNEL="$CHANNEL" VERSION="$VERSION") ;;
esac
curl -fsSL https://get.docker.com | env "${get_docker_args[@]}" sh

# Grant the remote user docker socket access. The group is created by the
# upstream packages; usermod -aG is idempotent.
if getent group docker >/dev/null; then
    usermod -aG docker "$_REMOTE_USER"
fi

# Pick a storage driver. security.nesting=true gives `overlay2` on
# btrfs/ext4; everything else falls back to fuse-overlayfs which
# works inside a userns without needing host-side mount() privileges
# the kernel doesn't grant us.
rootfs="$(findmnt -no FSTYPE / || true)"
driver="$STORAGE_DRIVER"
if [ -z "$driver" ]; then
    case "$rootfs" in
        btrfs|ext4)
            driver="overlay2"
            ;;
        *)
            driver="fuse-overlayfs"
            ;;
    esac
fi

mkdir -p /etc/docker
daemon_json="/etc/docker/daemon.json"
existing="{}"
if [ -s "$daemon_json" ]; then
    existing="$(cat "$daemon_json")"
fi

overrides="$(jq -n --arg sd "$driver" '{"storage-driver": $sd}')"
if [ -n "$DAEMON_ARGS" ]; then
    # DAEMON_ARGS is expected to be a JSON fragment users can paste; merge
    # it on top so per-repo overrides win without us inventing a schema.
    overrides="$(jq -s '.[0] * .[1]' <(printf '%s' "$overrides") <(printf '%s' "$DAEMON_ARGS"))"
fi
printf '%s' "$existing" | jq --argjson add "$overrides" '. * $add' > "$daemon_json.tmp"
mv "$daemon_json.tmp" "$daemon_json"

# systemd is PID 1 (see CONTAINER-ISOLATION.md). The installer enables
# the unit but doesn't always start it under apt's chrooted maintainer
# scripts; bring it up explicitly so the smoke test below has a daemon
# to talk to.
systemctl enable --now docker

# Smoke test as the remote user. `newgrp docker` is needed because the
# usermod above only takes effect on next login; sudo -i gives us a fresh
# session that picks up the new group.
sudo -iu "$_REMOTE_USER" -- docker version >/dev/null
