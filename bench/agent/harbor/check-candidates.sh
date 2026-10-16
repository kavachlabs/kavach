#!/usr/bin/env bash
# Grades each candidate fix of redis-10068 on both tasks through Harbor: the
# oracle agent runs a copy of each task whose solution is the candidate, and
# the table shows the reward and Kavach's verdict for each.
#
# Usage: check-candidates.sh [-n concurrent-trials]   (default 2)
# Jobs and task copies go to bench/agent/results/harbor-check/.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../../.." && pwd)
CANDIDATES=$ROOT/bench/agent/tasks/redis-10068/candidates
n=2
[[ ${1:-} == -n ]] && n=$2

# Docker mounts the trial directories, so they must be under $HOME (Colima).
out=$ROOT/bench/agent/results/harbor-check/$(date +%Y%m%d-%H%M%S)
mkdir -p "$out/tasks"
: >"$out/empty.patch"
for c in "$CANDIDATES"/*.patch "$out/empty.patch"; do
  name=$(basename "$c" .patch)
  for arm in control kavach; do
    copy=$out/tasks/$name--redis-10068-$arm
    cp -R "$HERE/tasks/redis-10068-$arm" "$copy"
    cp "$c" "$copy/solution/fix.patch"
  done
done

# The copies name the same prebuilt images; build them first with
# image-tag.py build redis-10068.
harbor run -p "$out/tasks" -a oracle -n "$n" -o "$out" --job-name job -q -y >"$out/harbor.log" 2>&1 \
  || { tail -20 "$out/harbor.log"; exit 1; }

python3 - "$out/job" <<'EOF'
import json, sys
from pathlib import Path
rows = {}
for result in Path(sys.argv[1]).glob("*/result.json"):
    r = json.loads(result.read_text())
    name, arm = r["task_name"].rsplit("--redis-10068-", 1)
    reward = ((r.get("verifier_result") or {}).get("rewards") or {}).get("reward", "error")
    k = result.parent / "verifier" / "kavach.json"
    kavach = json.loads(k.read_text()) if k.exists() else {"verdict": "-", "variants": ""}
    v = kavach["verdict"]
    if kavach["variants"]:
        v += " (" + kavach["variants"].split(" reproduce")[0] + ")"
    rows[name, arm] = (reward, v)
print(f"{'candidate':<24} {'arm':<8} {'reward':<7} kavach")
for (name, arm), (reward, v) in sorted(rows.items()):
    print(f"{name:<24} {arm:<8} {reward!s:<7} {v}")
EOF
echo "jobs: $out/job"
