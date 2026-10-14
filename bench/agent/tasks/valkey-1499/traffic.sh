#!/usr/bin/env bash
# Drives a recording valkey-server with the traffic of the reported incident.
# An application caches sessions. A cache keeper, connected in no-touch mode so
# that its own reads do not keep keys alive, inspects how long keys have been
# idle, and keeps the ones still in use alive with a script that TOUCHes them.
# Usage: traffic.sh <valkey-cli> <port>
set -euo pipefail
cli=("$1" -p "$2")
r() { "${cli[@]}" "$@"; }

# The application.
for i in 1 2 3 4 5 6; do
  r SET "session:$i" "user-$i" >/dev/null
done
r GET session:1 >/dev/null
r INCR stats:logins >/dev/null
r GET session:5 >/dev/null
r INCR stats:logins >/dev/null
sleep 3
r GET session:6 >/dev/null

# The cache keeper, on one connection: each line is one command.
peek="return redis.call('GET', KEYS[1])"
keep_alive="for _, k in ipairs(KEYS) do redis.call('TOUCH', k) end return #KEYS"
{
  echo "CLIENT NO-TOUCH ON"
  echo "OBJECT IDLETIME session:1"
  echo "OBJECT IDLETIME session:2"
  echo "EVAL \"$peek\" 1 session:2"
  echo "OBJECT IDLETIME session:2"
  echo "TOUCH session:1"
  echo "OBJECT IDLETIME session:1"
  echo "EVAL \"$keep_alive\" 2 session:3 session:4"
  echo "OBJECT IDLETIME session:3"
  echo "OBJECT IDLETIME session:4"
} | r
