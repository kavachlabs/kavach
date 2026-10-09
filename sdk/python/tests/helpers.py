"""Shared test helpers: a stub recorder and a frame splitter."""

from __future__ import annotations

import json
import os
import sys
import tempfile
import textwrap

# A recorder that dumps the raw record stream to argv[1], answers `ready` after
# `open` and `closed` after `close`, and then does whatever argv[2] says:
#   "ok"        behave
#   "die"       exit right after open
#   "fatal"     send a fatal error after open, then keep reading
#   "fixture"   send a fixture message after open
#   "silent"    never answer ready/durable/closed
STUB = textwrap.dedent(
    r"""
    import json, sys
    out, mode = sys.argv[1], sys.argv[2]
    stdin = sys.stdin.buffer
    def say(m):
        sys.stdout.write(json.dumps(m) + "\n"); sys.stdout.flush()
    f = open(out, "wb")
    def uvarint():
        x = shift = 0
        while True:
            c = stdin.read(1)
            if not c: return None
            f.write(c)
            x |= (c[0] & 0x7F) << shift
            if c[0] < 0x80: return x
            shift += 7
    while True:
        n = uvarint()
        if n is None: break
        body = stdin.read(n)
        f.write(body)
        f.flush()
        kind = body[0]
        if kind == 1:
            if mode == "die": sys.exit(3)
            if mode != "silent": say({"t": "ready", "protocol": 1, "recorder": "stub", "run": "r", "file": "/x"})
            if mode == "fatal": say({"t": "error", "message": "disk full", "fatal": True})
            if mode == "fixture": say({"t": "fixture", "file": "/fx/1.kavach", "seq": "3", "failure": "panic: boom"})
        elif kind == 6 and body[1] == 1 and mode != "silent":
            say({"t": "durable", "seq": "0"})
        elif kind == 7:
            if mode != "silent":
                say({"t": "closed"})
                break
    """
)


def write_stub(tmp: str) -> str:
    path = os.path.join(tmp, "stub_recorder.py")
    with open(path, "w") as f:
        f.write(STUB)
    return path


def stub_command(tmp: str, mode: str = "ok") -> tuple[list[str], str]:
    """(recorder_command, path of the dump file)."""
    dump = os.path.join(tmp, "dump.bin")
    return [sys.executable, write_stub(tmp), dump, mode], dump


def uvarint_decode(b: bytes, i: int) -> tuple[int, int]:
    x = shift = 0
    while True:
        c = b[i]
        i += 1
        x |= (c & 0x7F) << shift
        if c < 0x80:
            return x, i
        shift += 7


def split_frames(stream: bytes) -> list[tuple[int, bytes]]:
    """Split a record stream into (kind, payload) frames."""
    out, i = [], 0
    while i < len(stream):
        n, i = uvarint_decode(stream, i)
        out.append((stream[i], stream[i + 1 : i + n]))
        i += n
    return out


def read_stream(dump: str) -> list[tuple[int, bytes]]:
    with open(dump, "rb") as f:
        return split_frames(f.read())


def tmpdir() -> tempfile.TemporaryDirectory:
    return tempfile.TemporaryDirectory(prefix="kavach-test-")


def open_obj(frames: list[tuple[int, bytes]]) -> dict:
    return json.loads(frames[0][1])
