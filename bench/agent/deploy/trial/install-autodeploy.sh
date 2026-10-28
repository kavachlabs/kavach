#!/bin/sh
# Sets the VM up to deploy main each night (autodeploy.sh): installs Go for
# the build, checked against its published SHA-256, and the timer. Run as
# root from the checkout; it is safe to run again:
#   sh /opt/kavach/bench/agent/deploy/trial/install-autodeploy.sh
set -eu
GO_VERSION=go1.27.3
GO_SHA256=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
KIT=/opt/kavach/bench/agent/deploy/trial

if [ "$(/usr/local/go/bin/go env GOVERSION 2>/dev/null || true)" != "$GO_VERSION" ]; then
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	curl -fsSL "https://go.dev/dl/$GO_VERSION.linux-amd64.tar.gz" -o "$tmp/go.tgz"
	echo "$GO_SHA256  $tmp/go.tgz" | sha256sum -c -
	rm -rf /usr/local/go
	tar -C /usr/local -xzf "$tmp/go.tgz"
fi
/usr/local/go/bin/go version

for unit in bench-autodeploy.service bench-autodeploy.timer; do
	install -m 0644 "$KIT/$unit" "/etc/systemd/system/$unit"
done
systemctl daemon-reload
systemctl enable --now bench-autodeploy.timer
systemctl list-timers bench-autodeploy.timer
