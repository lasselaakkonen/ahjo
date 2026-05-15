#!/bin/sh
# install.sh — fetch the latest ahjo host binary for this platform, verify it
# against the release's SHA256SUMS, and install it.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/lasselaakkonen/ahjo/master/install.sh | sh
#
# Env:
#   AHJO_VERSION  pin a tag (e.g. v0.0.1); default: latest release
#   INSTALL_DIR   install location;        default: /usr/local/bin

set -eu

REPO="lasselaakkonen/ahjo"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

main() {
    require curl
    platform=$(detect_platform)
    tag=$(resolve_tag "${AHJO_VERSION:-}")
    asset="ahjo-${platform}"
    base="https://github.com/${REPO}/releases/download/${tag}"

    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT

    say "downloading ${asset} from ${tag}"
    fetch "${base}/${asset}"       "${tmp}/${asset}"
    fetch "${base}/SHA256SUMS"     "${tmp}/SHA256SUMS"

    say "verifying checksum"
    verify "$tmp"

    say "installing to ${INSTALL_DIR}/ahjo"
    install_binary "${tmp}/${asset}" "${INSTALL_DIR}/ahjo"

    "${INSTALL_DIR}/ahjo" --version
    say "next: run 'ahjo init'"
}

detect_platform() {
    os=$(uname -s)
    arch=$(uname -m)
    case "$os" in
        Darwin) os=darwin ;;
        Linux)  os=linux ;;
        *) die "unsupported OS: ${os} (only Darwin and Linux are released)" ;;
    esac
    case "$arch" in
        arm64|aarch64) arch=arm64 ;;
        x86_64|amd64)  arch=amd64 ;;
        *) die "unsupported arch: ${arch} (only arm64 and amd64 are released)" ;;
    esac
    printf '%s-%s\n' "$os" "$arch"
}

# Resolve the tag once and pin both downloads to /releases/download/<tag>/, so
# `latest` rolling forward mid-script can't mismatch the binary and SHA256SUMS.
resolve_tag() {
    pinned=$1
    if [ -n "$pinned" ]; then
        printf '%s\n' "$pinned"
        return
    fi
    tag=$(curl -fsSI "https://github.com/${REPO}/releases/latest/download/SHA256SUMS" \
        | tr -d '\r' \
        | sed -n 's|^[Ll]ocation: .*/releases/download/\([^/]*\)/SHA256SUMS.*|\1|p')
    [ -n "$tag" ] || die "could not resolve latest release tag from GitHub"
    printf '%s\n' "$tag"
}

fetch() {
    url=$1
    out=$2
    curl -fsSL --retry 3 --retry-delay 1 -o "$out" "$url" \
        || die "download failed: ${url}"
}

verify() {
    dir=$1
    if command -v sha256sum >/dev/null 2>&1; then
        (cd "$dir" && sha256sum -c --ignore-missing SHA256SUMS) >/dev/null \
            || die "checksum mismatch"
    elif command -v shasum >/dev/null 2>&1; then
        (cd "$dir" && shasum -a 256 -c --ignore-missing SHA256SUMS) >/dev/null \
            || die "checksum mismatch"
    else
        die "need sha256sum or shasum on PATH to verify the download"
    fi
}

install_binary() {
    src=$1
    dst=$2
    chmod 0755 "$src"
    dir=$(dirname "$dst")
    if [ -w "$dir" ]; then
        mv -f "$src" "$dst"
    elif command -v sudo >/dev/null 2>&1; then
        sudo install -m 0755 "$src" "$dst"
    else
        die "no write access to ${dir} and no sudo on PATH; set INSTALL_DIR=<writable dir> and retry"
    fi
}

require() {
    command -v "$1" >/dev/null 2>&1 || die "${1} is required but not on PATH"
}

say() { printf '  %s\n' "$1" >&2; }
die() { printf 'error: %s\n' "$1" >&2; exit 1; }

main "$@"
