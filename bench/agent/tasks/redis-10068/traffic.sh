#!/usr/bin/env bash
# Drives a recording redis-server with the traffic of the reported incident:
# an entry added with an explicit, old ID, then entries with auto-generated
# IDs, readers, and a retention job that trims by MINID.
# Usage: traffic.sh <redis-cli> <port>
set -euo pipefail
cli=("$1" -p "$2")
r() { "${cli[@]}" "$@"; }

r XADD orders 10-1 note backfilled >/dev/null
ids=()
for i in $(seq 1 12); do
  ids+=("$(r XADD orders '*' order "$i" status new)")
  if (( i % 4 == 0 )); then
    r XRANGE orders - + COUNT 5 >/dev/null
    r XLEN orders >/dev/null
  fi
  sleep 0.02
done
# Retention: keep the newest entries, from the 6th auto-generated one on.
r XTRIM orders MINID "${ids[5]}"
r XLEN orders
