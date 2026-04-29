#!/bin/bash
set -euo pipefail

apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssh-server

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
