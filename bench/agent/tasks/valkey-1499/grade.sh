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
# src/ builds deps/ only when its build settings change, and links them
# without depending on them: a fix in deps/ is rebuilt here, and the binaries
# relinked, or the old code is graded.
deps=()
for d in $(sed -n 's|^+++ b/deps/\([^/]*\)/.*|\1|p' /out/patch.diff | sort -u); do
  grep -q "^$d:" deps/Makefile && deps+=("$d")
done
if ((${#deps[@]})); then
  make -C deps "${deps[@]}" >&3 2>&3 || result true false fail - ""
  rm -f src/*-server src/*-cli src/*-benchmark
fi
make -j4 >&3 2>&3 || result true false fail - ""

# The whole test file, as SWE-bench runs it: FAIL_TO_PASS and PASS_TO_PASS.
git apply /task/hidden-test.patch
out=$(TERM=dumb ./runtest --single unit/introspection-2 2>&1)
echo "$out" >&3
grep -q 'All tests passed' <<<"$out" && hidden=pass || hidden=fail
git checkout -q -- tests

# kavach diff needs the integration, which the control arm's tree lacks.
if [[ $arm == control ]]; then
  { git apply /task/valkey-kavach.patch 2>&3 && make -j4 >&3 2>&3; } || result true true "$hidden" n/a ""
fi
out=$(kavach --no-banner diff /task/incident.kavach --old /opt/valkey-old/valkey-server --new src/valkey-server 2>&1) || true
# The console's probe strip and incident replay read these.
kavach --no-banner diff /task/incident.kavach --old /opt/valkey-old/valkey-server --new src/valkey-server --json >/out/kavach-diff.json 2>/dev/null || true
kavach --no-banner replay /task/incident.kavach --bin /opt/valkey-old/valkey-server --json >/out/replay-old.json 2>/dev/null || true
kavach --no-banner replay /task/incident.kavach --bin src/valkey-server --json >/out/replay-new.json 2>/dev/null || true
cp /task/incident.kavach /out/incident.kavach
echo "$out" >&3
result true true "$hidden" "$(awk '/^verdict/{print $2}' <<<"$out")" "$(sed -n 's/^variants *//p' <<<"$out")"
