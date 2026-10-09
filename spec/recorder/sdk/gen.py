#!/usr/bin/env python3
"""Generates the SDK recorder-conformance cases in this directory (SPEC.md §10.6).

    python3 spec/recorder/sdk/gen.py

The cases are checked in; edit them here and regenerate, never the .json files
by hand.
"""

import base64
import json
import pathlib

HERE = pathlib.Path(__file__).parent
T0 = "1759752000000000000"


def b64(s):
    return base64.b64encode(s.encode() if isinstance(s, str) else s).decode()


def j(v):
    return json.dumps(v, separators=(",", ":"))


def open_frame(start="genesis", snapshots=False):
    return {"frame": "open", "open": {"protocol": 1, "service": "conformance", "start": start, "snapshots": snapshots}}


def facts(**flags):
    f = {"host.runtime": "*"}
    f.update({k.replace("_", "."): {"value": b64(v)} for k, v in flags.items()})
    return {"frame": "facts", "facts": f}


def step(seq, ops, **answers):
    return {"step": {"source": "test", "position": str(seq), "data": b64(j(ops)), "ops": ops}, "answers": answers}


def rec(type_, critical=False, **fields):
    return {"frame": "record", "type": type_, "critical": critical, **fields}


def inp(seq, ops):
    return rec("input", source="test", position=str(seq), data=b64(j(ops)))


def out(data, sink="trace"):
    return rec("output", sink=sink, data=b64(data), scope="remote")


def marker(kind, message):
    return rec("marker", kind=kind, message=message, data="*")


END = {"frame": "step_end"}
CLOSE = {"frame": "close"}


def case(description, actions, frames, start="genesis", snapshots=False, snapshot=None, flags=None, control=None):
    c = {
        "description": description,
        "open": {"service": "conformance", "start": start, "snapshots": snapshots},
    }
    if snapshot is not None:
        c["snapshot"] = snapshot
    if flags:
        c["flags"] = flags
    c["actions"] = actions
    if control:
        c["control"] = control
    c["frames"] = frames
    return c


READ_OPS = [
    {"op": "clock"},
    {"op": "rand", "n": 3},
    {"op": "gateway", "gateway": "fx-rates", "request": "EURUSD"},
    {"op": "gateway", "gateway": "fx-rates", "request": "GBPUSD"},
    {"op": "config", "key": "flag.beta"},
    {"op": "config", "key": "MAX_TRANSFER"},
    {"op": "emit", "sink": "postgres:balances", "data": "alice=10"},
]

CASES = {
    "empty-step": case(
        "One step with no operations: open, facts, input, step_end, close.",
        [step(0, [])],
        [open_frame(), facts(), inp(0, []), END, CLOSE],
    ),
    "reads": case(
        "Every kind of read and an output, recorded in program order.",
        [step(0, READ_OPS,
              clock=[T0],
              rand=[b64(b"\x01\x02\xff")],
              gateway=[{"response": b64("1.07")}, {"error": "timeout"}],
              config=[{"value": b64("on")}, {"unset": True}])],
        [
            open_frame(), facts(), inp(0, READ_OPS),
            rec("clock", unix_nanos=T0), out(j({"clock": T0})),
            rec("rand", data=b64(b"\x01\x02\xff")), out(b"\x01\x02\xff"),
            rec("gateway", critical=True, gateway="fx-rates", request=b64("EURUSD"), response=b64("1.07"), error="", scope="remote"),
            out("1.07"),
            rec("gateway", critical=True, gateway="fx-rates", request=b64("GBPUSD"), response="", error="timeout", scope="remote"),
            out(j({"error": "timeout"})),
            rec("config", critical=True, key="flag.beta", present=True, value=b64("on"), source="*"),
            out("on"),
            rec("config", critical=True, key="MAX_TRANSFER", present=False, value="", source="*"),
            out(j({"unset": True})),
            out("alice=10", sink="postgres:balances"),
            END, CLOSE,
        ],
    ),
    "panic": case(
        "A panic is recorded as a marker after the step's outputs; the next step still runs.",
        [
            step(0, [{"op": "emit", "sink": "trace", "data": "before"}, {"op": "panic", "message": "boom"}]),
            step(1, [{"op": "clock"}], clock=[T0]),
        ],
        [
            open_frame(), facts(),
            inp(0, [{"op": "emit", "sink": "trace", "data": "before"}, {"op": "panic", "message": "boom"}]),
            out("before"), marker("panic", "boom"), END,
            inp(1, [{"op": "clock"}]), rec("clock", unix_nanos=T0), out(j({"clock": T0})), END,
            CLOSE,
        ],
    ),
    "error": case(
        "An error is recorded as a marker with its exact message.",
        [step(0, [{"op": "error", "message": "amount is null"}])],
        [open_frame(), facts(), inp(0, [{"op": "error", "message": "amount is null"}]),
         marker("error", "amount is null"), END, CLOSE],
    ),
    "invariant": case(
        "Starting from a snapshot of 999, the first step that ends ok breaks below_limit.",
        [step(0, [])],
        [open_frame(start="snapshot"), facts(), {"frame": "snapshot", "data": b64("999")},
         inp(0, []), marker("invariant", "below_limit"), END, CLOSE],
        start="snapshot", snapshot="999",
    ),
    "flags": case(
        "Flag facts the SDK knows are sent with host.runtime right after open.",
        [step(0, [])],
        [open_frame(), facts(flag_beta="on", flag_dark_mode="off"), inp(0, []), END, CLOSE],
        flags={"flag.beta": "on", "flag.dark.mode": "off"},
    ),
    "segment": case(
        "A snapshot_request is answered with a snapshot at the next step boundary.",
        [step(0, []), {"flush": {"durable": True}}, step(1, [])],
        [
            open_frame(snapshots=True), facts(), inp(0, []), END,
            {"frame": "flush", "durable": True},
            {"frame": "snapshot", "data": b64("1")},
            inp(1, []), END, CLOSE,
        ],
        snapshots=True,
        control=[{"after_step": 0, "send": {"t": "snapshot_request"}}],
    ),
}


def main():
    for old in HERE.glob("*.json"):
        old.unlink()
    for name, c in CASES.items():
        (HERE / f"{name}.json").write_text(json.dumps(c, indent=2) + "\n")
    print(f"wrote {len(CASES)} cases")


if __name__ == "__main__":
    main()
