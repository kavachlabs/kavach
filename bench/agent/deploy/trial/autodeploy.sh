#!/bin/sh
# Deploys the newest main when it differs from the VM's checkout and no trial
# run is live: `kavach-trial deploy main`, done on the VM itself. Run as root
# by bench-autodeploy.timer each night; install it with install-autodeploy.sh.
# It builds bench here, as the kavach user, with the Go in /usr/local/go.
#
#   sh autodeploy.sh [ref]   (default main)
set -eu

REPO=/opt/kavach
BENCH=$REPO/bin/bench
API=http://127.0.0.1:8480
GO=/usr/local/go/bin/go
UNIT=$REPO/bench/agent/deploy/trial/bench-trial.service

log() { echo "autodeploy: $*"; }

# live_runs is kavach-trial's: running jobs according to the API, and log
# helpers (each run has one) in case the API is down.
live_runs() {
	api=$(curl -fsS $API/api/jobs 2>/dev/null | grep -o '"status": *"running"' | wc -l || true)
	helpers=$(pgrep -fc '[b]ench _logpipe' || true)
	echo $((api + helpers))
}

# restart installs the checkout's unit and restarts the console.
restart() {
	install -m 0644 "$UNIT" /etc/systemd/system/bench-trial.service &&
		systemctl daemon-reload && systemctl restart bench-trial
}

# rollback puts the previous binary and checkout back, and fails the unit.
rollback() {
	log "$2; rolling back to $1"
	rm -f "$BENCH.new"
	[ ! -f "$BENCH.prev" ] || mv -f "$BENCH.prev" "$BENCH"
	if git -C $REPO checkout -q --detach "$1" && restart; then
		log "rolled back: the previous binary and checkout $1 are in place"
		exit 1
	fi
	log "ROLLBACK FAILED: fix the VM by hand (binary $BENCH vs $BENCH.prev, checkout $1, systemctl restart bench-trial)"
	exit 2
}

# main is a function so sh reads all of it before running any: the checkout
# this file lives in changes under it.
main() {
	ref=${1:-main}
	git -C $REPO fetch -q origin "$ref"
	sha=$(git -C $REPO rev-parse FETCH_HEAD)
	prev=$(git -C $REPO rev-parse HEAD)
	if [ "$sha" = "$prev" ]; then
		log "up to date at $sha"
		return
	fi
	live=$(live_runs)
	if [ "$live" != 0 ]; then
		log "$live trial run(s) are live; $sha waits for the next night"
		return
	fi

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	log "building bench at $sha (checkout is $prev)"
	# The whole tree: the console's go.mod replaces the root module with ../../..
	git -C $REPO archive "$sha" | tar -x -C "$tmp"
	chown -R kavach:kavach "$tmp"
	runuser -u kavach -- env HOME=/home/kavach GOTOOLCHAIN=local CGO_ENABLED=0 \
		sh -c "cd '$tmp/bench/agent/console' && nice $GO build -o '$tmp/bench' ./cmd/bench" || {
		log "the build failed; nothing was changed"
		exit 1
	}

	install -m 0755 "$tmp/bench" "$BENCH.new"
	# The new binary must accept the config and usage files as they are, read
	# as the service's user, which can't read what root can.
	runuser -u kavach -- "$BENCH.new" api -trial -check -root $REPO -jobs /var/lib/kavach/jobs || {
		rm -f "$BENCH.new"
		log "the new bench rejects the VM's trial.json or invite usage; nothing was changed"
		exit 1
	}
	# A visitor may have started a run during the build.
	live=$(live_runs)
	if [ "$live" != 0 ]; then
		rm -f "$BENCH.new"
		log "$live trial run(s) started during the build; $sha waits for the next night"
		return
	fi

	git -C $REPO checkout -q --detach "$sha" || {
		rm -f "$BENCH.new"
		log "could not check out $sha; nothing was changed"
		exit 1
	}
	# Rename, never write: each run's log helper is this executable.
	cp -p "$BENCH" "$BENCH.prev" && mv -f "$BENCH.new" "$BENCH" && restart ||
		rollback "$prev" "installing or restarting failed"

	# The service takes a moment to listen.
	i=0
	until code=$(curl -s -o /dev/null -w '%{http_code}' $API/api/trial/session) && [ "$code" = 200 ]; do
		i=$((i + 1))
		[ $i -lt 15 ] || rollback "$prev" "bench-trial answered ${code:-nothing} after the restart"
		sleep 1
	done
	log "deployed $sha (was $prev)"
}

main "$@"
