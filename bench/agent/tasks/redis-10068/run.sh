#!/usr/bin/env bash
# Reproduces the redis-10068 pilot end to end:
#   1. builds Kavach (CLI, recorder, C SDK) from this checkout;
#   2. clones Redis at the task's base commit, applies the Kavach integration
#      and builds it (the "old" build);
#   3. records the incident traffic against a live server, giving a fixture;
#   4. replays that fixture and the committed incident.kavach on the old build;
#   5. for each candidate fix: builds it, runs `kavach diff`, and runs the
#      hidden upstream test (SWE-bench's FAIL_TO_PASS and PASS_TO_PASS).
#
# Usage: run.sh [work-dir]   (default: a new temporary directory)
# Needs: git, make, a C compiler, cmake, go, tclsh (8.5+), python3.
set -euo pipefail

TASK=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$TASK/../../../.." && pwd)
WORK=${1:-$(mktemp -d)}
BASE=e88f6acb94c77c9a5b81f0b2a8bd132b2a5c3d3c
PORT=${PORT:-6399}
mkdir -p "$WORK/bin"
echo "work dir: $WORK"
# On macOS use clang, as sdk/c/README.md recommends.
[[ $(uname) == Darwin ]] && export CC=clang CXX=clang++

# 1. Kavach
cmake -S "$ROOT/sdk/c" -B "$WORK/kavach-c" -DCMAKE_BUILD_TYPE=Release >/dev/null
cmake --build "$WORK/kavach-c" --target kavach_static -j8 >/dev/null
(cd "$ROOT" && go build -o "$WORK/bin/kavach" ./cmd/kavach && go build -o "$WORK/bin/kavach-recorder" ./cmd/kavach-recorder)
KAVACH="$WORK/bin/kavach --no-banner"

build() {
  local flags=()
  if [[ $(uname) == Darwin ]]; then
    # Redis of early 2022 predates the current macOS SDK: stat64 is gone, and
    # the crash reporter needs AvailabilityMacros.h to recognise arm64.
    flags=("REDIS_CFLAGS=-Dstat64=stat -Dfstat64=fstat -include AvailabilityMacros.h")
  fi
  make -C "$1" -j8 CC="${CC:-cc}" ${flags[@]+"${flags[@]}"} KAVACH_SDK="$ROOT/sdk/c" KAVACH_LIB="$WORK/kavach-c/libkavach.a" \
    >"$1/build.log" 2>&1 || { tail -20 "$1/build.log"; exit 1; }
}

# 2. Redis, old build
if [[ ! -d "$WORK/redis" ]]; then
  git clone -q --filter=blob:none https://github.com/redis/redis.git "$WORK/redis"
fi
git -C "$WORK/redis" checkout -q -f "$BASE"
git -C "$WORK/redis" clean -qfd src
git -C "$WORK/redis" apply "$TASK/redis-kavach.patch"
build "$WORK/redis"
OLD="$WORK/redis/src/redis-server"

# 3. Record the incident
rm -rf "$WORK/rec" && mkdir -p "$WORK/rec"
# The recorder captures the server's environment (SPEC 4.8), so start it with
# a minimal one and relative paths: fixtures are shared, and must not carry
# this machine's variables or directory names.
cp "$WORK/bin/kavach-recorder" "$WORK/rec/"
(cd "$WORK/rec" && env -i PATH=/usr/bin:/bin HOME=/var/empty LANG=C TZ=UTC \
  KAVACH_REDIS_DIR=journal KAVACH_RECORDER=./kavach-recorder \
  "$OLD" --port "$PORT" --save '' --appendonly no >server.log 2>&1 &)
for _ in $(seq 50); do "$WORK/redis/src/redis-cli" -p "$PORT" ping >/dev/null 2>&1 && break; sleep 0.1; done
"$TASK/traffic.sh" "$WORK/redis/src/redis-cli" "$PORT" >/dev/null
"$WORK/redis/src/redis-cli" -p "$PORT" shutdown nosave >/dev/null 2>&1 || true
for _ in $(seq 50); do ls "$WORK"/rec/journal/fixtures/*.kavach >/dev/null 2>&1 && break; sleep 0.1; done
FRESH=$(ls "$WORK"/rec/journal/fixtures/*.kavach | head -1)

# 4. Replay on the old build. kavach exits non-zero when the verdict is a
# failure, which is the expected verdict here; the verdict is read from its output.
for f in "$FRESH" "$TASK/incident.kavach"; do
  echo "== replay $(basename "$f")"
  out=$($KAVACH replay "$f" --bin "$OLD") || true
  grep -E '^(service|recorded|result)' <<<"$out"
done

# 5. Candidates
hidden() {
  git -C "$1" apply "$TASK/hidden/test.patch"
  local out
  out=$(cd "$1" && TERM=dumb ./runtest --single unit/type/stream --only "/*XTRIM*" 2>&1 || true)
  git -C "$1" checkout -q -- tests/unit/type/stream.tcl
  if grep -q 'All tests passed' <<<"$out"; then echo pass; else echo fail; fi
}

printf '\n%-24s %-48s %s\n' candidate "kavach diff (fresh fixture)" "hidden test"
printf '%-24s %-48s %s\n' "(old build)" "-" "$(hidden "$WORK/redis")"
for c in "$TASK"/candidates/*.patch; do
  name=$(basename "$c" .patch)
  dir="$WORK/redis-$name"
  rm -rf "$dir"
  git -C "$WORK/redis" worktree prune
  git -C "$WORK/redis" worktree add -q --detach "$dir" "$BASE"
  git -C "$dir" apply "$TASK/redis-kavach.patch" "$c"
  build "$dir"
  out=$($KAVACH diff "$FRESH" --old "$OLD" --new "$dir/src/redis-server") || true
  verdict=$(awk '/^verdict/{print $2}' <<<"$out")
  variants=$(sed -n 's/^variants *//p' <<<"$out")
  printf '%-24s %-48s %s\n' "$name" "$verdict${variants:+ ($variants)}" "$(hidden "$dir")"
done
