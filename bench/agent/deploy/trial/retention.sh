#!/bin/sh
# Deletes trial runs nobody has touched for a while. bench-retention.timer
# runs it daily; by hand, as root:
#   sh retention.sh [--dry-run] [--days N] [--jobs DIR]
#
# A run is a folder <jobs>/<id> that Harbor made (it holds config.json,
# lock.json or result.json, as harbor/job.go's isJobDir checks) plus its
# bookkeeping in <jobs>/.bench/: <id>.json, .log, .pid, .owner, .paused,
# sync/<id>.json and retried/<id>/. It is deleted when nothing in any of
# them has changed for --days days (default 30, or RETENTION_DAYS), unless
#   - its id is not a plain name ([A-Za-z0-9._-], not starting with a dot)
#     or is one of the console's own files in .bench/ (skipped, logged);
#   - it is a showcase run: listed in trial.json's "showcase" (TRIAL_CONFIG,
#     default /etc/kavach/trial.json) or in TRIAL_SHOWCASE; both lists count.
#     TRIAL_SHOWCASE comes from the environment, else from the file
#     TRIAL_SHOWCASE_FILE (what bench-retention.service extracts from
#     trial.env), else from trial.env itself (TRIAL_ENV);
#   - it is still running, as the console decides (harbor/run.go, Runner.pid
#     and Runner.orphans): a process whose command line names the run's
#     config <jobs>/.bench/<id>.json (harbor run, or the log helper in front
#     of it) or ends with <jobs>/<id> (harbor jobs resume -p). If the process
#     list cannot be read, every run counts as running.
# It also removes <jobs>/.bench/home/<id>, the private HOME of a run with a
# visitor's keys, once it is older than a day and its run is not running
# (the console's SweepHomes does the same at start).
#
# Anything it cannot read or delete is logged and kept; the pass goes on and
# exits 1 at the end. Exit 2: bad arguments; 3: the jobs folder or the
# showcase list cannot be used (nothing is deleted).
set -eu

JOBS=${KAVACH_JOBS:-/var/lib/kavach/jobs}
DAYS=${RETENTION_DAYS:-30}
CONFIG=${TRIAL_CONFIG:-/etc/kavach/trial.json}
ENVFILE=${TRIAL_ENV:-/etc/kavach/trial.env}
DRY=

log() { echo "retention: $*"; }
die() { echo "retention: $1" >&2; exit "${2:-3}"; }
usage() { die "usage: retention.sh [--dry-run] [--days N] [--jobs DIR]" 2; }

while [ $# -gt 0 ]; do
	case $1 in
	--dry-run | -n) DRY=1 ;;
	--days) [ $# -ge 2 ] || usage; DAYS=$2; shift ;;
	--days=*) DAYS=${1#--days=} ;;
	--jobs) [ $# -ge 2 ] || usage; JOBS=$2; shift ;;
	--jobs=*) JOBS=${1#--jobs=} ;;
	*) usage ;;
	esac
	shift
done
case $DAYS in '' | *[!0-9]*) die "--days must be a whole number" 2 ;; esac
[ "$DAYS" -ge 1 ] || die "--days must be at least 1" 2
case $JOBS in /*) ;; *) die "--jobs must be an absolute path" 2 ;; esac
while [ "${JOBS%/}" != "$JOBS" ]; do JOBS=${JOBS%/}; done
BENCH=$JOBS/.bench
# The console's jobs folder always has .bench/; this refuses /, /var/lib/kavach
# and other folders that only look like one.
[ -n "$JOBS" ] && [ -d "$JOBS" ] && [ -d "$BENCH" ] && [ ! -L "$BENCH" ] ||
	die "${JOBS:-/} is not a jobs folder (no .bench/ in it)"
cd "$JOBS"

# Showcase ids, one per line. A list that is there but cannot be read stops
# the pass: deleting a showcase run by mistake is worse than keeping old ones.
showcase=
if [ -e "$CONFIG" ]; then
	showcase=$(python3 -I -c '
import json, sys
try:
    v = json.load(open(sys.argv[1])).get("showcase") or []
except Exception as e:
    sys.exit("retention: %s: %s" % (sys.argv[1], e))
for s in v:
    if isinstance(s, str) and s.strip():
        print(s.strip())
' "$CONFIG") || die "cannot read the showcase list in $CONFIG"
fi
if [ -n "${TRIAL_SHOWCASE+set}" ]; then
	list=$TRIAL_SHOWCASE
elif [ -n "${TRIAL_SHOWCASE_FILE:-}" ]; then
	list=$(cat -- "$TRIAL_SHOWCASE_FILE") || die "cannot read $TRIAL_SHOWCASE_FILE"
elif [ -e "$ENVFILE" ]; then
	list=$(sed -n 's/^[[:space:]]*TRIAL_SHOWCASE=//p' "$ENVFILE") || die "cannot read $ENVFILE"
else
	list=
fi
list=$(printf '%s\n' "$list" | tail -n 1 | tr -d "\"'")
showcase=$(printf '%s\n%s\n' "$showcase" "$(printf '%s' "$list" | tr ',' '\n')" |
	sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | sed '/^$/d')
log "jobs $JOBS, keep $DAYS days, showcase: $(printf '%s' "$showcase" | tr '\n' ' ')${DRY:+ (dry run)}"

is_showcase() { printf '%s\n' "$showcase" | grep -Fqx -- "$1"; }

# A plain run id, and not the name of one of the console's files in .bench/.
valid_id() {
	case $1 in
	'' | .* | *[!A-Za-z0-9._-]*) return 1 ;;
	home | retried | sync | trial-invites | invites-used) return 1 ;;
	esac
	return 0
}
printable() { printf '%s' "$1" | tr -c 'A-Za-z0-9._/-' '?'; }

# Every process's command line, read once. Unreadable: everything is live.
if procs=$(ps -A -ww -o args=); then
	procs_ok=1
else
	procs_ok=
	log "cannot list processes; treating every run as running"
fi
is_live() {
	[ -n "$procs_ok" ] || return 0
	state=$(printf '%s\n' "$procs" | awk -v cfg="$BENCH/$1.json" -v dir="$JOBS/$1" '
		{ sub(/[ \t]+$/, "") }
		index($0, cfg) || (length($0) >= length(dir) && substr($0, length($0) - length(dir) + 1) == dir) { found = 1 }
		END { print found ? "live" : "gone" }') || return 0
	[ "$state" != gone ]
}

failed=0
# recent N PATH... reports whether anything under the paths changed in the
# last N days. A path it cannot search counts as changed.
recent() {
	n=$1
	shift
	for p in "$@"; do
		[ -e "$p" ] || continue
		if ! out=$(find "$p" -mtime "-$n" -print 2>/dev/null); then
			log "cannot search $(printable "$p"); keeping it"
			return 0
		fi
		[ -z "$out" ] || return 0
	done
	return 1
}

remove() {
	[ -e "$1" ] || [ -L "$1" ] || return 0
	if [ -n "$DRY" ]; then
		log "would delete $1"
	elif rm -rf -- "$1"; then
		log "deleted $1"
	else
		log "FAILED to delete $1"
		failed=$((failed + 1))
	fi
}

kept=0
deleted=0
for dir in "$JOBS"/*; do
	[ -d "$dir" ] && [ ! -L "$dir" ] || continue
	id=${dir##*/}
	if ! valid_id "$id"; then
		log "skip $(printable "$id") (not a run id)"
		continue
	fi
	[ -e "$dir/config.json" ] || [ -e "$dir/lock.json" ] || [ -e "$dir/result.json" ] || continue
	if is_showcase "$id"; then
		log "keep $id (showcase)"
	elif is_live "$id"; then
		log "keep $id (running)"
	elif recent "$DAYS" "$dir" "$BENCH/$id.json" "$BENCH/$id.log" "$BENCH/$id.pid" \
		"$BENCH/$id.owner" "$BENCH/$id.paused" "$BENCH/sync/$id.json" "$BENCH/retried/$id"; then
		:
	else
		before=$failed
		remove "$dir"
		# A folder left behind keeps its bookkeeping, for the next pass.
		[ "$failed" -eq "$before" ] || { kept=$((kept + 1)); continue; }
		for f in "$BENCH/$id.json" "$BENCH/$id.log" "$BENCH/$id.pid" "$BENCH/$id.owner" \
			"$BENCH/$id.paused" "$BENCH/sync/$id.json" "$BENCH/retried/$id"; do
			remove "$f"
		done
		[ "$failed" -gt "$before" ] || { deleted=$((deleted + 1)); continue; }
	fi
	kept=$((kept + 1))
done

homes=0
for home in "$BENCH"/home/*; do
	[ -d "$home" ] && [ ! -L "$home" ] || continue
	id=${home##*/}
	if ! valid_id "$id"; then
		log "skip home/$(printable "$id") (not a run id)"
	elif is_live "$id"; then
		log "keep home/$id (running)"
	elif ! recent 1 "$home"; then
		before=$failed
		remove "$home"
		[ "$failed" -gt "$before" ] || homes=$((homes + 1))
	fi
done

log "${DRY:+dry run: }$deleted runs deleted, $kept kept, $homes home folders removed, $failed failures"
[ "$failed" -eq 0 ] || exit 1
