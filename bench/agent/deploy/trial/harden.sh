#!/bin/sh
# Hardens the trial VM: security updates without reboots, fail2ban for SSH,
# key-only SSH, a cap on the journal. setup.sh runs it; on a VM set up before
# it existed, run it by hand as root:
#   sh /opt/kavach/bench/agent/deploy/trial/harden.sh
# It is safe to run again. Keep your current SSH session open until a new one
# has logged in.
set -eu

step() { printf '\n==> %s\n' "$*"; }

[ "$(id -u)" -eq 0 ] || { echo "harden.sh: run as root" >&2; exit 1; }
export DEBIAN_FRONTEND=noninteractive
# needrestart only lists services to restart, so an install never restarts
# bench-trial or cloudflared (runs and sessions would go with them).
export NEEDRESTART_MODE=l

step "Installing unattended-upgrades and fail2ban"
apt-get update -q
apt-get install -y -q unattended-upgrades fail2ban python3-systemd

step "Unattended upgrades: security origin only, never reboot"
# 52 sorts after Ubuntu's 50unattended-upgrades; #clear drops its list
# (APT appends lists across files) before ours replaces it.
cat >/etc/apt/apt.conf.d/52kavach-unattended-upgrades <<'EOF'
// Written by bench/agent/deploy/trial/harden.sh.
#clear Unattended-Upgrade::Allowed-Origins;
#clear Unattended-Upgrade::Origins-Pattern;
Unattended-Upgrade::Allowed-Origins {
	"${distro_id}:${distro_codename}-security";
};
Unattended-Upgrade::Automatic-Reboot "false";
EOF
cat >/etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
systemctl enable --now unattended-upgrades.service >/dev/null 2>&1 || true
apt-config dump | grep -E '^Unattended-Upgrade::(Allowed-Origins|Automatic-Reboot)' || true

step "fail2ban: ban an address for 1 h after 5 failed SSH logins in 10 min"
# The sshd filter reads the journal (sshd-session on OpenSSH 9.8 and later).
cat >/etc/fail2ban/jail.d/kavach-sshd.local <<'EOF'
# Written by bench/agent/deploy/trial/harden.sh.
[sshd]
enabled = true
backend = systemd
ignoreip = 127.0.0.1/8 ::1
maxretry = 5
findtime = 10m
bantime = 1h
EOF
systemctl enable fail2ban >/dev/null 2>&1
systemctl restart fail2ban
sleep 2
fail2ban-client status sshd || echo "warning: fail2ban has no sshd jail; see journalctl -u fail2ban" >&2

step "sshd: keys only, root by key only"
# sshd keeps the first value it reads, and sshd_config includes
# sshd_config.d/*.conf in order before its own lines, so 00- wins over a
# cloud image's 50-cloud-init.conf.
dropin=/etc/ssh/sshd_config.d/00-kavach-hardening.conf
tmp=$dropin.new
cat >"$tmp" <<'EOF'
# Written by bench/agent/deploy/trial/harden.sh.
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
EOF
mkdir -p /run/sshd # sshd -t needs it; socket activation may not have made it yet
old=
[ -f "$dropin" ] && old=$(cat "$dropin")
mv "$tmp" "$dropin"
if ! /usr/sbin/sshd -t; then
	echo "sshd -t rejected the configuration; restoring the previous drop-in" >&2
	if [ -n "$old" ]; then printf '%s\n' "$old" >"$dropin"; else rm -f "$dropin"; fi
	exit 1
fi
echo "sshd -t: configuration OK"
/usr/sbin/sshd -T 2>/dev/null | grep -E '^(passwordauthentication|kbdinteractiveauthentication|permitrootlogin) ' || true
# Ubuntu's unit is ssh.service; when only ssh.socket is active, the next
# connection starts sshd with the new configuration anyway.
if systemctl is-active --quiet ssh.service; then
	systemctl reload ssh.service
	echo "reloaded ssh.service"
elif systemctl is-active --quiet sshd.service; then
	systemctl reload sshd.service
	echo "reloaded sshd.service"
else
	echo "sshd is socket-activated; new connections use the new configuration"
fi

step "journald: at most 200M on disk"
mkdir -p /etc/systemd/journald.conf.d
cat >/etc/systemd/journald.conf.d/kavach.conf <<'EOF'
# Written by bench/agent/deploy/trial/harden.sh.
[Journal]
SystemMaxUse=200M
EOF
systemctl restart systemd-journald
journalctl --disk-usage

cat <<'EOF'

==> Done. Before closing this session, open a NEW terminal and check that
    `ssh root@<vm>` still logs in with your key. If it does not, fix it
    from this session (rm /etc/ssh/sshd_config.d/00-kavach-hardening.conf,
    then systemctl reload ssh). Locked out with no session left: Hetzner
    Cloud Console -> the server -> Console (>_) is a terminal that needs
    no SSH. It logs in with root's password; a server made with an SSH key
    has none, so set one first under Rescue -> Reset root password. There,
    fail2ban-client set sshd unbanip <ip> lifts a ban.
EOF
