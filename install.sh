#!/bin/sh
# install.sh — fetch the latest ahjo host binary for this platform, verify it
# against the release's SHA256SUMS, and install it.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/lasselaakkonen/ahjo/master/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/lasselaakkonen/ahjo/master/install.sh | sh -s -- --install-dir <dir>
#
# Options:
#   --install-dir <dir>  install location; default: $HOME/.local/bin
#
# Env:
#   AHJO_VERSION  pin a tag (e.g. v0.0.1); default: latest release
#   INSTALL_DIR   same as --install-dir   (the flag takes precedence)

set -eu

REPO="lasselaakkonen/ahjo"
INSTALL_DIR="${INSTALL_DIR:-}"   # from env if set; --install-dir overrides; default applied after parse

main() {
    parse_args "$@"
    : "${INSTALL_DIR:="$HOME/.local/bin"}"
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
    check_path "$INSTALL_DIR"
    say "next: run 'ahjo init'"
}

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --install-dir)
                [ $# -ge 2 ] || die "--install-dir requires a directory"
                INSTALL_DIR=$2; shift 2 ;;
            --install-dir=*)
                INSTALL_DIR=${1#*=}; shift ;;
            -h|--help)
                usage; exit 0 ;;
            *)
                die "unknown argument: $1 (see --help)" ;;
        esac
    done
}

usage() {
    cat >&2 <<EOF
install.sh — install the ahjo binary for this platform

Usage:
  curl -fsSL https://raw.githubusercontent.com/${REPO}/master/install.sh | sh
  curl -fsSL https://raw.githubusercontent.com/${REPO}/master/install.sh | sh -s -- --install-dir <dir>

Options:
  --install-dir <dir>   install location (default: \$HOME/.local/bin)
  -h, --help            show this help

Env:
  AHJO_VERSION   pin a release tag (e.g. v0.0.1); default: latest
  INSTALL_DIR    same as --install-dir (the flag takes precedence)
EOF
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
    dir=$(dirname "$dst")
    chmod 0755 "$src"

    # Unprivileged path: create the dir (if missing) and move into place.
    if mkdir -p "$dir" 2>/dev/null && [ -w "$dir" ]; then
        mv -f "$src" "$dst"
        return
    fi

    # Needs root to create or write the directory.
    command -v sudo >/dev/null 2>&1 \
        || die "no write access to ${dir} and no sudo on PATH; re-run with --install-dir=<writable dir>, e.g. --install-dir=\"\$HOME/.local/bin\""

    explain_sudo "$dir"
    sudo mkdir -p "$dir"
    sudo install -m 0755 "$src" "$dst"
}

# Printed ABOVE the sudo password prompt so the user knows the escape hatch.
explain_sudo() {
    dir=$1
    {
        printf '\n'
        printf '  %s needs administrator rights; you will be prompted for your password.\n' "$dir"
        printf '  to install without sudo, pass a writable directory instead, e.g.:\n'
        printf '    curl -fsSL https://raw.githubusercontent.com/%s/master/install.sh | sh -s -- --install-dir "$HOME/.local/bin"\n' "$REPO"
        printf '\n'
    } >&2
}

# Warn if the install dir isn't on PATH, so the user knows why `ahjo` won't be
# found, and hand them a shell-specific one-liner to fix it.
check_path() {
    dir=$1
    case ":${PATH}:" in
        *":${dir}:"*) return 0 ;;
    esac

    # Show $HOME rather than the expanded path in anything we ask them to paste.
    display=$dir
    case "$dir" in
        "$HOME"/*) display="\$HOME${dir#"$HOME"}" ;;
    esac

    printf '\n' >&2
    printf '  %s is not on your PATH, so your shell will not find the `ahjo` command yet.\n' "$display" >&2
    case "$(basename "${SHELL:-}")" in
        zsh)
            printf '  add it to ~/.zshrc, then restart your shell (or run: source ~/.zshrc):\n\n' >&2
            printf "    echo 'export PATH=\"%s:\$PATH\"' >> ~/.zshrc\n\n" "$display" >&2 ;;
        bash)
            printf '  add it to ~/.bashrc, then restart your shell (or run: source ~/.bashrc):\n\n' >&2
            printf "    echo 'export PATH=\"%s:\$PATH\"' >> ~/.bashrc\n\n" "$display" >&2 ;;
        fish)
            printf '  add it with fish_add_path (persists across sessions):\n\n' >&2
            printf '    fish_add_path %s\n\n' "$display" >&2 ;;
        *)
            printf '  add it to your shell startup file, then restart your shell:\n\n' >&2
            printf '    export PATH="%s:$PATH"\n\n' "$display" >&2 ;;
    esac
}

require() {
    command -v "$1" >/dev/null 2>&1 || die "${1} is required but not on PATH"
}

say() { printf '  %s\n' "$1" >&2; }
die() { printf 'error: %s\n' "$1" >&2; exit 1; }

main "$@"
