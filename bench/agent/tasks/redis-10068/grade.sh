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

# Changes to tests are dropped, as SWE-bench does: the hidden test replaces them.
if [[ -s /out/patch.diff ]]; then
  git apply --exclude='tests/*' /out/patch.diff 2>&3 || result false false fail - ""
fi
make -j"$(nproc)" >&3 2>&3 || result true false fail - ""

git apply /task/hidden-test.patch
out=$(TERM=dumb ./runtest --single unit/type/stream --only "/*XTRIM*" 2>&1)
echo "$out" >&3
grep -q 'All tests passed' <<<"$out" && hidden=pass || hidden=fail
git checkout -q -- tests

# kavach diff needs the integration, which the control arm's tree lacks.
if [[ $arm == control ]]; then
  { git apply /task/redis-kavach.patch 2>&3 && make -j"$(nproc)" >&3 2>&3; } || result true true "$hidden" n/a ""
fi
out=$(kavach --no-banner diff /task/incident.kavach --old /opt/redis-old/redis-server --new src/redis-server 2>&1) || true
echo "$out" >&3
result true true "$hidden" "$(awk '/^verdict/{print $2}' <<<"$out")" "$(sed -n 's/^variants *//p' <<<"$out")"
