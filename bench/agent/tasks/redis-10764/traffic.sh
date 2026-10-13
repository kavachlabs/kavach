#!/usr/bin/env bash
# Drives a recording redis-server with the traffic of the reported incident.
# A game backend keeps a job queue, the sorted set `jobs` (score = priority),
# which workers drain with BZMPOP, and one leaderboard per match, named by the
# match number. Workers wait on the queue while it is empty. When match 1
# records its first score, its leaderboard key "1" is created; "1" is also
# the numkeys argument of the waiting workers' BZMPOP.
# Usage: traffic.sh <redis-cli> <port>
set -euo pipefail
cli=("$1" -p "$2")
r() { "${cli[@]}" "$@"; }

# Waits until n clients are blocked in BZMPOP (CLIENT is not recorded).
waiting() {
  for _ in $(seq 100); do
    (( $(r CLIENT LIST | grep -c 'flags=b .*cmd=bzmpop') >= $1 )) && return
    sleep 0.05
  done
  echo "traffic: workers did not block" >&2; exit 1
}
worker() { r BZMPOP 0 1 jobs MIN COUNT 2; }

r ZADD jobs 1 job:1 2 job:2 3 job:3
worker
worker
r ZADD 2 30 player:ana 20 player:ben
r ZINCRBY 2 15 player:ben
r ZREVRANGE 2 0 9 WITHSCORES
worker &                 # the queue is empty: waits
waiting 1
r ZADD jobs 4 job:4      # serves it
wait
r ZADD 3 12 player:cy
r ZREVRANGE 3 0 9 WITHSCORES
worker &
waiting 1
worker &
waiting 2
r ZINCRBY 2 5 player:ana
r ZREVRANGE 2 0 9 WITHSCORES
r ZADD jobs 5 job:5      # serves the first worker
r ZCARD jobs
r ZADD 1 10 player:raj   # match 1's first score: wakes the second worker
wait
r ZREVRANGE 1 0 9 WITHSCORES
r ZCARD 1
