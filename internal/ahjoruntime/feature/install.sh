#!/bin/bash
# ahjo-runtime devcontainer Feature: produces ahjo's `ahjo-base` image when
# applied to a fresh `images:ubuntu/24.04` system container. Replaces the COI
# profile build that previously layered on top of `coi-default`.
#
# Per the devcontainer Feature contract this script runs as root with these
# env vars set by the runner (see internal/devcontainer/features.go):
#   _REMOTE_USER, _REMOTE_USER_HOME, _CONTAINER_USER, _CONTAINER_USER_HOME
# We never hardcode `ubuntu` here — the runner picks the user, and ahjo
# happens to pass `ubuntu` because that's the upstream image's canonical
# 1000-uid account. Future renames (Phase 4) update only the runner.
set -euo pipefail

: "${_REMOTE_USER:?ahjo-runtime: _REMOTE_USER must be set by the runner}"
: "${_REMOTE_USER_HOME:?ahjo-runtime: _REMOTE_USER_HOME must be set by the runner}"

# Ensure the remote user exists at UID 1000. images:ubuntu/24.04 ships with
# `ubuntu` already at 1000:1000; treat that as the happy path. Otherwise create
# the account so the runner contract holds for any base image.
if ! id -u "$_REMOTE_USER" >/dev/null 2>&1; then
    useradd --uid 1000 --user-group --create-home --shell /bin/bash "$_REMOTE_USER"
fi
remote_uid="$(id -u "$_REMOTE_USER")"
remote_gid="$(id -g "$_REMOTE_USER")"
if [ "$remote_uid" != "1000" ]; then
    echo "ahjo-runtime: $_REMOTE_USER has uid $remote_uid, expected 1000 — raw.idmap will not work" >&2
    exit 1
fi

apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    openssh-server jq curl ca-certificates gnupg git

mkdir -p /etc/ssh/ahjo-host-keys
chmod 755 /etc/ssh/ahjo-host-keys

cat > /etc/ssh/sshd_config.d/00-ahjo.conf <<'EOF'
HostKey /etc/ssh/ahjo-host-keys/ssh_host_ed25519_key
HostKey /etc/ssh/ahjo-host-keys/ssh_host_rsa_key
PasswordAuthentication no
PermitRootLogin no
PubkeyAuthentication yes
AuthorizedKeysFile .ssh/authorized_keys
ChallengeResponseAuthentication no
KbdInteractiveAuthentication no
UsePAM yes
Port 22
EOF

install -d -m 0700 -o "$remote_uid" -g "$remote_gid" "$_REMOTE_USER_HOME/.ssh"
rm -f /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub
systemctl enable ssh

# Pre-seed github.com's host keys into the system known_hosts so that
# `ahjo repo add git@github.com:…` doesn't trip the StrictHostKeyChecking
# prompt on a fresh container — ahjo runs `git clone` non-interactively via
# `incus exec`, so any prompt aborts the clone with "Host key verification
# failed". Belt-and-suspenders: also accept gitlab.com / bitbucket.org which
# users hit just as often.
{
    ssh-keyscan -t rsa,ed25519 github.com gitlab.com bitbucket.org 2>/dev/null
} > /etc/ssh/ssh_known_hosts.tmp
if [ -s /etc/ssh/ssh_known_hosts.tmp ]; then
    mv /etc/ssh/ssh_known_hosts.tmp /etc/ssh/ssh_known_hosts
    chmod 0644 /etc/ssh/ssh_known_hosts
else
    rm -f /etc/ssh/ssh_known_hosts.tmp
fi

# Node + corepack from NodeSource. images:ubuntu/24.04 ships no node; coi-default
# used to provide it via mise. Corepack reads `packageManager: pnpm@x.y.z` from
# each project's package.json and activates the pinned version on demand —
# matches what production Docker / CI sees. Suppress the corepack download
# prompt so non-interactive contexts don't hang on first use of a new pnpm.
curl -fsSL https://deb.nodesource.com/setup_lts.x | bash -
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nodejs
corepack enable
grep -q '^COREPACK_ENABLE_DOWNLOAD_PROMPT=' /etc/environment \
    || echo 'COREPACK_ENABLE_DOWNLOAD_PROMPT=0' >> /etc/environment
cat > /etc/profile.d/corepack.sh <<'EOF'
export COREPACK_ENABLE_DOWNLOAD_PROMPT=0
EOF
chmod 644 /etc/profile.d/corepack.sh

# Claude Code: install via Anthropic's native installer per the project's
# memorised convention (`curl -fsSL https://claude.ai/install.sh | bash` —
# never `npm install -g @anthropic-ai/claude-code`). Runs as the remote
# user so the binary lands at $_REMOTE_USER_HOME/.local/bin/claude with the
# user's permissions; the installer also auto-updates in the background.
#
# We then symlink it into /usr/local/bin/claude so `incus exec … claude`
# (which runs without a login shell, so ~/.profile's PATH adjustment isn't
# sourced) finds it. The symlink follows the canonical install dir, so
# auto-updates that rewrite ~/.local/bin/claude transparently apply.
runuser -u "$_REMOTE_USER" -- bash -lc 'curl -fsSL https://claude.ai/install.sh | bash'
ln -sf "$_REMOTE_USER_HOME/.local/bin/claude" /usr/local/bin/claude

# ahjo-claude-prepare: prepares a freshly-created container's claude config
# so the user's first `claude` invocation is friction-free. Plants ahjo's
# defaults the user can change later — model "opusplan" (opus in plan mode,
# sonnet for execution), effortLevel "high", trust dialog suppressed for
# /repo. All settings.json fields, so /model and /effort overwrite cleanly.
#
# Idempotent via $HOME/.ahjo-claude-prepared. Reads HOME from getent so it
# also works under `incus exec --user 1000` with a sparse environment.
# Mutates only files under the invoking user's $HOME — never the host —
# so this script is fully user-name-agnostic.
cat > /usr/local/bin/ahjo-claude-prepare <<'PREPARE'
#!/bin/bash
set -e
: "${HOME:=$(getent passwd "$(id -u)" | cut -d: -f6)}"
[ -n "$HOME" ] || { echo "ahjo-claude-prepare: HOME not resolvable" >&2; exit 1; }
marker="$HOME/.ahjo-claude-prepared"
[ -f "$marker" ] && exit 0
mkdir -p "$HOME/.claude"
[ -f "$HOME/.claude/settings.json" ] || echo '{}' > "$HOME/.claude/settings.json"
[ -f "$HOME/.claude.json" ]          || echo '{}' > "$HOME/.claude.json"

tmp=$(mktemp)
jq '. + {skipDangerousModePermissionPrompt: true, model: "opusplan", effortLevel: "high"}' \
    "$HOME/.claude/settings.json" > "$tmp" && mv "$tmp" "$HOME/.claude/settings.json"

tmp=$(mktemp)
jq '.projects["/repo"] = ((.projects["/repo"] // {}) + {hasTrustDialogAccepted: true})' \
    "$HOME/.claude.json" > "$tmp" && mv "$tmp" "$HOME/.claude.json"

touch "$marker"
PREPARE
chmod 0755 /usr/local/bin/ahjo-claude-prepare
