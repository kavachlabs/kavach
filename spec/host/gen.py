#!/usr/bin/env python3
"""Generates the host-protocol transcripts in this directory (SPEC.md §9.6).

    python3 spec/host/gen.py

The transcripts are checked in; edit the cases here and regenerate, never the
.jsonl files by hand.
"""

import base64
import json
import pathlib

HERE = pathlib.Path(__file__).parent
T0 = "1759752000000000000"

# A gateway error holding every class of character §9.6 treats differently, and
# the exact JSON the conformance handler must emit for it.
ESCAPE_IN = 'a "q" \\ <b>&amp; caf\u00e9\nl\tt\r\u0001'
ESCAPE_OUT = '{"error":"a \\"q\\" \\\\ <b>&amp; caf\u00e9\\nl\\tt\\r\\u0001"}'.encode()


def b64(s):
    return base64.b64encode(s.encode() if isinstance(s, str) else s).decode()


def j(v):
    return json.dumps(v, separators=(",", ":"))


def hello(start="genesis", snapshot=None):
    m = {"t": "hello", "protocol": 1, "service": "conformance", "start": start, "mode": "process"}
    if snapshot is not None:
        m["snapshot"] = b64(snapshot)
    return m


READY = {"t": "ready", "protocol": 1, "invariants": ["below_limit"]}


def step(seq, ops):
    return {"t": "step", "seq": str(seq), "source": "test", "position": str(seq), "data": b64(j(ops))}


def trace(data):
    return {"t": "emit", "sink": "trace", "data": b64(data), "scope": "remote"}


def done(outcome="ok", message=None):
    m = {"t": "done", "outcome": outcome}
    if message is not None:
        m["message"] = message
    return m


def send(m):
    return {"send": m}


def expect(m):
    return {"expect": m}


def opening(**kw):
    return [send(hello(**kw)), expect(READY)]


def closing():
    return [send({"t": "end"}), {"expect_exit": 0}]


CASES = {
    "empty-step": (
        "A step with no operations succeeds.",
        [*opening(), send(step(0, [])), expect(done()), *closing()],
    ),
    "clock": (
        "A clock read is answered and the value emitted as a string.",
        [
            *opening(),
            send(step(0, [{"op": "clock"}])),
            expect({"t": "clock"}),
            send({"t": "clock", "unix_nanos": T0}),
            expect(trace(j({"clock": T0}))),
            expect(done()),
            *closing(),
        ],
    ),
    "rand": (
        "A random read asks for exactly n bytes and gets them back unencoded.",
        [
            *opening(),
            send(step(0, [{"op": "rand", "n": 4}])),
            expect({"t": "rand", "n": 4}),
            send({"t": "rand", "data": b64(b"\x01\x02\x03\xff")}),
            expect(trace(b"\x01\x02\x03\xff")),
            expect(done()),
            *closing(),
        ],
    ),
    "gateway": (
        "A gateway query sends its request and gets a response or an error.",
        [
            *opening(),
            send(step(0, [
                {"op": "gateway", "gateway": "fx-rates", "request": "EURUSD"},
                {"op": "gateway", "gateway": "fx-rates", "request": "GBPUSD"},
            ])),
            expect({"t": "gateway", "gateway": "fx-rates", "request": b64("EURUSD"), "scope": "remote"}),
            send({"t": "gateway", "response": b64("1.07")}),
            expect(trace("1.07")),
            expect({"t": "gateway", "gateway": "fx-rates", "request": b64("GBPUSD"), "scope": "remote"}),
            send({"t": "gateway", "error": "timeout"}),
            expect(trace(j({"error": "timeout"}))),
            expect(done()),
            *closing(),
        ],
    ),
    "config": (
        "A config read gets a value or 'not set'.",
        [
            *opening(),
            send(step(0, [{"op": "config", "key": "flag.beta"}, {"op": "config", "key": "MAX_TRANSFER"}])),
            expect({"t": "config", "key": "flag.beta"}),
            send({"t": "config", "present": True, "value": b64("on")}),
            expect(trace("on")),
            expect({"t": "config", "key": "MAX_TRANSFER"}),
            send({"t": "config", "present": False}),
            expect(trace(j({"unset": True}))),
            expect(done()),
            *closing(),
        ],
    ),
    "getenv": (
        "The host runs with the environment variables it is started with (§9.1).",
        [
            {"env": {"KAVACH_CONF_VAR": "prod-value"}},
            *opening(),
            send(step(0, [{"op": "getenv", "name": "KAVACH_CONF_VAR"}, {"op": "getenv", "name": "KAVACH_CONF_MISSING"}])),
            expect(trace("prod-value")),
            expect(trace(j({"unset": True}))),
            expect(done()),
            *closing(),
        ],
    ),
    "emit": (
        "Outputs are reported in order with their sink and scope.",
        [
            *opening(),
            send(step(0, [{"op": "emit", "sink": "postgres:balances", "data": "alice=10"}, {"op": "emit", "sink": "kafka:out", "data": ""}])),
            expect({"t": "emit", "sink": "postgres:balances", "data": b64("alice=10"), "scope": "remote"}),
            expect({"t": "emit", "sink": "kafka:out", "data": "", "scope": "remote"}),
            expect(done()),
            *closing(),
        ],
    ),
    "mixed-reads": (
        "Reads of every kind are requested in program order.",
        [
            *opening(),
            send(step(0, [
                {"op": "config", "key": "flag.beta"},
                {"op": "clock"},
                {"op": "gateway", "gateway": "accounts", "request": "alice"},
                {"op": "rand", "n": 2},
            ])),
            expect({"t": "config", "key": "flag.beta"}),
            send({"t": "config", "present": True, "value": b64("off")}),
            expect(trace("off")),
            expect({"t": "clock"}),
            send({"t": "clock", "unix_nanos": T0}),
            expect(trace(j({"clock": T0}))),
            expect({"t": "gateway", "gateway": "accounts", "request": b64("alice"), "scope": "remote"}),
            send({"t": "gateway", "response": b64('{"balance":10}')}),
            expect(trace('{"balance":10}')),
            expect({"t": "rand", "n": 2}),
            send({"t": "rand", "data": b64(b"\x00\x00")}),
            expect(trace(b"\x00\x00")),
            expect(done()),
            *closing(),
        ],
    ),
    "panic": (
        "A panic ends the step with its exact message; later operations do not run.",
        [
            *opening(),
            send(step(0, [{"op": "emit", "sink": "trace", "data": "before"}, {"op": "panic", "message": "boom"}, {"op": "emit", "sink": "trace", "data": "after"}])),
            expect(trace("before")),
            expect(done("panic", "boom")),
            *closing(),
        ],
    ),
    "error": (
        "An error ends the step with its exact message.",
        [
            *opening(),
            send(step(0, [{"op": "error", "message": "amount is null"}])),
            expect(done("error", "amount is null")),
            *closing(),
        ],
    ),
    "steps-continue": (
        "Steps after a failed step still run, against the same handler.",
        [
            *opening(),
            send(step(0, [{"op": "panic", "message": "first"}])),
            expect(done("panic", "first")),
            send(step(1, [{"op": "emit", "sink": "trace", "data": "second"}])),
            expect(trace("second")),
            expect(done()),
            *closing(),
        ],
    ),
    "invariant-count": (
        "The invariant fails once the count reaches 1000, after a step that ends ok.",
        [
            *opening(),
            send(step(0, [{"op": "count", "n": 998}])),
            expect(done()),
            send(step(1, [])),
            expect(done("invariant", "below_limit")),
            *closing(),
        ],
    ),
    "failed-step-keeps-count": (
        "A count operation takes effect even when a later operation fails the step.",
        [
            *opening(),
            send(step(0, [{"op": "count", "n": 998}, {"op": "panic", "message": "after counting"}])),
            expect(done("panic", "after counting")),
            send(step(1, [])),
            expect(done()),
            send(step(2, [])),
            expect(done("invariant", "below_limit")),
            *closing(),
        ],
    ),
    "json-escaping": (
        "Emitted JSON escapes only quote, backslash and control characters.",
        [
            *opening(),
            send(step(0, [{"op": "gateway", "gateway": "g", "request": "r"}])),
            expect({"t": "gateway", "gateway": "g", "request": b64("r"), "scope": "remote"}),
            send({"t": "gateway", "error": ESCAPE_IN}),
            expect(trace(ESCAPE_OUT)),
            expect(done()),
            *closing(),
        ],
    ),
    "snapshot-start": (
        "A snapshot start restores the count before the first step.",
        [
            *opening(start="snapshot", snapshot="999"),
            send(step(0, [])),
            expect(done("invariant", "below_limit")),
            *closing(),
        ],
    ),
    "failed-step-no-count": (
        "A failed step adds nothing to the count, so no invariant fails.",
        [
            *opening(start="snapshot", snapshot="999"),
            send(step(0, [{"op": "error", "message": "no"}])),
            expect(done("error", "no")),
            *closing(),
        ],
    ),
    "abort": (
        "After an abort the step ends as aborted and makes no further requests.",
        [
            *opening(),
            send(step(0, [{"op": "clock"}, {"op": "emit", "sink": "trace", "data": "after"}, {"op": "rand", "n": 1}])),
            expect({"t": "clock"}),
            send({"t": "abort", "detail": "handler read clock, journal has rand at seq 3"}),
            expect(done("aborted")),
            *closing(),
        ],
    ),
    "stdout": (
        "A handler that prints cannot corrupt the protocol stream.",
        [
            *opening(),
            send(step(0, [{"op": "print", "text": "hello from the handler"}, {"op": "emit", "sink": "trace", "data": "x"}])),
            expect(trace("x")),
            expect(done()),
            *closing(),
        ],
    ),
}


def main():
    for old in HERE.glob("*.jsonl"):
        old.unlink()
    for name, (description, lines) in CASES.items():
        out = [j({"comment": description})] + [j(l) for l in lines]
        (HERE / f"{name}.jsonl").write_text("\n".join(out) + "\n")
    print(f"wrote {len(CASES)} transcripts")


if __name__ == "__main__":
    main()
