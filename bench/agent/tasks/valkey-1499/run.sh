#!/usr/bin/env bash
# Reproduces the valkey-1499 task end to end:
#   1. builds Kavach (CLI, recorder, C SDK) from this checkout;
#   2. clones Valkey at the task's base commit, applies the Kavach integration
#      and builds it (the "old" build);
#   3. records the incident traffic against a live server, giving a fixture;
#   4. replays that fixture and the committed incident.kavach on the old build;
#   5. for each candidate fix: builds it, runs `kavach diff`, and runs the
#      hidden upstream test (SWE-bench's FAIL_TO_PASS and PASS_TO_PASS).
#
# Usage: run.sh [work-dir]   (default: a new temporary directory)
# The test servers listen on Unix sockets under the work dir, and macOS limits
# their paths to 104 bytes: keep the work dir's path short there.
# Needs: git, make, a C compiler, cmake, go, tclsh (8.5+), lsof.
set -euo pipefail

TASK=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$TASK/../../../.." && pwd)
WORK=${1:-$(mktemp -d)}
BASE=93b701d8d48eaf87077e707dd77eaaed0e66900f
PORT=${PORT:-27499}           # the recording server
TEST_PORT=${TEST_PORT:-28111} # the first port the hidden test's servers use
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
  make -C "$1" -j8 CC="${CC:-cc}" KAVACH_SDK="$ROOT/sdk/c" KAVACH_LIB="$WORK/kavach-c/libkavach.a" \
    >"$1/build.log" 2>&1 || { tail -20 "$1/build.log"; exit 1; }
}

# 2. Valkey, old build
if [[ ! -d "$WORK/valkey" ]]; then
  git clone -q --filter=blob:none https://github.com/valkey-io/valkey.git "$WORK/valkey"
fi
git -C "$WORK/valkey" checkout -q -f "$BASE"
git -C "$WORK/valkey" clean -qfd src
git -C "$WORK/valkey" apply "$TASK/valkey-kavach.patch"
build "$WORK/valkey"
OLD="$WORK/valkey/src/valkey-server"
CLI="$WORK/valkey/src/valkey-cli"

# 3. Record the incident
if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "port $PORT is in use; set PORT" >&2
  exit 1
fi
rm -rf "$WORK/rec" && mkdir -p "$WORK/rec"
# The recorder captures the server's environment (SPEC 4.8), so start it with
# a minimal one and relative paths: fixtures are shared, and must not carry
# this machine's variables or directory names.
cp "$WORK/bin/kavach-recorder" "$WORK/rec/"
(cd "$WORK/rec" && env -i PATH=/usr/bin:/bin HOME=/var/empty LANG=C TZ=UTC \
  KAVACH_VALKEY_DIR=journal KAVACH_RECORDER=./kavach-recorder \
  "$OLD" --port "$PORT" --bind 127.0.0.1 --save '' --appendonly no >server.log 2>&1 &)
for _ in $(seq 50); do "$CLI" -p "$PORT" ping >/dev/null 2>&1 && break; sleep 0.1; done
"$TASK/traffic.sh" "$CLI" "$PORT" >/dev/null
"$CLI" -p "$PORT" shutdown nosave >/dev/null 2>&1 || true
for _ in $(seq 50); do ls "$WORK"/rec/journal/fixtures/*.kavach >/dev/null 2>&1 && break; sleep 0.1; done
FRESH=$(ls "$WORK"/rec/journal/fixtures/*.kavach | head -1)

# 4. Replay on the old build. kavach exits non-zero when the verdict is a
# failure, which is the expected verdict here; the verdict is read from its output.
for f in "$FRESH" "$TASK/incident.kavach"; do
  [[ -f $f ]] || continue
  echo "== replay $(basename "$f")"
  out=$($KAVACH replay "$f" --bin "$OLD") || true
  grep -E '^(service|recorded|result)' <<<"$out"
done

# 5. Candidates. The hidden test is the whole of unit/introspection-2, as
# SWE-bench runs it.
hidden() {
  git -C "$1" apply "$TASK/hidden/test.patch"
  local out
  out=$(cd "$1" && TERM=dumb ./runtest --single unit/introspection-2 --baseport "$TEST_PORT" 2>&1 || true)
  git -C "$1" checkout -q -- tests/unit/introspection-2.tcl
  if grep -q 'All tests passed' <<<"$out"; then echo pass; else echo fail; fi
}

printf '\n%-24s %-48s %s\n' candidate "kavach diff (fresh fixture)" "hidden test"
printf '%-24s %-48s %s\n' "(old build)" "-" "$(hidden "$WORK/valkey")"
for c in "$TASK"/candidates/*.patch; do
  name=$(basename "$c" .patch)
  dir="$WORK/valkey-$name"
  rm -rf "$dir"
  git -C "$WORK/valkey" worktree prune
  git -C "$WORK/valkey" worktree add -q --detach "$dir" "$BASE"
  git -C "$dir" apply "$TASK/valkey-kavach.patch" "$c"
  build "$dir"
  out=$($KAVACH diff "$FRESH" --old "$OLD" --new "$dir/src/valkey-server") || true
  verdict=$(awk '/^verdict/{print $2}' <<<"$out")
  variants=$(sed -n 's/^variants *//p' <<<"$out")
  printf '%-24s %-48s %s\n' "$name" "$verdict${variants:+ ($variants)}" "$(hidden "$dir")"
done
