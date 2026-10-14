#!/usr/bin/env bash
# Grades one attempt: applies /out/patch.diff to the tree the agent started
# from, builds it, runs the hidden upstream test and `kavach diff`, and writes
# /out/grade.json. The hidden test is the grade; `kavach diff` is recorded
# for comparison.
# Usage: grade.sh control|kavach
set -uo pipefail
arm=$1
cd "/grade-$arm"
# A missing patch means /out is not the attempt's directory (with Colima, a
# path outside $HOME mounts as an empty directory), not an empty change.
[[ -f /out/patch.diff ]] || { echo "no /out/patch.diff" >&2; exit 1; }
exec 3>/out/grade.log

result() {
  printf '{"applied":%s,"built":%s,"hidden":"%s","kavach_diff":"%s","variants":"%s"}\n' "$@" | tee /out/grade.json
  exit 0
}

# Changes to the hidden test's file are dropped, as SWE-bench does (it checks
# the file out at the base commit before applying the test patch).
if [[ -s /out/patch.diff ]]; then
  git apply --exclude='axum/src/routing/tests/fallback.rs' /out/patch.diff 2>&3 || result false false fail - ""
fi
cargo test -p axum --lib --no-run >&3 2>&3 || result true false fail - ""

git apply /task/hidden-test.patch
out=$(cargo test -p axum --lib -- routing::tests::fallback 2>&1)
echo "$out" >&3
if grep -q '^test result: ok\.' <<<"$out" && grep -q 'doesnt_panic_if_used_with_nested_router ... ok' <<<"$out"; then
  hidden=pass
else
  hidden=fail
fi
git checkout -q -- axum/src/routing/tests/fallback.rs

# kavach diff needs the integration, which the control arm's tree lacks.
if [[ $arm == control ]]; then
  git apply /task/axum-kavach.patch 2>&3 || result true true "$hidden" n/a ""
fi
cargo build -p kavach-driver >&3 2>&3 || result true true "$hidden" n/a ""
out=$(kavach --no-banner diff /task/incident.kavach --old /opt/axum-old/kavach-driver --new target/debug/kavach-driver 2>&1) || true
echo "$out" >&3
result true true "$hidden" "$(awk '/^verdict/{print $2}' <<<"$out")" "$(sed -n 's/^variants *//p' <<<"$out")"
