"""Types shared by the recorder and the host: the handler-facing API."""

from __future__ import annotations

import platform
import traceback
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Any, Callable, Mapping, Protocol

__version__ = "0.1.0"

#: Recorded in the journal header (`producer`) and sent in `ready.sdk`.
PRODUCER = f"kavach-python/{__version__}"

_EPOCH = datetime(1970, 1, 1, tzinfo=timezone.utc)

REMOTE = "remote"
LOCAL = "local"


@dataclass(frozen=True)
class Input:
    """One event consumed by a handler."""

    source: str  # where the event came from, e.g. "kafka:wallet-events"
    position: str  # its position in that source, e.g. "3:1042"
    data: bytes  # the event exactly as received


@dataclass(frozen=True)
class Output:
    """One effect a handler requested with `env.emit`."""

    sink: str
    data: bytes
    local: bool = False


@dataclass(frozen=True)
class Invariant:
    """A named property of handler state that must hold after every step.

    `check()` holds when it returns normally (or returns anything but
    ``False``); it is violated when it raises or returns ``False``.
    """

    name: str
    check: Callable[[], Any]


@dataclass(frozen=True)
class Gateway:
    """A registered gateway: the connection that makes the query, and its scope.

    `connection(request: bytes) -> bytes` is called when recording. Raise any
    exception to report a failure; its text is recorded as the gateway's error.
    """

    connection: Callable[[bytes], bytes]
    scope: str = REMOTE


class KavachError(Exception):
    """Base class of errors raised by the SDK itself."""


class _Message(Exception):
    def __init__(self, message: str):
        super().__init__(message)
        self.message = message


class Panic(_Message):
    """Fails the step as a ``panic`` whose marker message is exactly `message`."""


class HandlerError(_Message):
    """Fails the step as an ``error`` whose marker message is exactly `message`."""


class GatewayError(Exception):
    """A gateway query failed. `error` is the text the connection reported."""

    def __init__(self, error: str):
        super().__init__(error)
        self.error = error


class UnknownGatewayError(KavachError, LookupError):
    """The handler queried a gateway that was not registered."""


class RecorderError(KavachError):
    """The recorder could not be started and recording was required."""


class Handler(Protocol):
    """What a handler implements. Optional methods are looked up by name:

    * ``snapshot() -> bytes`` and ``restore(data: bytes) -> None``
    * ``invariants() -> list[Invariant]``
    """

    def handle(self, env: "Env", input: Input) -> None: ...


class Env:
    """Passed to a handler for each input. Everything nondeterministic a
    handler does must go through it."""

    def now_ns(self) -> int:
        """Nanoseconds since the Unix epoch, UTC."""
        raise NotImplementedError

    def now(self) -> datetime:
        """The clock as an aware UTC datetime (microsecond precision)."""
        ns = self.now_ns()
        return _EPOCH + timedelta(microseconds=ns // 1000)

    def random(self, n: int) -> bytes:
        """`n` random bytes."""
        raise NotImplementedError

    def query(self, gateway: str, request: bytes) -> bytes:
        """Query a registered gateway; raises `GatewayError` if it failed."""
        raise NotImplementedError

    def config(self, key: str) -> bytes | None:
        """Read a config value that can change what the handler does."""
        raise NotImplementedError

    def emit(self, sink: str, data: bytes, local: bool = False) -> None:
        """Request an effect. Delivered only after the step succeeds."""
        raise NotImplementedError


@dataclass(frozen=True)
class Failure:
    kind: str  # "panic", "error" or "invariant"
    message: str
    detail: str = ""


def classify(exc: BaseException) -> Failure:
    """The failure mapping of SPEC §4.5, used both live and in the host.

    * `Panic(msg)`      -> panic, message exactly ``msg``
    * `HandlerError(m)` -> error, message exactly ``m``
    * anything else     -> panic, message ``"<TypeName>: <str(exc)>"``;
      the traceback is the detail.
    """
    detail = "".join(traceback.format_exception(type(exc), exc, exc.__traceback__))
    if isinstance(exc, Panic):
        return Failure("panic", exc.message, detail)
    if isinstance(exc, HandlerError):
        return Failure("error", exc.message, detail)
    return Failure("panic", f"{type(exc).__name__}: {exc}", detail)


def invariant_list(handler: Any) -> list[Invariant]:
    fn = getattr(handler, "invariants", None)
    return list(fn()) if callable(fn) else []


def check_invariants(handler: Any) -> Failure | None:
    """The first violated invariant, in declaration order, or None."""
    for inv in invariant_list(handler):
        try:
            res = inv.check()
        except Exception as e:
            return Failure("invariant", inv.name, str(e) or type(e).__name__)
        if res is False:
            return Failure("invariant", inv.name, "check returned False")
    return None


def resolve_gateway(gateways: Mapping[str, Any] | None, name: str) -> Gateway:
    """Look `name` up in a registry of `Gateway`, ``(connection, scope)`` or a
    bare connection callable (remote)."""
    try:
        g = gateways[name] if gateways is not None else None
    except KeyError:
        g = None
    if g is None:
        raise UnknownGatewayError(f"kavach: gateway {name!r} is not registered")
    if isinstance(g, Gateway):
        gw = g
    elif isinstance(g, tuple):
        gw = Gateway(*g)
    else:
        gw = Gateway(g)
    if gw.scope not in (REMOTE, LOCAL):
        raise ValueError(f"kavach: gateway {name!r} has scope {gw.scope!r}, want 'remote' or 'local'")
    return gw


def call_gateway(gw: Gateway, request: bytes) -> tuple[bytes, str]:
    """Run a connection; return (response, error). Exactly one is meaningful."""
    try:
        resp = gw.connection(request)
    except GatewayError as e:
        return b"", e.error
    except Exception as e:
        return b"", str(e) or type(e).__name__
    if isinstance(resp, str):
        resp = resp.encode()
    return bytes(resp), ""


def to_bytes(v: bytes | bytearray | memoryview | str) -> bytes:
    return v.encode() if isinstance(v, str) else bytes(v)


def runtime() -> str:
    """The `host.runtime` fact, e.g. ``cpython-3.13.1``."""
    return f"{platform.python_implementation().lower()}-{platform.python_version()}"
