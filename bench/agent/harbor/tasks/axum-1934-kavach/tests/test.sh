#!/bin/bash
# Grades the agent's change with the task's grade.sh and turns its result
# into Harbor's reward and kavach.json. The reward is the hidden upstream test
# alone; the Kavach verdict is recorded beside it.
set -uo pipefail
logs=/logs/verifier
# Daytona and Modal run this in a login shell. A directory of the image's PATH
# that the shell lost (its tools are then missing) is a broken environment,
# not a failed attempt, so fail without a reward.
for d in $(tr '\0' '\n' </proc/1/environ 2>/dev/null | sed -n 's/^PATH=//p' | tr : ' '); do
  case ":$PATH:" in *":$d:"*) ;; *) echo "grader environment: $d is not on PATH" >&2; exit 1 ;; esac
done

# No patch means the collect hook failed, not that the agent changed nothing
# (that is an empty patch), so fail without a reward rather than score it.
cp /logs/artifacts/patch.diff /out/patch.diff || exit 1
/opt/harness/grade.sh "$ARM" >/dev/null || exit 1
cp /out/grade.json /out/grade.log "$logs/"
# Diff tasks also save the JSON reports and the fixture the console draws
# from; contract tasks, the traffic they checked and its replays.
cp /out/kavach-diff.json /out/replay-old.json /out/replay-new.json /out/incident.kavach /out/traffic.kavach "$logs/" 2>/dev/null || true

python3 - "$logs" <<'PY'
import json, re, sys
logs = sys.argv[1]
g = json.load(open(f"{logs}/grade.json"))
# grade.json has no field for the old build's verdict; it is the `old` line
# of the kavach diff output, the last thing grade.sh logs.
old = ""
for line in open(f"{logs}/grade.log", errors="replace"):
    m = re.match(r"old +(\S+)$", line.rstrip())
    if m:
        old = m.group(1)
verdict = g["kavach_diff"] if g["kavach_diff"] not in ("", "-") else "n/a"
json.dump({"verdict": verdict, "variants": g["variants"], "old": old},
          open(f"{logs}/kavach.json", "w"))
open(f"{logs}/reward.txt", "w").write("1\n" if g["hidden"] == "pass" else "0\n")
PY
