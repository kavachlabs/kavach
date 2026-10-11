#!/usr/bin/env bash
# Watches a running attempt in the OpenCode TUI.
#
#   attach.sh <trial-dir> [--web]
#
# Reads <trial-dir>/agent/opencode-live.json and runs `opencode attach` on
# the attempt's read-only view; --web prints the URL to open in a browser
# instead. The view refuses prompts and other changes, so typing in the TUI
# gets an error rather than reaching the agent.
set -euo pipefail

[[ $# -ge 1 ]] || { sed -n '2,/^set /{/^#/s/^# \{0,1\}//p;}' "$0"; exit 2; }
live=$1/agent/opencode-live.json
[[ -f $live ]] || live=$1/opencode-live.json
[[ -f $live ]] || { echo "no opencode-live.json in $1 (not started yet?)" >&2; exit 1; }
read -r url session dir finished < <(python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
print(d["url"], d["session"], d.get("dir", "/"), d.get("finished", "-"))' "$live")
if [[ $finished != - ]]; then
  echo "the attempt finished at $finished; open it with show.sh $1" >&2
  exit 1
fi
if [[ ${2:-} == --web ]]; then
  echo "$url"
  exit 0
fi
# No --dir: attach changes to that directory on this machine, and the
# attempt's directory ($dir) exists only in its container.
exec opencode attach "$url" --session "$session"
