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
printf '{"exit":%d,"seconds":%d}\n' "$status" "$(($(date +%s) - start))" >/out/agent.json
