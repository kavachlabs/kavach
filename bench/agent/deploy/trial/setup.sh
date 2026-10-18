#!/bin/sh
# Prepares a fresh Ubuntu VM (24.04 or later) for the trial console. Run as
# root:
#   sh setup.sh <git ref>
# then put /opt/kavach/bin/bench in place (see README.md), fill
# /etc/kavach/trial.env and start the services. It is safe to run again.
set -eu
REF=${1:-main}
HARBOR_VERSION=0.24.0

apt-get update
apt-get install -y git curl ca-certificates python3
id kavach >/dev/null 2>&1 || useradd -m -s /bin/sh kavach

# The repository: tasks, and the live agent Harbor imports from it.
if [ ! -d /opt/kavach/.git ]; then
	git clone https://github.com/kavachlabs/kavach.git /opt/kavach
fi
git -C /opt/kavach fetch origin "$REF"
git -C /opt/kavach checkout --detach FETCH_HEAD
mkdir -p /opt/kavach/bin /var/lib/kavach/jobs /etc/kavach
chown -R kavach:kavach /var/lib/kavach

# Harbor, with the remote sandbox providers, for the kavach user.
su - kavach -c 'curl -LsSf https://astral.sh/uv/install.sh | sh'
su - kavach -c "\$HOME/.local/bin/uv tool install --force --with 'harbor[daytona]==$HARBOR_VERSION' --with 'harbor[modal]==$HARBOR_VERSION' harbor==$HARBOR_VERSION"

# cloudflared, from Cloudflare's apt repository.
install -d -m 0755 /usr/share/keyrings
curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg
echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main' >/etc/apt/sources.list.d/cloudflared.list
apt-get update && apt-get install -y cloudflared

[ -f /etc/kavach/trial.env ] || install -m 0600 /opt/kavach/bench/agent/deploy/trial/trial.env.example /etc/kavach/trial.env
KIT=/opt/kavach/bench/agent/deploy/trial
for unit in bench-trial.service bench-retention.service bench-retention.timer; do
	install -m 0644 "$KIT/$unit" "/etc/systemd/system/$unit"
done
systemctl daemon-reload

# Security updates, fail2ban, key-only SSH, a capped journal.
sh "$KIT/harden.sh"

echo "next: copy bench to /opt/kavach/bin/bench, fill /etc/kavach/trial.env, set up the tunnel (README.md)"
echo "then: sh $KIT/retention.sh --dry-run, and systemctl enable --now bench-retention.timer (OPERATIONS.md)"
