#!/usr/bin/env bash
# Drives a recording redis-server with the traffic of the reported incident:
# a wallet service credits accounts with a Lua script that validates the
# amount with tonumber() and adds it with HINCRBY, in micro-units, and keeps a
# display balance in dollars with HINCRBYFLOAT. Readers check balances. A
# top-up of $200 (200000000 micro-units) fails.
#
# The dollar amounts are exact binary fractions, so HINCRBYFLOAT, which adds
# in long double, gives the same digits on every platform (it does not on
# 1.25 + 9.99: macOS arm64 and Linux print different digits).
# Usage: traffic.sh <redis-cli> <port>
set -euo pipefail
cli=("$1" -p "$2")
r() { "${cli[@]}" "$@"; }

CREDIT='
local amount = tonumber(ARGV[1])
if not amount or amount <= 0 then
  return redis.error_reply("ERR invalid amount")
end
local micros = redis.call("HINCRBY", KEYS[1], "balance_micros", amount)
redis.call("HINCRBYFLOAT", KEYS[1], "balance_usd", amount / 1000000)
return micros'

credit() { r EVAL "$CREDIT" 1 "wallet:$1" "$2"; }

r HSET wallet:alice owner alice currency USD >/dev/null
r HSET wallet:bob owner bob currency USD >/dev/null
credit alice 1250000          # $1.25
credit bob 37500000           # $37.50
credit alice 9750000          # $9.75
r HGETALL wallet:alice
credit bob 120125000          # $120.125
credit alice abc || true      # rejected by the script
r HGET wallet:bob balance_micros
credit alice 50000000         # $50
r HGETALL wallet:bob
# The incident: a $200 top-up.
credit bob 200000000 || true
r HGETALL wallet:bob
