#!/usr/bin/env bash
# Drives a recording redis-server with the traffic of the incident: daily
# activity bitmaps (one bit per user ID), a dashboard that counts them and
# finds the first active user, and a new release of the reporting job that
# sends a unit Redis does not accept ("BITS" for BIT). On yesterday's bitmap
# the job gets an error; on today's, which nobody has written yet, BITPOS
# answers as if the arguments were fine.
# Usage: traffic.sh <redis-cli> <port>
set -euo pipefail
cli=("$1" -p "$2")
r() { "${cli[@]}" "$@"; }

y=active:2023-08-20 t=active:2023-08-21
for uid in 3 17 18 40 41 42 97 130; do
  r SETBIT "$y" "$uid" 1 >/dev/null
done
r SET signups:2023-08-20 8 >/dev/null

# Dashboard, release 1.
for k in "$y" "$t"; do
  r BITCOUNT "$k"
  r BITCOUNT "$k" 0 -1 BIT
  r BITPOS "$k" 1
  r BITPOS "$k" 1 0 -1 BIT
done
r BITCOUNT "$y" 2 5
r BITPOS "$y" 1 1 -1 BYTE
r GETBIT "$y" 42

# Reporting job, release 2: the unit is misspelled.
r BITCOUNT "$y" 0 -1 BITS || true
r BITPOS "$y" 1 0 -1 BITS || true
r BITCOUNT "$t" 0 -1 BITS || true
r BITPOS "$t" 1 0 -1 BITS || true
