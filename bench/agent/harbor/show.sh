#!/usr/bin/env bash
# Opens a finished attempt in the OpenCode TUI.
#
#   show.sh <trial-dir>
#
# <trial-dir> is a Harbor trial directory (<jobs>/<job>/<task>__<id>) or its
# agent/ directory. The session exported after the run
# (agent/opencode-session.json) is imported into a separate OpenCode data
# directory, so benchmark sessions stay out of your own session list
# (KAVACH_SHOW_DATA, default ~/.local/share/kavach-bench/opencode), and opened
# there. OpenCode's config is kept there too: plugins in your own config can
# make `opencode import` fail. Needs opencode on PATH: the run's version (1.1.8)
# or later.
set -euo pipefail

[[ $# -eq 1 ]] || { sed -n '2,/^set /{/^#/s/^# \{0,1\}//p;}' "$0"; exit 2; }
file=$1/agent/opencode-session.json
[[ -f $file ]] || file=$1/opencode-session.json
[[ -f $file ]] || { echo "no opencode-session.json in $1" >&2; exit 1; }

data=${KAVACH_SHOW_DATA:-$HOME/.local/share/kavach-bench/opencode}
work=$data/sessions
mkdir -p "$work"
# OpenCode lists a session under the project it ran in, which for a git
# repository is identified by its first commit. Filing the copy under the
# "global" project (any directory outside a git repository) lets it open
# here.
copy=$(mktemp "${TMPDIR:-/tmp}/opencode-session.XXXXXX")
trap 'rm -f "$copy"' EXIT
session=$(python3 - "$file" "$copy" <<'EOF'
import json, sys
s = json.load(open(sys.argv[1]))
s["info"]["projectID"] = "global"
# OpenCode after 1.1.8 requires a slug, which 1.1.8's export lacks.
s["info"].setdefault("slug", s["info"]["id"])
json.dump(s, open(sys.argv[2], "w"))
print(s["info"]["id"])
EOF
)
cd "$work"
export XDG_DATA_HOME=$data XDG_CONFIG_HOME=$data/config
opencode import "$copy" >/dev/null
exec opencode --session "$session"
