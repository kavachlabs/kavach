#!/usr/bin/env python3
"""Plays host-protocol transcripts (SPEC.md §9.6) against a host command.

    python3 spec/host/run.py --host "python -m kavach.conformance" [--pass-env NAME]... [transcript.jsonl...]

The host command is run with `kavach-host` appended. With no transcripts given,
every *.jsonl file next to this script is played. Exits 0 if all pass.
Standard library only, Python 3.10 or later.
"""

import argparse
import json
import os
import pathlib
import queue
import shlex
import subprocess
import sys
import threading

TIMEOUT = 5.0
IGNORED = {"ready": ("sdk", "environment"), "done": ("detail",)}


class Fail(Exception):
    pass


def normalize(msg):
    msg = dict(msg)
    for key in IGNORED.get(msg.get("t"), ()):
        msg.pop(key, None)
    return msg


def start_host(cmd, env_vars, pass_env):
    env = {k: os.environ[k] for k in ("PATH", "HOME", *pass_env) if k in os.environ}
    env.update(env_vars)
    proc = subprocess.Popen(
        shlex.split(cmd) + ["kavach-host"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
    )
    lines = queue.Queue()

    def pump(stream, sink):
        for line in iter(stream.readline, b""):
            sink(line)
        sink(None)

    stderr = []
    threading.Thread(target=pump, args=(proc.stdout, lines.put), daemon=True).start()
    threading.Thread(target=pump, args=(proc.stderr, stderr.append), daemon=True).start()
    return proc, lines, stderr


def play(path, cmd, pass_env):
    steps = [json.loads(l) for l in path.read_text().splitlines() if l.strip()]
    steps = [s for s in steps if "comment" not in s]
    env_vars = steps.pop(0)["env"] if steps and "env" in steps[0] else {}
    proc, lines, stderr = start_host(cmd, env_vars, pass_env)
    try:
        for i, step in enumerate(steps, 1):
            if "send" in step:
                proc.stdin.write((json.dumps(step["send"]) + "\n").encode())
                proc.stdin.flush()
            elif "expect" in step:
                try:
                    raw = lines.get(timeout=TIMEOUT)
                except queue.Empty:
                    raise Fail(f"line {i}: timed out waiting for {step['expect']}")
                if raw is None:
                    raise Fail(f"line {i}: host closed its output; expected {step['expect']}")
                try:
                    got = json.loads(raw)
                except ValueError:
                    raise Fail(f"line {i}: host wrote a line that is not JSON: {raw!r}")
                if not isinstance(got, dict) or normalize(got) != normalize(step["expect"]):
                    raise Fail(f"line {i}:\n  expected {json.dumps(step['expect'])}\n  got      {raw.decode().strip()}")
            elif "expect_exit" in step:
                proc.stdin.close()
                try:
                    code = proc.wait(timeout=TIMEOUT)
                except subprocess.TimeoutExpired:
                    raise Fail(f"line {i}: host did not exit")
                if code != step["expect_exit"]:
                    raise Fail(f"line {i}: host exited {code}, expected {step['expect_exit']}")
                extra = lines.get(timeout=TIMEOUT)
                if extra is not None:
                    raise Fail(f"line {i}: host wrote after its last expected message: {extra!r}")
            else:
                raise Fail(f"line {i}: unknown transcript entry {step}")
    except Fail as e:
        proc.kill()
        proc.wait()
        tail = b"".join(l for l in stderr[-20:] if l).decode(errors="replace").strip()
        return f"{e}" + (f"\n  host stderr:\n    " + tail.replace("\n", "\n    ") if tail else "")
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait()
    return None


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--host", required=True, help="host command, without kavach-host")
    ap.add_argument("--pass-env", action="append", default=[], help="also pass this variable from the runner's environment")
    ap.add_argument("transcripts", nargs="*", type=pathlib.Path)
    args = ap.parse_args()
    paths = args.transcripts or sorted(pathlib.Path(__file__).parent.glob("*.jsonl"))
    failed = 0
    for path in paths:
        err = play(path, args.host, args.pass_env)
        print(f"{'PASS' if err is None else 'FAIL'}  {path.name}")
        if err:
            failed += 1
            print("  " + err.replace("\n", "\n  "))
    print(f"{len(paths) - failed}/{len(paths)} transcripts passed")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
