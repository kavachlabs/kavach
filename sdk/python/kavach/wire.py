"""Encoding of the record stream (SPEC §10.2) and its record payloads (§4)."""

from __future__ import annotations

import struct

# Frame kinds (§10.2).
OPEN = 0x01
RECORD = 0x02
STEP_END = 0x03
FACTS = 0x04
SNAPSHOT = 0x05
FLUSH = 0x06
CLOSE = 0x07

# Record types (§4).
INPUT = 0x01
CLOCK = 0x02
RAND = 0x03
OUTPUT = 0x04
MARKER = 0x05
GATEWAY = 0x07
CONFIG = 0x09

CRITICAL = 0x01

SCOPE_REMOTE = 0
SCOPE_LOCAL = 1


def uvarint(n: int) -> bytes:
    if n < 0:
        raise ValueError("uvarint of a negative number")
    out = bytearray()
    while n >= 0x80:
        out.append((n & 0x7F) | 0x80)
        n >>= 7
    out.append(n)
    return bytes(out)


def bytes_field(b: bytes) -> bytes:
    return uvarint(len(b)) + b


def string_field(s: str) -> bytes:
    return bytes_field(s.encode("utf-8"))


def scope_byte(local: bool) -> bytes:
    return bytes([SCOPE_LOCAL if local else SCOPE_REMOTE])


def frame(kind: int, payload: bytes = b"") -> bytes:
    return uvarint(1 + len(payload)) + bytes([kind]) + payload


def record_frame(rtype: int, payload: bytes, critical: bool = False) -> bytes:
    return frame(RECORD, bytes([rtype, CRITICAL if critical else 0]) + payload)


def input_record(source: str, position: str, data: bytes) -> bytes:
    return record_frame(INPUT, string_field(source) + string_field(position) + bytes_field(data))


def clock_record(unix_nanos: int) -> bytes:
    return record_frame(CLOCK, struct.pack("<q", unix_nanos))


def rand_record(data: bytes) -> bytes:
    return record_frame(RAND, bytes_field(data))


def output_record(sink: str, data: bytes, local: bool) -> bytes:
    return record_frame(OUTPUT, string_field(sink) + bytes_field(data) + scope_byte(local))


def marker_record(kind: str, message: str, data: bytes = b"") -> bytes:
    return record_frame(MARKER, string_field(kind) + string_field(message) + bytes_field(data))


def gateway_record(gateway: str, request: bytes, response: bytes, error: str, local: bool) -> bytes:
    return record_frame(
        GATEWAY,
        string_field(gateway) + bytes_field(request) + bytes_field(response) + string_field(error) + scope_byte(local),
        critical=True,
    )


def config_record(key: str, value: bytes | None, source: str) -> bytes:
    present = value is not None
    return record_frame(
        CONFIG,
        string_field(key) + bytes([1 if present else 0]) + bytes_field(value or b"") + string_field(source),
        critical=True,
    )


def step_end_frame() -> bytes:
    return frame(STEP_END)


def snapshot_frame(data: bytes) -> bytes:
    return frame(SNAPSHOT, bytes_field(data))


def flush_frame(durable: bool) -> bytes:
    return frame(FLUSH, bytes([1 if durable else 0]))


def close_frame() -> bytes:
    return frame(CLOSE)


def facts_frame(facts: dict[str, bytes]) -> bytes:
    """A `facts` frame: an environment payload whose facts all have form 0 (value)."""
    body = bytearray(uvarint(len(facts)))
    for key, value in facts.items():
        body += string_field(key) + b"\x00" + bytes_field(value)
    return frame(FACTS, bytes(body))
