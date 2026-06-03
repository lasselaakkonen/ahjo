#!/bin/bash
# ahjo/docker built-in Feature: installs Docker Engine + the compose plugin
# into an ahjo container. Runs as root with the spec-defined Feature env
# vars (_REMOTE_USER, VERSION, CHANNEL, DAEMON_ARGS).
#
# This Feature is shipped embedded in the ahjo binary; the upstream
# docker-in-docker / docker-outside-of-docker Features declare `mounts`
# and `privileged: true`, both of which ahjo rejects because the runtime
# profile (security.nesting=true + setxattr syscall intercept,
# btrfs rootfs, systemd PID 1) already provides the kernel surface Docker
# needs. The install here just lays down get.docker.com's binaries and
# leaves dockerd at its default config — which on dockerd >=26 is the
# containerd snapshotter, whose layer whiteouts use xattrs (handled by
# security.syscalls.intercept.setxattr=true). Writing
# `{"storage-driver":"overlay2"}` here would route off the snapshotter
# onto the legacy graph driver and make dockerd refuse to start in
# snapshotter mode.
#
# `customizations.ahjo.nested_incus` is NOT required either. That opt-in
# exists for nested Incus / LXC, which need loop-mounted block-backed
# filesystems wired through /dev/loop-control + /dev/loop0..7. Docker's
# containerd snapshotter sits on the container's existing overlayfs and
# never touches /dev/loop*, so the kernel-attack-surface bump that
# nested_incus carries (see CONTAINER-ISOLATION.md) is unnecessary for
# docker-in-incus.
set -euo pipefail

: "${_REMOTE_USER:?ahjo/docker: _REMOTE_USER must be set by the runner}"

VERSION="${VERSION:-latest}"
CHANNEL="${CHANNEL:-stable}"
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
# compose up" actually needs. Its apt postinst starts dockerd; at that
# point /etc/docker/daemon.json does not exist, so dockerd uses its
# default — which on >=26 is the containerd snapshotter (overlayfs
# snapshotter, xattr whiteouts). That's the working path for ahjo, so
# we deliberately leave daemon.json absent unless the caller overrides
# via DAEMON_ARGS.
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

# Honor DAEMON_ARGS as the escape hatch for per-repo dockerd config
# overrides (log-level, registry-mirrors, insecure-registries, …, or
# for the rare caller who genuinely needs the legacy graph driver:
# `{"storage-driver":"overlay2","features":{"containerd-snapshotter":false}}`
# — both keys are required together; setting storage-driver alone in
# snapshotter mode makes dockerd refuse to start).
#
# Apt's postinst already started dockerd with no daemon.json; we have
# to restart so the merged config takes effect.
if [ -n "$DAEMON_ARGS" ]; then
    mkdir -p /etc/docker
    daemon_json="/etc/docker/daemon.json"
    existing='{}'
    if [ -s "$daemon_json" ]; then
        existing="$(cat "$daemon_json")"
    fi
    printf '%s' "$existing" | jq --argjson add "$DAEMON_ARGS" '. * $add' > "$daemon_json.tmp"
    mv "$daemon_json.tmp" "$daemon_json"
    systemctl restart docker
fi

# Smoke test as the remote user. `newgrp docker` is needed because the
# usermod above only takes effect on next login; sudo -i gives us a fresh
# session that picks up the new group.
sudo -iu "$_REMOTE_USER" -- docker version >/dev/null
