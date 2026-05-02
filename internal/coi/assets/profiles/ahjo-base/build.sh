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

# ahjo-claude-prepare: silences claude's two first-run prompts ("trust this
# directory?" and the --dangerously-skip-permissions warning) inside the
# container. Invoked once by `ahjo shell` immediately after COI's container
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
tmp=$(mktemp)
jq '. + {skipDangerousModePermissionPrompt: true}' \
    "$HOME/.claude/settings.json" > "$tmp" && mv "$tmp" "$HOME/.claude/settings.json"
tmp=$(mktemp)
jq '.projects["/workspace"] = ((.projects["/workspace"] // {}) + {hasTrustDialogAccepted: true})' \
    "$HOME/.claude.json" > "$tmp" && mv "$tmp" "$HOME/.claude.json"
touch "$marker"
PREPARE
chmod 0755 /usr/local/bin/ahjo-claude-prepare
