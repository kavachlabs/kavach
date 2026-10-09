"""The conformance handler of SPEC §9.6, exactly as specified."""

from __future__ import annotations

import json
import os

from kavach import Env, Gateway, GatewayError, HandlerError, Input, Invariant, Panic


_ESC = {'"': '\\"', "\\": "\\\\", "\n": "\\n", "\r": "\\r", "\t": "\\t"}


def _str(v: str) -> str:
    # §9.6: escape only " \ \n \r \t and other chars below U+0020 as \u00xx
    # (lowercase hex); everything else is raw UTF-8.
    return '"' + "".join(_ESC.get(c) or (f"\\u{ord(c):04x}" if c < " " else c) for c in v) + '"'


def _compact(obj: dict) -> bytes:
    parts = [_str(k) + ":" + ("true" if v is True else _str(v)) for k, v in obj.items()]
    return ("{" + ",".join(parts) + "}").encode("utf-8")


def _refuse(request: bytes) -> bytes:
    raise GatewayError("conformance host has no live connections")


class AnyGateway(dict):
    """A gateway registry in which every name is a remote gateway."""

    def __init__(self, connection=_refuse):
        super().__init__()
        self._connection = connection

    def __missing__(self, name):
        return Gateway(self._connection, "remote")


class Conformance:
    """State is a count; each non-failing step adds 1 (plus any `count` ops)."""

    def __init__(self):
        self.count = 0

    def handle(self, env: Env, input: Input) -> None:
        for op in json.loads(input.data):
            kind = op["op"]
            if kind == "clock":
                env.emit("trace", _compact({"clock": str(env.now_ns())}))
            elif kind == "rand":
                env.emit("trace", env.random(op["n"]))
            elif kind == "gateway":
                try:
                    resp = env.query(op["gateway"], op["request"].encode("utf-8"))
                except GatewayError as e:
                    env.emit("trace", _compact({"error": e.error}))
                else:
                    env.emit("trace", resp)
            elif kind == "config":
                value = env.config(op["key"])
                env.emit("trace", _compact({"unset": True}) if value is None else value)
            elif kind == "getenv":
                value = os.environb.get(op["name"].encode("utf-8"))
                env.emit("trace", _compact({"unset": True}) if value is None else value)
            elif kind == "emit":
                env.emit(op["sink"], op["data"].encode("utf-8"))
            elif kind == "panic":
                raise Panic(op["message"])
            elif kind == "error":
                raise HandlerError(op["message"])
            elif kind == "print":
                print(op["text"])
            elif kind == "count":
                self.count += op["n"]  # takes effect immediately, no rollback
            else:
                raise HandlerError(f"unknown operation {kind!r}")
        self.count += 1

    def snapshot(self) -> bytes:
        return str(self.count).encode("ascii")

    def restore(self, data: bytes) -> None:
        self.count = int(data.decode("ascii"))

    def invariants(self) -> list[Invariant]:
        def below_limit():
            if self.count >= 1000:
                raise AssertionError(f"count is {self.count}")

        return [Invariant("below_limit", below_limit)]
