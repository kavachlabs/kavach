#!/bin/sh
# Runs one attempt in the task container: the model gets /task/TASK.md and
# works in /work. Leaves transcript.jsonl, agent.log, patch.diff and
# agent.json in /out.
# Usage: agent.sh <model> <timeout-seconds>
set -u
cd /work
start=$(date +%s)
timeout "$2" opencode run --model "$1" --format json "$(cat /task/TASK.md)" >/out/transcript.jsonl 2>/out/agent.log
status=$?
git add -A && git diff --cached HEAD >/out/patch.diff
# An API error (no credit, rate limit, provider down) ends the run without the
# model having finished; it is recorded so the attempt is not counted as the
# model failing.
python3 - "$status" "$(($(date +%s) - start))" <<'EOF2' >/out/agent.json
import json, sys
error = None
for line in open("/out/transcript.jsonl", errors="replace"):
    try:
        e = json.loads(line)
    except ValueError:
        continue
    if e.get("type") == "error":
        error = e.get("error", {}).get("data", {}).get("message") or e.get("error", {}).get("name")
print(json.dumps({"exit": int(sys.argv[1]), "seconds": int(sys.argv[2]), "api_error": error}))
EOF2
