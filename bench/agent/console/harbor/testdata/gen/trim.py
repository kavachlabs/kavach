# Trims a real OpenCode transcript: tool outputs cut to keep the fixture small,
# and a cost of $0.002 put on each step (the model's provider reported none).
import json, sys
src, dst, limit = sys.argv[1], sys.argv[2], int(sys.argv[3]) if len(sys.argv) > 3 else 10**9
def cut(s, n=400):
    return s if len(s) <= n else s[:n] + "\n…"
out = []
for line in open(src):
    e = json.loads(line)
    p = e.get("part", {})
    st = p.get("state", {})
    if "output" in st and not p.get("tool", "").startswith("kavach_"):
        st["output"] = cut(st["output"])
    md = st.get("metadata", {})
    for k in ("output", "preview", "diff"):
        if isinstance(md.get(k), str):
            md[k] = cut(md[k], 800 if k == "diff" else 400)
    md.pop("filediff", None)
    if e["type"] == "step_finish":
        p["cost"] = 0.002
    out.append(json.dumps(e))
    if len(out) >= limit:
        break
open(dst, "w").write("\n".join(out) + "\n")
