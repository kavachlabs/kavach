#!/usr/bin/env python3
"""A stand-in for kavach-recorder that checks an SDK's record stream (SPEC.md §10.6).

    python3 spec/recorder/sdk/fake_recorder.py <case.json> <result.json>

An SDK under test is started with this as its recorder command instead of
kavach-recorder. It reads the record stream (§10.2) from standard input, or,
when the open frame has `ring`, from the shared-memory ring that is file
descriptor 3 (§10.7), with standard input as the doorbell,
answers on standard output as the recorder would (§10.3), sending the control
messages the case scripts, and when the stream closes compares every frame it
received with the case's `frames`. It writes {"pass": bool, "error": str,
"frames": [...]} to result.json and exits 0 if the frames match, 1 if not.
Standard library only, Python 3.10 or later.
"""

import base64
import json
import mmap
import os
import struct
import sys
import threading
import time

RECORD_TYPES = {1: "input", 2: "clock", 3: "rand", 4: "output", 5: "marker", 7: "gateway", 9: "config"}
SCOPES = {0: "remote", 1: "local"}
FORMS = {0: "value", 1: "sha256"}


class Malformed(Exception):
    pass


class Buf:
    def __init__(self, b):
        self.b, self.i = b, 0

    def take(self, n):
        if self.i + n > len(self.b):
            raise Malformed("payload too short")
        out = self.b[self.i:self.i + n]
        self.i += n
        return out

    def u8(self):
        return self.take(1)[0]

    def i64(self):
        return struct.unpack("<q", self.take(8))[0]

    def uvarint(self):
        x = shift = 0
        for _ in range(10):
            c = self.u8()
            x |= (c & 0x7F) << shift
            if c < 0x80:
                return x
            shift += 7
        raise Malformed("uvarint too long")

    def bytes(self):
        return self.take(self.uvarint())

    def string(self):
        try:
            return self.bytes().decode("utf-8")
        except UnicodeDecodeError:
            raise Malformed("string is not UTF-8")

    def done(self):
        return self.i == len(self.b)


def b64(b):
    return base64.b64encode(b).decode()


def decode_record(p):
    t, flags = p.u8(), p.u8()
    if t not in RECORD_TYPES:
        raise Malformed(f"record type {t:#04x} is not one an SDK may send")
    r = {"frame": "record", "type": RECORD_TYPES[t], "critical": bool(flags & 1)}
    if flags & ~1:
        raise Malformed(f"reserved flag bits set: {flags:#04x}")
    if t == 1:
        r.update(source=p.string(), position=p.string(), data=b64(p.bytes()))
    elif t == 2:
        r.update(unix_nanos=str(p.i64()))
    elif t == 3:
        r.update(data=b64(p.bytes()))
    elif t == 4:
        r.update(sink=p.string(), data=b64(p.bytes()), scope=SCOPES.get(p.u8(), "invalid"))
    elif t == 5:
        r.update(kind=p.string(), message=p.string(), data=b64(p.bytes()))
    elif t == 7:
        r.update(gateway=p.string(), request=b64(p.bytes()), response=b64(p.bytes()),
                 error=p.string(), scope=SCOPES.get(p.u8(), "invalid"))
    elif t == 9:
        r.update(key=p.string(), present=bool(p.u8()), value=b64(p.bytes()), source=p.string())
    return r


def decode_facts(p):
    facts = {}
    for _ in range(p.uvarint()):
        key, form, value = p.string(), p.u8(), p.bytes()
        facts[key] = {"unset": True} if form == 2 else {FORMS.get(form, "invalid"): b64(value)}
    return facts


def decode_frame(kind, payload):
    p = Buf(payload)
    if kind == 0x01:
        f = {"frame": "open", "open": json.loads(payload)}
        p.i = len(payload)
    elif kind == 0x02:
        f = decode_record(p)
    elif kind == 0x03:
        f = {"frame": "step_end"}
    elif kind == 0x04:
        f = {"frame": "facts", "facts": decode_facts(p)}
    elif kind == 0x05:
        f = {"frame": "snapshot", "data": b64(p.bytes())}
    elif kind == 0x06:
        f = {"frame": "flush", "durable": bool(p.u8())}
    elif kind == 0x07:
        f = {"frame": "close"}
    else:
        raise Malformed(f"unknown frame kind {kind:#04x}")
    if not p.done():
        raise Malformed(f"{f['frame']} frame has {len(payload) - p.i} trailing bytes")
    return f


class Ring:
    """The record stream in the ring of SPEC.md §10.7, read like a file."""

    HEADER = 256

    def __init__(self, fd, capacity):
        self.cap = capacity
        self.mem = mmap.mmap(fd, self.HEADER + capacity)
        if self.mem[:8] != b"KVRING01" or struct.unpack_from("<Q", self.mem, 8)[0] != capacity:
            raise Malformed("bad ring header")
        self.pos = 0
        self.ended = threading.Event()
        # Standard input carries only doorbell bytes; its end means the service ended.
        threading.Thread(target=self._doorbell, daemon=True).start()

    def _doorbell(self):
        try:
            while os.read(0, 4096):
                pass
        finally:
            self.ended.set()

    def read(self, n):
        out = b""
        while len(out) < n:
            ended = self.ended.is_set()
            write = struct.unpack_from("<Q", self.mem, 64)[0]
            if write - self.pos > self.cap:
                raise Malformed("ring holds more bytes than its capacity")
            take = min(n - len(out), write - self.pos)
            if take > 0:
                off = self.HEADER + self.pos % self.cap
                first = min(take, self.cap - self.pos % self.cap)
                out += bytes(self.mem[off:off + first]) + bytes(self.mem[self.HEADER:self.HEADER + take - first])
                self.pos += take
                struct.pack_into("<Q", self.mem, 128, self.pos)
            elif ended:
                break
            else:
                time.sleep(0.0005)
        return out


def read_uvarint(stream):
    x = shift = 0
    for n in range(10):
        c = stream.read(1)
        if not c:
            if n:
                raise Malformed("stream ends inside a frame length")
            return None
        x |= (c[0] & 0x7F) << shift
        if c[0] < 0x80:
            return x
        shift += 7
    raise Malformed("frame length uvarint too long")


def matches(want, got, open_frame=False):
    """want "*" matches any value; an open frame's expected keys are a subset."""
    if want == "*":
        return True
    if isinstance(want, dict) and isinstance(got, dict):
        if open_frame:
            return all(k in got and matches(v, got[k]) for k, v in want.items())
        if want.get("frame") == "open":
            return got.get("frame") == "open" and matches(want["open"], got.get("open"), open_frame=True)
        if want.get("frame") == "facts" and got.get("frame") == "facts":
            extra = [k for k in got["facts"] if k not in want["facts"] and not k.startswith("host.x.")]
            return not extra and all(k in got["facts"] and matches(v, got["facts"][k]) for k, v in want["facts"].items())
        return want.keys() == got.keys() and all(matches(v, got[k]) for k, v in want.items())
    return want == got


def compare(want, got):
    for i, (w, g) in enumerate(zip(want, got)):
        if not matches(w, g):
            return f"frame {i}:\n  expected {json.dumps(w)}\n  got      {json.dumps(g)}"
    if len(got) < len(want):
        return f"frame {len(got)}: expected {json.dumps(want[len(got)])}, but the stream ended"
    if len(got) > len(want):
        return f"frame {len(want)}: unexpected extra frame {json.dumps(got[len(want)])}"
    return None


def main():
    case = json.load(open(sys.argv[1]))
    result_path = sys.argv[2]
    control = {}
    for c in case.get("control", []):
        control.setdefault(c["after_step"], []).append(c["send"])

    def say(msg):
        sys.stdout.write(json.dumps(msg) + "\n")
        sys.stdout.flush()

    frames, error, steps, records = [], None, 0, 0
    stdin = sys.stdin.buffer
    try:
        while True:
            n = read_uvarint(stdin)
            if n is None:
                break
            if n < 1:
                raise Malformed("empty frame")
            body = stdin.read(n)
            if len(body) < n:
                raise Malformed("stream ends inside a frame")
            f = decode_frame(body[0], body[1:])
            frames.append(f)
            if f["frame"] == "open":
                if f["open"].get("ring_path"):
                    fd = os.open(f["open"]["ring_path"], os.O_RDWR)
                    os.unlink(f["open"]["ring_path"])
                    stdin = Ring(fd, f["open"]["ring"])
                elif f["open"].get("ring"):
                    stdin = Ring(3, f["open"]["ring"])
                say({"t": "ready", "protocol": 1, "recorder": "fake-recorder", "run": "conformance", "file": "/dev/null"})
            elif f["frame"] == "record":
                records += 1
            elif f["frame"] == "step_end":
                for msg in control.get(steps, []):
                    say(msg)
                steps += 1
            elif f["frame"] == "flush" and f["durable"]:
                say({"t": "durable", "seq": str(max(records - 1, 0))})
            elif f["frame"] == "close":
                break
    except (Malformed, ValueError) as e:
        error = f"malformed record stream after {len(frames)} frames: {e}"

    if error is None:
        error = compare(case["frames"], frames)
    with open(result_path, "w") as out:
        json.dump({"pass": error is None, "error": error, "frames": frames}, out, indent=2)
    if error:
        sys.stderr.write(f"fake-recorder: {case.get('description', '')}\n{error}\n")
    if frames and frames[-1]["frame"] == "close":
        say({"t": "closed"})
    sys.exit(1 if error else 0)


if __name__ == "__main__":
    main()
