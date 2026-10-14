#!/usr/bin/env bash
# Reproduces the axum-1934 task end to end:
#   1. builds Kavach (CLI and recorder) from this checkout;
#   2. clones axum at the task's base commit, applies the Kavach integration
#      (the kavach-driver example) and builds it (the "old" build);
#   3. records the incident traffic through the driver, giving a fixture;
#   4. replays that fixture and the committed incident.kavach on the old build;
#   5. for each candidate fix: builds it, runs `kavach diff`, and runs the
#      hidden upstream test (SWE-bench's FAIL_TO_PASS and PASS_TO_PASS).
#
# Usage: run.sh [work-dir]   (default: a new temporary directory)
# Needs: git, go, cargo (Rust 1.92 to match the image; the panic message is
# the compiler's rendering of the assertion), python3, network for crates.io.
set -euo pipefail

TASK=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$TASK/../../../.." && pwd)
WORK=${1:-$(mktemp -d)}
BASE=e97462d45251c21ffd1ba4fb5db30f3cf6f6cb6c
mkdir -p "$WORK/bin"
WORK=$(cd "$WORK" && pwd)
echo "work dir: $WORK"

# 1. Kavach
(cd "$ROOT" && go build -o "$WORK/bin/kavach" ./cmd/kavach && go build -o "$WORK/bin/kavach-recorder" ./cmd/kavach-recorder)
KAVACH="$WORK/bin/kavach --no-banner"

# 2. axum, old build
A=$WORK/axum
if [[ ! -d $A ]]; then
  git clone -q --filter=blob:none https://github.com/tokio-rs/axum.git "$A"
fi
git -C "$A" checkout -q -f "$BASE"
git -C "$A" clean -qfd examples axum
git -C "$A" apply "$TASK/axum-kavach.patch"
# The integration names the SDK where the image installs it.
sed -i.bak "s#/opt/kavach/sdk/rust#$ROOT/sdk/rust#" "$A/examples/kavach-driver/Cargo.toml" && rm "$A/examples/kavach-driver/Cargo.toml.bak"
cp "$TASK/Cargo.lock" "$A/Cargo.lock"
build() {
  (cd "$A" && cargo build -q -p kavach-driver 2>"$WORK/build.log") || { tail -20 "$WORK/build.log"; exit 1; }
  cp "$A/target/debug/kavach-driver" "$1"
}
build "$WORK/bin/driver-old"
OLD=$WORK/bin/driver-old

# 3. Record the incident. The recorder captures the process's environment
# (SPEC 4.8), so the driver runs with a minimal one.
rm -rf "$WORK/rec" && mkdir -p "$WORK/rec"
(cd "$WORK/rec" && "$TASK/traffic.sh" | env -i PATH=/usr/bin:/bin HOME=/var/empty LANG=C TZ=UTC \
  KAVACH_DIR=journal KAVACH_RECORDER="$WORK/bin/kavach-recorder" "$OLD" >driver.out 2>driver.err)
FRESH=$(ls "$WORK"/rec/journal/fixtures/*.kavach | sort | head -1)

# 4. Replay on the old build. kavach exits non-zero when the verdict is a
# failure, which is the expected verdict here; the verdict is read from its output.
for f in "$FRESH" "$TASK/incident.kavach"; do
  echo "== replay $(basename "$f")"
  out=$($KAVACH replay "$f" --bin "$OLD" 2>&1) || true
  grep -E '^(service|recorded|result|kavach)' <<<"$out" || true
done

# 5. Candidates
hidden() {
  git -C "$A" apply "$TASK/hidden/test.patch"
  local out
  out=$(cd "$A" && cargo test -p axum --lib -- routing::tests::fallback 2>&1 || true)
  git -C "$A" checkout -q -- axum/src/routing/tests/fallback.rs
  if grep -q '^test result: ok\.' <<<"$out" && grep -q 'doesnt_panic_if_used_with_nested_router ... ok' <<<"$out"; then
    echo pass
  else
    echo fail
  fi
}

printf '\n%-28s %-44s %s\n' candidate "kavach diff (fresh fixture)" "hidden test"
printf '%-28s %-44s %s\n' "(old build)" "-" "$(hidden)"
for c in "$TASK"/candidates/*.patch; do
  name=$(basename "$c" .patch)
  git -C "$A" checkout -q -- axum
  git -C "$A" apply "$c"
  build "$WORK/bin/driver-$name"
  out=$($KAVACH diff "$FRESH" --old "$OLD" --new "$WORK/bin/driver-$name") || true
  verdict=$(awk '/^verdict/{print $2}' <<<"$out")
  variants=$(sed -n 's/^variants *//p' <<<"$out")
  printf '%-28s %-44s %s\n' "$name" "$verdict${variants:+ ($variants)}" "$(hidden)"
done
git -C "$A" checkout -q -- axum
