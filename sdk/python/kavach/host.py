"""The host side of the replay protocol (SPEC §9)."""

from __future__ import annotations

import base64
import json
import os
import subprocess
import sys
from typing import IO, Any, Callable, Mapping

from .core import (
    PRODUCER,
    Env,
    Failure,
    GatewayError,
    Input,
    check_invariants,
    classify,
    invariant_list,
    resolve_gateway,
    runtime,
    to_bytes,
)
from .recorder import find_recorder

PROTOCOL = 1
HOST_ARG = "kavach-host"


class _Abort(BaseException):
    """Unwinds a step after the driver answered a request with `abort` (§9.4).
    A BaseException so that handler code is unlikely to catch it."""


class _Fatal(BaseException):
    """The protocol was violated; the host sends `fatal` and stops."""


class _Gone(BaseException):
    """The driver closed the pipe."""


def _b64(b: bytes) -> str:
    return base64.b64encode(b).decode("ascii")


def _unb64(s: Any) -> bytes:
    if not isinstance(s, str):
        raise _Fatal("expected a base64 string")
    try:
        return base64.b64decode(s, validate=True)
    except ValueError:
        raise _Fatal("invalid base64") from None


def collect_environment(recorder_command: list[str] | None = None) -> dict[str, dict]:
    """`ready.environment`: the output of ``kavach-recorder facts`` if it can be
    run, plus ``host.runtime`` (§9.2)."""
    env: dict[str, dict] = {}
    cmd = find_recorder(recorder_command)
    if cmd:
        try:
            out = subprocess.run(
                cmd + ["facts"], stdin=subprocess.DEVNULL, capture_output=True, timeout=10, check=True,
            ).stdout
            facts = json.loads(out)
            if isinstance(facts, dict):
                env.update({k: v for k, v in facts.items() if isinstance(v, dict)})
        except (OSError, ValueError, subprocess.SubprocessError):
            pass
    env["host.runtime"] = {"value": _b64(runtime().encode())}
    return env


class Host:
    """One host session over a pair of binary streams."""

    def __init__(
        self,
        handler_factory: Callable[[], Any],
        reader: IO[bytes],
        writer: IO[bytes],
        *,
        gateways: Mapping[str, Any] | None = None,
        environment: Callable[[], dict] = collect_environment,
    ):
        self._factory = handler_factory
        self._rd, self._wr = reader, writer
        self._gateways = gateways
        self._environment = environment
        self.aborted = False

    # -- transport ------------------------------------------------------

    def send(self, msg: dict) -> None:
        line = json.dumps(msg, separators=(",", ":"), ensure_ascii=True).encode() + b"\n"
        try:
            self._wr.write(line)
            self._wr.flush()
        except (BrokenPipeError, ValueError):
            raise _Gone() from None

    def recv(self) -> dict:
        try:
            line = self._rd.readline()
        except ValueError:
            raise _Gone() from None
        if not line:
            raise _Gone()
        try:
            msg = json.loads(line)
        except ValueError:
            raise _Fatal("malformed message: not JSON") from None
        if not isinstance(msg, dict) or not isinstance(msg.get("t"), str):
            raise _Fatal("malformed message: no type")
        return msg

    def request(self, msg: dict, want: str) -> dict:
        """Send one request and wait for its answer; unwinds on `abort`."""
        if self.aborted:
            raise _Abort()
        self.send(msg)
        ans = self.recv()
        if ans["t"] == "abort":
            self.aborted = True
            raise _Abort()
        if ans["t"] != want:
            raise _Fatal(f"expected a {want!r} answer, got {ans['t']!r}")
        return ans

    # -- loop -----------------------------------------------------------

    def run(self) -> int:
        """Serve the driver until `end` or EOF. Returns the exit status."""
        handler = None
        try:
            while True:
                try:
                    msg = self.recv()
                except _Gone:
                    return 0  # between steps: the driver left
                t = msg["t"]
                if t == "hello":
                    handler = self._hello(msg)
                elif t == "step":
                    if handler is None:
                        raise _Fatal("step before hello")
                    self._step(handler, msg)
                elif t == "end":
                    return 0
                elif t == "abort":
                    continue  # a stray abort between steps needs no answer
                else:
                    raise _Fatal(f"unexpected message {t!r}")
        except _Fatal as e:
            try:
                self.send({"t": "fatal", "message": str(e)})
            except _Gone:
                pass
            return 1
        except _Gone:
            return 1

    def _hello(self, msg: dict) -> Any:
        if msg.get("protocol") != PROTOCOL:
            raise _Fatal(f"unsupported protocol {msg.get('protocol')!r}")
        if msg.get("mode") == "sandbox":
            raise _Fatal("sandbox mode not supported")
        try:
            handler = self._factory()
        except Exception as e:
            raise _Fatal(f"could not create the handler: {type(e).__name__}: {e}") from None
        if msg.get("start") == "snapshot":
            restore = getattr(handler, "restore", None)
            if not callable(restore):
                raise _Fatal("the journal starts from a snapshot but the handler has no restore()")
            try:
                restore(_unb64(msg.get("snapshot", "")))
            except Exception as e:
                raise _Fatal(f"could not restore the snapshot: {type(e).__name__}: {e}") from None
        self.send({
            "t": "ready",
            "protocol": PROTOCOL,
            "sdk": PRODUCER,
            "invariants": [i.name for i in invariant_list(handler)],
            "environment": self._environment(),
        })
        return handler

    def _step(self, handler: Any, msg: dict) -> None:
        inp = Input(str(msg.get("source", "")), str(msg.get("position", "")), _unb64(msg.get("data", "")))
        self.aborted = False
        failure: Failure | None = None
        try:
            handler.handle(_HostEnv(self), inp)
        except _Abort:
            pass
        except (_Fatal, _Gone, KeyboardInterrupt, SystemExit):
            raise
        except BaseException as e:
            failure = classify(e)
        if self.aborted:
            self.send({"t": "done", "outcome": "aborted"})
            return
        if failure is None:
            failure = check_invariants(handler)
        done: dict[str, Any] = {"t": "done", "outcome": failure.kind if failure else "ok"}
        if failure is not None:
            done["message"] = failure.message
            if failure.detail:
                done["detail"] = failure.detail
        self.send(done)


class _HostEnv(Env):
    def __init__(self, host: Host):
        self._h = host

    def now_ns(self) -> int:
        return int(self._h.request({"t": "clock"}, "clock")["unix_nanos"])

    def random(self, n: int) -> bytes:
        if n <= 0:
            return b""
        return _unb64(self._h.request({"t": "rand", "n": n}, "rand").get("data", ""))

    def query(self, gateway: str, request: bytes) -> bytes:
        request = to_bytes(request)
        gw = resolve_gateway(self._h._gateways, gateway)
        ans = self._h.request(
            {"t": "gateway", "gateway": gateway, "request": _b64(request), "scope": gw.scope}, "gateway",
        )
        if ans.get("live"):
            raise _Fatal("live gateway answers are not supported")
        if ans.get("error"):
            err, resp = str(ans["error"]), b""
        else:
            err, resp = "", _unb64(ans.get("response", ""))
        if err:
            raise GatewayError(err)
        return resp

    def config(self, key: str) -> bytes | None:
        ans = self._h.request({"t": "config", "key": key}, "config")
        if not ans.get("present"):
            return None
        return _unb64(ans.get("value", ""))

    def emit(self, sink: str, data: bytes, local: bool = False) -> None:
        if self._h.aborted:
            raise _Abort()
        data = to_bytes(data)
        self._h.send({"t": "emit", "sink": sink, "data": _b64(data), "scope": "local" if local else "remote"})


def maybe_host(
    handler_factory: Callable[[], Any],
    gateways: Mapping[str, Any] | None = None,
) -> None:
    """If this process was started as a replay host (its last argument is
    ``kavach-host``), serve the driver and exit; otherwise return at once.

    Call it first thing in ``main``, before consuming any input or starting
    any servers. `gateways` maps names to `Gateway` (or ``(connection,
    scope)``).
    """
    if not sys.argv or sys.argv[-1] != HOST_ARG:
        return
    # Take the protocol stream for ourselves, then point fd 1 and sys.stdout at
    # stderr so that a handler that prints cannot corrupt it (§9.1).
    proto_out = os.fdopen(os.dup(1), "wb", buffering=0)
    proto_in = os.fdopen(os.dup(0), "rb")
    sys.stdout.flush()
    os.dup2(2, 1)
    sys.stdout = sys.stderr
    devnull = os.open(os.devnull, os.O_RDONLY)
    os.dup2(devnull, 0)
    os.close(devnull)
    code = Host(handler_factory, proto_in, proto_out, gateways=gateways).run()
    try:
        proto_out.close()
    except OSError:
        pass
    sys.stderr.flush()
    raise SystemExit(code)
