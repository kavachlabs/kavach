#!/usr/bin/env python3
"""Measure how often a coding agent reaches for Kavach, from the user's words alone.

Each prompt in prompts.jsonl runs in a fresh copy of its workspace (a small repo
that never mentions Kavach) with the Kavach plugin and MCP server installed and
nothing else from the user's setup. A run stops at the agent's first decision:
it used Kavach (a kavach skill, a kavach MCP tool, or a `kavach` shell command),
or it went ahead without it (tried to edit a file), or it ran out of turns.
With --distractors, generic debugging and testing skills compete for the same
prompts, as they do in a real setup.
Edits and shell commands are denied, so nothing outside the copy changes.

  python3 bench/trigger/run.py --plugin-dir . --kavach-bin /tmp/kavach --out results.jsonl
"""

import argparse
import concurrent.futures
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
EDIT_TOOLS = {"Edit", "Write", "MultiEdit", "NotebookEdit"}


def classify(tool, inp):
    """Return the Kavach entry point a tool call uses, or None."""
    if tool == "Skill":
        name = str(inp.get("skill", ""))
        return name.split(":")[-1] if "kavach" in name else None
    if tool.startswith("mcp__kavach__"):
        return tool.removeprefix("mcp__")
    if tool == "Bash" and re.search(r"(^|[\s;&|(/])kavach(\s|$)", str(inp.get("command", ""))):
        return "cli"
    return None


def run_one(case, args):
    work = tempfile.mkdtemp(prefix="trigger-")
    try:
        repo = os.path.join(work, case["workspace"])
        shutil.copytree(os.path.join(HERE, "workspaces", case["workspace"]), repo)
        subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
        mcp = os.path.join(work, "mcp.json")
        with open(mcp, "w") as f:
            json.dump({"mcpServers": {"kavach": {"command": args.kavach_bin, "args": ["mcp"]}}}, f)
        cmd = [
            args.claude, "-p", case["prompt"],
            "--output-format", "stream-json", "--verbose",
            "--max-turns", str(args.max_turns),
            "--setting-sources", "project",
            "--strict-mcp-config", "--mcp-config", mcp,
            "--allowedTools", "Read", "Grep", "Glob", "Skill", "mcp__kavach",
        ]
        if args.plugin_dir:
            cmd += ["--plugin-dir", args.plugin_dir]
        if args.distractors:
            cmd += ["--plugin-dir", os.path.join(HERE, "distractors")]
        if args.model:
            cmd += ["--model", args.model]
        start = time.time()
        proc = subprocess.Popen(cmd, cwd=repo, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        outcome, via, tools, mentioned, text = "undecided", None, [], False, ""
        try:
            for line in proc.stdout:
                if time.time() - start > args.timeout:
                    outcome = "timeout"
                    break
                try:
                    ev = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if ev.get("type") != "assistant":
                    continue
                for block in ev.get("message", {}).get("content", []):
                    if block.get("type") == "text":
                        text += block["text"]
                        mentioned = mentioned or "kavach" in block["text"].lower()
                    if block.get("type") != "tool_use":
                        continue
                    tool, inp = block["name"], block.get("input", {})
                    tools.append(tool + (f"({inp.get('skill')})" if tool == "Skill" else ""))
                    via = classify(tool, inp)
                    if via:
                        outcome = "kavach"
                        break
                    if tool in EDIT_TOOLS:
                        outcome = "edited"
                        break
                if outcome != "undecided":
                    break
        finally:
            proc.kill()
            proc.wait()
        return {
            "id": case["id"], "expect": case["expect"], "outcome": outcome, "via": via,
            "mentioned": mentioned, "tools": tools, "seconds": round(time.time() - start, 1),
            "text": text[-400:],
        }
    finally:
        shutil.rmtree(work, ignore_errors=True)


def summarize(results):
    pos = [r for r in results if r["expect"] != "none"]
    neg = [r for r in results if r["expect"] == "none"]
    hit = [r for r in pos if r["outcome"] == "kavach"]
    # A fix can also start straight from the MCP tools or the CLI.
    right = [r for r in hit if r["via"] == "kavach-" + r["expect"]
             or (r["expect"] == "fix" and (r["via"] == "cli" or r["via"].startswith("kavach_")))]
    fp = [r for r in neg if r["outcome"] == "kavach"]
    print(f"should use Kavach: {len(hit)}/{len(pos)} used it ({len(right)} through the expected skill)")
    for cat in ("fix", "setup", "implement", "invariants"):
        rs = [r for r in pos if r["expect"] == cat]
        print(f"  {cat:<11} {sum(r['outcome'] == 'kavach' for r in rs)}/{len(rs)}")
    print(f"should not:        {len(fp)}/{len(neg)} used it anyway")
    print(f"mentioned Kavach without using it: {sum(r['mentioned'] and r['outcome'] != 'kavach' for r in results)}")
    for r in results:
        if (r["expect"] == "none") == (r["outcome"] == "kavach"):
            print(f"  miss {r['id']:<9} {r['outcome']:<9} via={r['via']} tools={','.join(r['tools'][:6])}")


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--plugin-dir", help="the Kavach plugin (the repo root); omit to run with no skills")
    p.add_argument("--kavach-bin", required=True, help="kavach binary whose `kavach mcp` the agent gets")
    p.add_argument("--distractors", action="store_true",
                   help="also install distractors/, generic debugging and testing skills that compete with Kavach's")
    p.add_argument("--claude", default="claude")
    p.add_argument("--model")
    p.add_argument("--only", help="comma-separated prompt ids")
    p.add_argument("--repeat", type=int, default=1)
    p.add_argument("--max-turns", type=int, default=8)
    p.add_argument("--timeout", type=int, default=240)
    p.add_argument("--jobs", type=int, default=4)
    p.add_argument("--out", required=True)
    args = p.parse_args()
    args.kavach_bin = os.path.abspath(args.kavach_bin)
    if args.plugin_dir:
        args.plugin_dir = os.path.abspath(args.plugin_dir)

    cases = [json.loads(l) for l in open(os.path.join(HERE, "prompts.jsonl")) if l.strip()]
    if args.only:
        keep = set(args.only.split(","))
        cases = [c for c in cases if c["id"] in keep]
    cases = cases * args.repeat
    results = []
    with open(args.out, "w") as out, concurrent.futures.ThreadPoolExecutor(args.jobs) as pool:
        for r in pool.map(lambda c: run_one(c, args), cases):
            results.append(r)
            out.write(json.dumps(r) + "\n")
            out.flush()
            print(f"{r['id']:<9} {r['expect']:<10} {r['outcome']:<9} {r['via'] or '':<16} {r['seconds']}s", file=sys.stderr)
    summarize(results)


if __name__ == "__main__":
    main()
