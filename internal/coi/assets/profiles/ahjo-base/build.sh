#!/bin/bash
set -euo pipefail

apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssh-server jq

mkdir -p /etc/ssh/ahjo-host-keys
chown root:root /etc/ssh/ahjo-host-keys
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

install -d -m 0700 -o code -g code /home/code/.ssh
rm -f /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub
systemctl enable ssh

# ahjo-claude-prepare: prepares a freshly-created container's claude config so
# the user's first `claude` invocation is friction-free.
#
# It does two things:
#
#   1. Strips COI's `env.CLAUDE_CODE_EFFORT_LEVEL` injection. COI's claude
#      integration writes that env-var block into both ~/.claude/settings.json
#      and ~/.claude.json on session setup. Because env-var values take the
#      highest precedence in claude's effort resolution, leaving the block in
#      place would lock /effort to whatever value COI wrote — the user could
#      not lower or raise it from the TUI without seeing "X overrides this
#      session" forever. We delete just the CLAUDE_CODE_EFFORT_LEVEL key (not
#      the surrounding env object — claude lets users put their own env vars
#      there) and drop the env object entirely if it ends up empty.
#
#   2. Plants ahjo's defaults the user *can* change later: model "opusplan"
#      (opus in plan mode, sonnet for execution) and effortLevel "high".
#      Both are normal settings.json fields, so /model and /effort overwrite
#      them cleanly. Also sets the prompt suppressors that silence the
#      "trust this directory?" and "--dangerously-skip-permissions" prompts
#      on first run.
#
# Invoked once by `ahjo shell` immediately after COI's first container
# creation, via `coi container exec --user 1000`, before claude ever launches.
# Idempotent via $HOME/.ahjo-claude-prepared. Mutates only files under the
# invoking user's $HOME — never the host. /workspace is COI's hardcoded
# workspace mount.
cat > /usr/local/bin/ahjo-claude-prepare <<'PREPARE'
#!/bin/bash
set -e
# `coi container exec --user 1000` runs without HOME set; resolve from passwd
# so the script also works when invoked with a sparse environment.
: "${HOME:=$(getent passwd "$(id -u)" | cut -d: -f6)}"
[ -n "$HOME" ] || { echo "ahjo-claude-prepare: HOME not resolvable" >&2; exit 1; }
marker="$HOME/.ahjo-claude-prepared"
[ -f "$marker" ] && exit 0
mkdir -p "$HOME/.claude"
[ -f "$HOME/.claude/settings.json" ] || echo '{}' > "$HOME/.claude/settings.json"
[ -f "$HOME/.claude.json" ]          || echo '{}' > "$HOME/.claude.json"

# Strip COI's CLAUDE_CODE_EFFORT_LEVEL env-var injection from both files,
# remove the env object if that key was the only one in it, then plant our
# defaults. Two separate jq pipelines keep the merges readable.
strip_env='del(.env.CLAUDE_CODE_EFFORT_LEVEL) | if (.env // {}) == {} then del(.env) else . end'

tmp=$(mktemp)
jq "$strip_env"' + {skipDangerousModePermissionPrompt: true, model: "opusplan", effortLevel: "high"}' \
    "$HOME/.claude/settings.json" > "$tmp" && mv "$tmp" "$HOME/.claude/settings.json"

tmp=$(mktemp)
jq "$strip_env"' | .projects["/workspace"] = ((.projects["/workspace"] // {}) + {hasTrustDialogAccepted: true})' \
    "$HOME/.claude.json" > "$tmp" && mv "$tmp" "$HOME/.claude.json"

touch "$marker"
PREPARE
chmod 0755 /usr/local/bin/ahjo-claude-prepare

# pnpm: hand management from mise to corepack so package.json's
# `packageManager: pnpm@x.y.z` is honored per-project. coi-default installs
# pnpm via `mise use --global npm:pnpm@latest`, which is just npm's "latest"
# and ignores the pin. Corepack's shim downloads + activates the pinned
# version on demand and falls back to current "latest" outside any project.
#
# Why we install-then-uninstall pnpm: ahjo-base is a layered image on top of
# coi-default. By the time this script runs, the parent layer has already
# installed pnpm via mise (we can't intercept that without forking COI).
# So we remove what coi-default installed, in this layer.
#
# This is not a fight with mise — it aligns with mise's own upstream
# guidance. The mise project's Node cookbook recipe explicitly recommends
# `corepack enable` (via mise hooks) for Node projects rather than
# installing pnpm as a mise tool, because:
#   - corepack reads `packageManager: pnpm@x.y.z` from each project's
#     package.json and activates that exact version on demand;
#   - mise's npm:pnpm backend is just a global "latest" install with no
#     per-project awareness, which makes pnpm-pinned monorepos behave
#     differently in ahjo than in CI / production Docker.
# COI happens to install pnpm via mise anyway; we revert that choice here.
#
# The cost is ~30s of redundant install→uninstall during `ahjo update`
# only (not at container-create — branches COW-clone the finished image).
# Long-term fix lives upstream: a COI patch making the mise tool list
# opt-out-able would let ahjo skip the install in the first place.
#
# Mechanics (also surprising — worth keeping):
#   - mise canonicalizes `npm:pnpm` to a bare `pnpm = "latest"` entry in
#     ~/.config/mise/config.toml (mise has a native pnpm backend), so
#     `unuse` must match the bare name, not `npm:pnpm`.
#   - `mise unuse` removes the config entry but leaves the binary at
#     ~/.local/share/mise/installs/pnpm/. `mise reshim` then regenerates
#     a shim for *every installed tool* regardless of config, so without
#     `uninstall` the shim rematerializes the next time anything triggers
#     a reshim. `uninstall` removes the install dir; reshim has nothing
#     to make a shim from.
su - code -c 'mise unuse --global pnpm || true'
su - code -c 'mise uninstall pnpm || true'

# corepack ships with Node and creates pnpm/yarn/npx shims in the system
# Node bin (NodeSource installs to /usr/bin). `mise activate` prepends mise's
# shim dir to PATH, but with mise's pnpm shim removed, lookup falls through
# to /usr/bin/pnpm (corepack).
corepack enable

# Suppress corepack's "About to download pnpm@x.y.z, continue?" prompt so
# non-interactive contexts don't hang on first use of a new pnpm version.
# /etc/environment covers PAM logins; profile.d covers `runuser -l` etc.
grep -q '^COREPACK_ENABLE_DOWNLOAD_PROMPT=' /etc/environment \
    || echo 'COREPACK_ENABLE_DOWNLOAD_PROMPT=0' >> /etc/environment
cat > /etc/profile.d/corepack.sh <<'EOF'
export COREPACK_ENABLE_DOWNLOAD_PROMPT=0
EOF
chmod 644 /etc/profile.d/corepack.sh
