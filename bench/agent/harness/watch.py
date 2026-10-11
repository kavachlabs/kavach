#!/usr/bin/env python3
"""Shows an agent attempt as it runs, or replays a finished one.

    watch.py <attempt-dir>             replay
    watch.py -f <attempt-dir>          follow until the agent finishes

An attempt directory is one that run.sh writes (it holds transcript.jsonl).
Colour is on when stdout is a terminal and NO_COLOR is unset.
"""
import argparse
import json
import os
import shutil
import sys
import textwrap
import time

USE_COLOR = sys.stdout.isatty() and "NO_COLOR" not in os.environ


def paint(code):
    return lambda s: f"\033[{code}m{s}\033[0m" if USE_COLOR else s


dim, bold, red, green, yellow = paint("2"), paint("1"), paint("31"), paint("32"), paint("33")
blue, magenta, cyan, kavach = paint("34"), paint("35"), paint("36"), paint("1;92")
WIDTH = min(shutil.get_terminal_size((100, 20)).columns, 120)


def short(n):
    return f"{n / 1000:.1f}k" if n >= 1000 else str(n)


class View:
    def __init__(self, lines):
        self.lines = lines
        self.t0 = None
        self.steps = 0
        self.tokens = {"input": 0, "cache": 0, "output": 0}
        self.tools = {}

    def clock(self, ms):
        if self.t0 is None:
            self.t0 = ms
        s = (ms - self.t0) // 1000
        return dim(f"{s // 60:02d}:{s % 60:02d}")

    def block(self, text, color=dim, limit=None):
        rows = text.rstrip("\n").splitlines()
        limit = self.lines if limit is None else limit
        for row in rows[:limit]:
            print("        " + color(row[: WIDTH - 10]))
        if len(rows) > limit:
            print("        " + dim(f"… {len(rows) - limit} more lines"))

    def event(self, e):
        kind, part = e.get("type"), e.get("part", {})
        when = self.clock(e.get("timestamp", 0))
        if kind == "text":
            text = part.get("text", "").strip()
            if text:
                wrapped = textwrap.wrap(text, WIDTH - 10) or [""]
                print(f"{when}  {bold('●')} {bold(wrapped[0])}")
                for row in wrapped[1:]:
                    print("        " + bold(row))
        elif kind == "tool_use":
            self.tool(when, part)
        elif kind == "step_finish":
            self.steps += 1
            t = part.get("tokens", {})
            self.tokens["input"] += t.get("input", 0)
            self.tokens["cache"] += t.get("cache", {}).get("read", 0)
            self.tokens["output"] += t.get("output", 0)
            print(dim(f"{'':7}─ step {self.steps} · in {short(t.get('input', 0))}"
                      f" (+{short(t.get('cache', {}).get('read', 0))} cached) · out {short(t.get('output', 0))}"))
        elif kind == "error":
            err = e.get("error", {})
            print(f"{when}  {red(bold('✖ ' + err.get('name', 'error')))} {red(err.get('data', {}).get('message', ''))}")

    def tool(self, when, part):
        name, state = part.get("tool", "?"), part.get("state", {})
        args, meta, out = state.get("input", {}), state.get("metadata", {}), state.get("output", "")
        self.tools[name] = self.tools.get(name, 0) + 1
        if state.get("status") == "error":
            print(f"{when}  {red('✖ ' + name)} {red(str(state.get('error', ''))[:WIDTH])}")
            return
        if name.startswith("kavach_"):
            self.kavach(when, name.split("_", 2)[-1], args, out)
        elif name == "bash":
            cmd = args.get("command", "").strip().splitlines() or [""]
            code = meta.get("exit")
            mark = red(f" exit {code}") if code not in (0, None) else ""
            print(f"{when}  {yellow('$')} {yellow(cmd[0][: WIDTH - 12])}{dim(' …') if len(cmd) > 1 else ''}{mark}")
            self.block(meta.get("output", out))
        elif name in ("edit", "write"):
            path = args.get("filePath", "")
            print(f"{when}  {magenta('✎ ' + name)} {magenta(path)}")
            diff = meta.get("diff", "")
            rows = [r for r in diff.splitlines() if r[:1] in "+-@" and not r.startswith(("+++", "---"))]
            for r in rows[: self.lines * 3]:
                c = green if r[0] == "+" else red if r[0] == "-" else cyan
                print("        " + c(r[: WIDTH - 10]))
        elif name == "read":
            start, n = args.get("offset"), args.get("limit")
            span = f" lines {start or 1}–{(start or 1) + n - 1}" if n else f" from line {start}" if start else ""
            print(f"{when}  {blue('▤ read')} {blue(args.get('filePath', ''))}{dim(span)}")
        elif name in ("grep", "glob", "list"):
            what = args.get("pattern") or args.get("path", "")
            where = args.get("path", "") if args.get("pattern") else ""
            print(f"{when}  {cyan('⌕ ' + name)} {cyan(what)} {dim(where)} {dim(out.splitlines()[0] if out else '')}")
        else:
            print(f"{when}  {dim('· ' + name)} {dim(json.dumps(args)[: WIDTH - 20])}")

    def kavach(self, when, verb, args, out):
        try:
            r = json.loads(out)
        except ValueError:
            print(f"{when}  {kavach('◆ kavach ' + verb)}")
            self.block(out)
            return
        verdict = r.get("status") or r.get("verdict") or ""
        color = green if r.get("passed") else red
        print(f"{when}  {kavach('◆ kavach ' + verb)} {dim(args.get('new') or args.get('bin') or '')}  {color(bold(verdict))}")
        if "old" in r and verb == "diff":
            print("        " + dim(f"old build: {r['old']}"))
        checks = (r.get("variants") or {}).get("checks", [])
        if checks:
            rep = sum(1 for c in checks if c.get("reproduces"))
            ok = sum(1 for c in checks if c.get("reproduces") and c.get("passed"))
            print("        " + dim(f"variants: {rep} of {len(checks)} reproduce on old · {ok} of those pass on new"))
        div = r.get("divergence") or {}
        if div.get("detail") and not r.get("passed"):
            print("        " + red(div["detail"]))
        if r.get("detail"):
            self.block(r["detail"], red)

    def footer(self, attempt, results=True):
        tools = " ".join(f"{k}×{v}" for k, v in sorted(self.tools.items(), key=lambda kv: -kv[1]))
        t = self.tokens
        print(dim("─" * WIDTH))
        print(f"{bold(str(self.steps))} steps · in {short(t['input'])} (+{short(t['cache'])} cached) · out {short(t['output'])}")
        print(dim(tools))
        if not results:
            return
        for name in ("agent.json", "grade.json"):
            path = os.path.join(attempt, name)
            if os.path.exists(path):
                show_result(name[:-5], json.load(open(path)))


def summarize(attempt):
    """Steps and token totals of an attempt's transcript."""
    steps, tokens = 0, {"input": 0, "cache": 0, "output": 0}
    try:
        lines = open(os.path.join(attempt, "transcript.jsonl"), errors="replace")
    except OSError:
        return steps, tokens
    for line in lines:
        try:
            e = json.loads(line)
        except ValueError:
            continue
        if e.get("type") == "step_finish":
            t = e.get("part", {}).get("tokens", {})
            steps += 1
            tokens["input"] += t.get("input", 0)
            tokens["cache"] += t.get("cache", {}).get("read", 0)
            tokens["output"] += t.get("output", 0)
    return steps, tokens


def show_result(what, r):
    if what == "agent":
        bad = r.get("api_error") or r.get("exit") not in (0, None)
        msg = f"agent  exit {r.get('exit')} · {r.get('seconds')} s"
        if r.get("exit") == 124:
            msg += " · timed out"
        if r.get("api_error"):
            msg += f" · API error: {r['api_error'][:WIDTH - 40]}"
        print((red if bad else dim)(msg))
    else:
        hidden = r.get("hidden")
        badge = green(bold(" PASS ")) if hidden == "pass" else red(bold(" FAIL "))
        print(f"grade {badge} hidden test · kavach diff {bold(r.get('kavach_diff', ''))} {dim(r.get('variants', ''))}")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("attempt", help="attempt directory, or its transcript.jsonl")
    ap.add_argument("-f", "--follow", action="store_true", help="follow until the agent finishes")
    ap.add_argument("--pid", type=int, help="with -f, also stop when this process exits")
    ap.add_argument("-n", "--lines", type=int, default=6, help="output lines shown per tool call")
    ap.add_argument("--result", action="store_true", help="only print the agent and grade results")
    a = ap.parse_args()
    attempt = os.path.dirname(a.attempt) if a.attempt.endswith(".jsonl") else a.attempt
    if a.result:
        for name in ("agent", "grade"):
            path = os.path.join(attempt, name + ".json")
            if os.path.exists(path):
                show_result(name, json.load(open(path)))
        return
    transcript = os.path.join(attempt, "transcript.jsonl")
    parts = os.path.normpath(attempt).split(os.sep)
    print(bold(" · ".join(parts[-4:])) if len(parts) >= 4 else bold(attempt))

    view = View(a.lines)
    while a.follow and not os.path.exists(transcript):
        time.sleep(0.2)
    with open(transcript, errors="replace") as f:
        buf = ""
        while True:
            chunk = f.readline()
            if chunk:
                buf += chunk
                if buf.endswith("\n"):
                    try:
                        view.event(json.loads(buf))
                    except ValueError:
                        pass
                    buf = ""
                    sys.stdout.flush()
                continue
            if not a.follow or os.path.exists(os.path.join(attempt, "agent.json")):
                break
            if a.pid:
                try:
                    os.kill(a.pid, 0)
                except OSError:
                    break
            time.sleep(0.2)
    # When following, run.sh prints the results once grading is done.
    view.footer(attempt, results=not a.follow)


if __name__ == "__main__":
    try:
        main()
    except (KeyboardInterrupt, BrokenPipeError):
        sys.stderr.close()
