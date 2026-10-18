#!/bin/sh
# Deletes trial runs nobody has touched for a while. bench-retention.timer
# runs it daily; by hand:
#   sh retention.sh [--dry-run] [--days N] [--jobs DIR]
#
# A run is the folder <jobs>/<id> plus its bookkeeping in <jobs>/.bench/
# (<id>.json, .log, .pid, .owner, .paused, retried/<id>/). It is deleted
# when nothing in either has changed for --days days (default 30, or
# RETENTION_DAYS), unless it is
#   - a showcase run: listed in /etc/kavach/trial.json ("showcase") or in
#     TRIAL_SHOWCASE (from the environment, else /etc/kavach/trial.env);
#     both lists count;
#   - still running: as the console decides (harbor/run.go, Runner.pid and
#     Runner.orphans), a process whose command line names the run's config
#     <jobs>/.bench/<id>.json (harbor run, or the log helper in front of it)
#     or ends with <jobs>/<id> (harbor jobs resume -p).
# It also removes <jobs>/.bench/home/<id> folders, the private HOME of a run
# with a visitor's keys, that are older than a day and whose run is not
# running (the console's SweepHomes does the same at start).
set -eu

JOBS=${KAVACH_JOBS:-/var/lib/kavach/jobs}
DAYS=${RETENTION_DAYS:-30}
CONFIG=${TRIAL_CONFIG:-/etc/kavach/trial.json}
ENVFILE=${TRIAL_ENV:-/etc/kavach/trial.env}
DRY=

usage() {
	echo "usage: retention.sh [--dry-run] [--days N] [--jobs DIR]" >&2
	exit 2
}
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
case $DAYS in '' | *[!0-9]*) echo "retention: --days must be a whole number" >&2; exit 2 ;; esac
[ "$DAYS" -ge 1 ] || { echo "retention: --days must be at least 1" >&2; exit 2; }
case $JOBS in /*) ;; *) echo "retention: --jobs must be an absolute path" >&2; exit 2 ;; esac
JOBS=${JOBS%/}
[ -d "$JOBS" ] || { echo "retention: $JOBS is not a directory" >&2; exit 3; }
BENCH=$JOBS/.bench

log() { echo "retention: $*"; }

# Showcase ids, one per line. A config that is there but unreadable stops
# the run: deleting a showcase run by mistake is worse than keeping old ones.
showcase=
if [ -e "$CONFIG" ]; then
	ids=$(python3 -I -c '
import json, sys
try:
    v = json.load(open(sys.argv[1])).get("showcase") or []
except Exception as e:
    sys.exit("retention: %s: %s" % (sys.argv[1], e))
for s in v:
    s = s.get("id") if isinstance(s, dict) else s
    if isinstance(s, str) and s.strip():
        print(s.strip())
' "$CONFIG") || { echo "retention: cannot read the showcase list in $CONFIG" >&2; exit 3; }
	showcase=$ids
fi
list=${TRIAL_SHOWCASE-}
if [ -z "${TRIAL_SHOWCASE+set}" ] && [ -e "$ENVFILE" ]; then
	[ -r "$ENVFILE" ] || { echo "retention: cannot read $ENVFILE" >&2; exit 3; }
	list=$(sed -n 's/^[[:space:]]*TRIAL_SHOWCASE=//p' "$ENVFILE" | tail -n 1 | tr -d "\"'")
fi
showcase=$(printf '%s\n%s\n' "$showcase" "$(printf '%s' "$list" | tr ',' '\n')" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | sed '/^$/d')
log "jobs $JOBS, keep $DAYS days, showcase: $(printf '%s' "$showcase" | tr '\n' ' ')${DRY:+ (dry run)}"

is_showcase() {
	printf '%s\n' "$showcase" | grep -Fqx -- "$1"
}

# Every process's command line, read once.
procs=$(ps -A -ww -o args=)
is_live() {
	printf '%s\n' "$procs" | awk -v cfg="$BENCH/$1.json" -v dir="$JOBS/$1" '
		{ sub(/[ \t]+$/, "") }
		index($0, cfg) || (length($0) >= length(dir) && substr($0, length($0) - length(dir) + 1) == dir) { found = 1 }
		END { exit !found }'
}

# recent PATH... reports whether anything under the paths changed in the
# last N days (find -mtime -N).
recent() {
	n=$1
	shift
	for p in "$@"; do
		[ -e "$p" ] || continue
		[ -n "$(find "$p" -mtime "-$n" -print 2>/dev/null | head -n 1)" ] && return 0
	done
	return 1
}

remove() {
	if [ -n "$DRY" ]; then
		log "would delete $*"
	else
		rm -rf -- "$@"
		log "deleted $*"
	fi
}

# bookkeeping ID: the run's files in .bench/ that exist, one per line.
bookkeeping() {
	for f in "$BENCH/$1.json" "$BENCH/$1.log" "$BENCH/$1.pid" "$BENCH/$1.owner" "$BENCH/$1.paused" "$BENCH/retried/$1"; do
		if [ -e "$f" ]; then printf '%s\n' "$f"; fi
	done
}

kept=0
deleted=0
for dir in "$JOBS"/*; do
	[ -d "$dir" ] && [ ! -L "$dir" ] || continue
	id=${dir##*/}
	if is_showcase "$id"; then
		log "keep $id (showcase)"
	elif is_live "$id"; then
		log "keep $id (running)"
	elif recent "$DAYS" "$dir" "$BENCH/$id.json" "$BENCH/$id.log" "$BENCH/$id.pid" "$BENCH/$id.owner" "$BENCH/$id.paused" "$BENCH/retried/$id"; then
		:
	else
		remove "$dir"
		bookkeeping "$id" | while IFS= read -r f; do remove "$f"; done
		deleted=$((deleted + 1))
		continue
	fi
	kept=$((kept + 1))
done

homes=0
for home in "$BENCH"/home/*; do
	[ -d "$home" ] && [ ! -L "$home" ] || continue
	id=${home##*/}
	if is_live "$id"; then
		log "keep home/$id (running)"
	elif recent 1 "$home"; then
		:
	else
		remove "$home"
		homes=$((homes + 1))
	fi
done

log "${DRY:+dry run: }$deleted runs deleted, $kept kept, $homes home folders removed"
