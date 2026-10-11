# Converts an OpenCode event log to ATIF with Harbor's own converter.
import json, sys
from harbor.agents.installed.opencode import OpenCode
from harbor.utils.trajectory_utils import format_trajectory_json
src, dst, model, instruction = sys.argv[1:5]
events = [json.loads(l) for l in open(src) if l.strip()]
a = OpenCode.__new__(OpenCode)
a.model_name = model
a._instruction = instruction
a.version = lambda: "1.1.8"
t = a._convert_events_to_trajectory(events)
open(dst, "w").write(format_trajectory_json(t.to_json_dict()))
print(t.final_metrics)
