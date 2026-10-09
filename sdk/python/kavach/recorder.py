"""The SDK half of the flight recorder pipe (SPEC §3.6, §10)."""

from __future__ import annotations

import atexit
import json
import logging
import os
import shutil
import subprocess
import sys
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Mapping, Sequence

from . import wire
from .core import (
    PRODUCER,
    Env,
    Failure,
    GatewayError,
    Input,
    LOCAL,
    Output,
    RecorderError,
    call_gateway,
    check_invariants,
    classify,
    resolve_gateway,
    runtime,
    to_bytes,
)

log = logging.getLogger("kavach")

_F_SETPIPE_SZ = 1031  # Linux; fcntl.F_SETPIPE_SZ only exists on newer Pythons
_PIPE_SIZE = 1 << 20
#: How long construction waits for `ready` when recording is not required;
#: after that, recording goes on and the recorder catches up (§10.1).
_READY_WAIT = 2.0


def find_recorder(command: Sequence[str] | None = None) -> list[str] | None:
    """The recorder argument vector: `command`, else $KAVACH_RECORDER, else
    ``kavach-recorder`` on PATH. None if there is none."""
    if command:
        return list(command)
    env = os.environ.get("KAVACH_RECORDER")
    if env:
        return [env]
    found = shutil.which("kavach-recorder")
    return [found] if found else None


@dataclass
class StepResult:
    """What happened in one `Recorder.step`."""

    ok: bool = True
    kind: str = ""  # "", "panic", "error" or "invariant"
    message: str = ""
    detail: str = ""
    exception: BaseException | None = None
    outputs: list[Output] = field(default_factory=list)

    def raise_for_failure(self) -> None:
        """Re-raise the handler's exception, or a `RuntimeError` for an invariant."""
        if self.ok:
            return
        if self.exception is not None:
            raise self.exception
        raise RuntimeError(f"kavach: invariant {self.message} violated: {self.detail}")


class Recorder:
    """Runs a handler step by step and writes everything to `kavach-recorder`.

    Reads are recorded live (real clock, ``os.urandom``, the registered gateway
    connections and the config provider); outputs are delivered through
    `deliver` only after the step succeeded. If the recorder cannot be
    started, dies, or reports a fatal error, the problem is logged on the
    ``kavach`` logger, recording stops and steps carry on unrecorded; with
    ``required=True`` a recorder that cannot be started raises `RecorderError`
    from the constructor instead.
    """

    def __init__(
        self,
        handler: Any,
        *,
        service: str,
        start: str = "genesis",
        snapshots: bool | None = None,
        deliver: Callable[[list[Output]], None] | None = None,
        gateways: Mapping[str, Any] | None = None,
        config: Callable[[str], bytes | str | None] | None = None,
        config_source: str | None = None,
        flags: Callable[[], Mapping[str, bytes | str]] | None = None,
        recorder_command: Sequence[str] | None = None,
        required: bool = False,
        handler_id: str | None = None,
        dir: str | None = None,
        compression: str | None = None,
        level: int | None = None,
        block_bytes: int | None = None,
        flush_ms: int | None = None,
        segment_bytes: int | None = None,
        segment_seconds: int | None = None,
        retain_segments: int | None = None,
        secret_keys: Sequence[str] | None = None,
        on_fixture: Callable[[dict], None] | None = None,
        clock_ns: Callable[[], int] | None = None,
        random_bytes: Callable[[int], bytes] | None = None,
        ready_timeout: float = 10.0,
        close_timeout: float = 10.0,
    ):
        if start not in ("genesis", "snapshot"):
            raise ValueError("start must be 'genesis' or 'snapshot'")
        can_snapshot = callable(getattr(handler, "snapshot", None)) and callable(getattr(handler, "restore", None))
        if start == "snapshot" and not can_snapshot:
            raise ValueError("start='snapshot' needs a handler with snapshot() and restore()")
        self._handler = handler
        self._deliver = deliver
        self._gateways = gateways
        self._config = config if config is not None else (lambda k: os.environ.get(k))
        self._config_source = config_source or ("config" if config is not None else "env")
        self._clock_ns = clock_ns or time.time_ns
        self._random = random_bytes or os.urandom
        self._on_fixture = on_fixture
        self._close_timeout = close_timeout
        self._snapshots = can_snapshot if snapshots is None else snapshots
        if self._snapshots and not can_snapshot:
            raise ValueError("snapshots=True needs a handler with snapshot() and restore()")

        self._step_lock = threading.RLock()
        self._step_thread: int | None = None
        self._cond = threading.Condition()
        self._active = False
        self._failed_logged = False
        self._closing = False
        self._closed = False
        self._closed_ack = threading.Event()
        self._ready = threading.Event()
        self._durable_count = 0
        self._snapshot_requested = threading.Event()
        self._buf: bytearray | None = None
        self._outputs: list[Output] = []
        self._proc: subprocess.Popen | None = None
        self._thread: threading.Thread | None = None
        self.file: str | None = None  # first segment's path, once ready
        self.run: str | None = None

        open_obj: dict[str, Any] = {
            "protocol": 1,
            "service": service,
            "start": start,
            "producer": PRODUCER,
            "snapshots": bool(self._snapshots),
        }
        for k, v in (
            ("handler", handler_id), ("dir", dir), ("compression", compression), ("level", level),
            ("block_bytes", block_bytes), ("flush_ms", flush_ms), ("segment_bytes", segment_bytes),
            ("segment_seconds", segment_seconds), ("retain_segments", retain_segments),
            ("secret_keys", list(secret_keys) if secret_keys else None),
        ):
            if v is not None:
                open_obj[k] = v

        cmd = find_recorder(recorder_command)
        try:
            if cmd is None:
                raise RecorderError("kavach-recorder not found (set recorder_command, $KAVACH_RECORDER or put it on PATH)")
            self._spawn(cmd)
            self._write(wire.frame(wire.OPEN, json.dumps(open_obj, separators=(",", ":")).encode()))
            facts: dict[str, bytes] = {"host.runtime": runtime().encode()}
            if flags is not None:
                for k, v in flags().items():
                    facts[k] = to_bytes(v)
            self._write(wire.facts_frame(facts))
            if start == "snapshot":
                self._write(wire.snapshot_frame(to_bytes(handler.snapshot())))
            if required:
                if not self._ready.wait(ready_timeout) or not self._active:
                    raise RecorderError("kavach-recorder did not become ready")
            else:
                # Waiting here, not in a step, keeps a slow-starting recorder
                # from finding a full ring or pipe at its first read.
                self._ready.wait(_READY_WAIT)
        except Exception as e:
            if required:
                self._kill()
                if isinstance(e, RecorderError):
                    raise
                raise RecorderError(f"kavach-recorder could not be started: {e}") from e
            self._fail(f"could not start the recorder: {e}")
        atexit.register(self.close)

    # -- process and control stream -------------------------------------

    def _spawn(self, cmd: list[str]) -> None:
        self._proc = subprocess.Popen(
            cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=None, bufsize=0,
        )  # environment unchanged (§10.1)
        self._active = True
        if sys.platform.startswith("linux"):
            try:
                import fcntl

                fcntl.fcntl(self._proc.stdin.fileno(), _F_SETPIPE_SZ, _PIPE_SIZE)
            except (OSError, ImportError):
                pass
        self._thread = threading.Thread(target=self._control_loop, name="kavach-control", daemon=True)
        self._thread.start()

    def _kill(self) -> None:
        self._active = False
        p = self._proc
        if p is not None and p.poll() is None:
            p.kill()
        if p is not None:
            try:
                p.wait(timeout=2)
            except subprocess.TimeoutExpired:
                pass
            for stream in (p.stdin, p.stdout):
                try:
                    stream.close()
                except OSError:
                    pass

    def _fail(self, reason: str) -> None:
        """Stop recording, loudly, once. Never raises."""
        was = self._active
        self._active = False
        self._ready.set()
        if not self._failed_logged and (was or not self._closing):
            self._failed_logged = True
            log.error("kavach: %s; recording has stopped and steps run unrecorded", reason)
        with self._cond:
            self._cond.notify_all()

    def _control_loop(self) -> None:
        proc = self._proc
        try:
            for raw in iter(proc.stdout.readline, b""):
                try:
                    msg = json.loads(raw)
                    t = msg["t"]
                except (ValueError, KeyError, TypeError):
                    log.warning("kavach: unreadable message from the recorder: %r", raw[:200])
                    continue
                self._on_control(t, msg)
        except Exception as e:
            log.warning("kavach: control stream failed: %s", e)
        if not self._closing:
            self._fail("the recorder exited unexpectedly")
        self._closed_ack.set()
        self._ready.set()
        with self._cond:
            self._cond.notify_all()

    def _on_control(self, t: str, msg: dict) -> None:
        if t == "ready":
            self.file, self.run = msg.get("file"), msg.get("run")
            self._ready.set()
        elif t == "snapshot_request":
            self._snapshot_requested.set()
        elif t == "durable":
            with self._cond:
                self._durable_count += 1
                self._cond.notify_all()
        elif t == "segment":
            log.debug("kavach: new journal segment %s", msg.get("file"))
        elif t == "fixture":
            log.warning(
                "kavach: wrote fixture %s (input seq %s, %s)", msg.get("file"), msg.get("seq"), msg.get("failure"),
            )
            if self._on_fixture is not None:
                try:
                    self._on_fixture(msg)
                except Exception:
                    log.exception("kavach: on_fixture callback failed")
        elif t == "error":
            fatal = bool(msg.get("fatal"))
            log.error("kavach: recorder %s: %s", "fatal error" if fatal else "error", msg.get("message"))
            if fatal:
                self._fail(f"the recorder reported a fatal error: {msg.get('message')}")
        elif t == "closed":
            self._closed_ack.set()
        else:
            log.debug("kavach: ignoring recorder message %r", t)

    # -- writing --------------------------------------------------------

    def _write(self, data: bytes | bytearray) -> None:
        if not self._active or not data:
            return
        fd = self._proc.stdin.fileno()
        try:
            view = memoryview(bytes(data))
            while view:
                n = os.write(fd, view)
                view = view[n:]
        except OSError as e:
            self._fail(f"could not write to the recorder: {e}")

    def _rec(self, frame_bytes: bytes) -> None:
        if self._buf is not None and self._active:
            self._buf += frame_bytes

    # -- stepping -------------------------------------------------------

    def step(self, inp: Input) -> StepResult:
        """Run the handler on one input. Never raises for a handler failure
        (see `StepResult.raise_for_failure`); a ``KeyboardInterrupt`` or other
        non-`Exception` is recorded as a panic and then re-raised."""
        with self._step_lock:
            if self._closed:
                raise RuntimeError("kavach: step on a closed Recorder")
            self._step_thread = threading.get_ident()
            try:
                return self._step(inp)
            finally:
                self._step_thread = None

    def _step(self, inp: Input) -> StepResult:
        self._answer_snapshot_request()
        self._buf = bytearray()
        self._outputs = []
        # The input goes in before the handler runs, so that a step that kills
        # the process still leaves it on record (§10.2).
        self._write(wire.input_record(inp.source, inp.position, inp.data))
        failure: Failure | None = None
        raised: BaseException | None = None
        try:
            self._handler.handle(_RecordEnv(self), inp)
        except BaseException as e:
            failure, raised = classify(e), e
        if failure is None:
            failure = check_invariants(self._handler)
        if failure is not None:
            self._rec(wire.marker_record(failure.kind, failure.message, failure.detail.encode()))
        self._rec(wire.step_end_frame())
        buf, outputs = self._buf, self._outputs
        self._buf, self._outputs = None, []
        self._write(buf)
        result = StepResult(ok=failure is None, outputs=outputs)
        if failure is not None:
            result.kind, result.message, result.detail = failure.kind, failure.message, failure.detail
            result.exception = raised
        elif self._deliver is not None and outputs:
            self._deliver(outputs)
        if raised is not None and not isinstance(raised, Exception):
            raise raised
        return result

    def _answer_snapshot_request(self) -> None:
        if not self._snapshot_requested.is_set():
            return
        self._snapshot_requested.clear()
        if not self._active or not self._snapshots:
            return
        try:
            data = to_bytes(self._handler.snapshot())
        except Exception:
            log.exception("kavach: snapshot failed; staying in the current segment")
            return
        self._write(wire.snapshot_frame(data))

    # -- flush and close ------------------------------------------------

    def flush(self, durable: bool = False, timeout: float = 10.0) -> bool:
        """Ask the recorder to close its open block now. With ``durable=True``
        also wait (up to `timeout`) until it is on disk. Returns False if
        recording has stopped or the wait timed out. Must be called between
        steps, not from a handler."""
        if self._step_thread == threading.get_ident():
            raise RuntimeError("kavach: flush() must not be called from inside a step")
        with self._step_lock:
            if not self._active:
                return False
            with self._cond:
                before = self._durable_count
            self._write(wire.flush_frame(durable))
        if not durable:
            return self._active
        with self._cond:
            self._cond.wait_for(lambda: self._durable_count > before or not self._active, timeout)
            return self._durable_count > before

    def close(self) -> None:
        """Orderly shutdown: send ``close`` and wait for ``closed``."""
        if self._step_thread == threading.get_ident():
            raise RuntimeError("kavach: close() must not be called from inside a step")
        with self._step_lock:
            if self._closed:
                return
            self._closed = True
            atexit.unregister(self.close)
            if self._proc is None:
                return
            if self._active:
                self._closing = True
                self._write(wire.close_frame())
                if not self._closed_ack.wait(self._close_timeout):
                    log.warning("kavach: the recorder did not answer close within %.0fs", self._close_timeout)
            self._closing = True
            self._active = False
            try:
                self._proc.stdin.close()
            except OSError:
                pass
            try:
                self._proc.wait(timeout=2)
            except subprocess.TimeoutExpired:
                log.warning("kavach: the recorder is still running after close")
            if self._thread is not None:
                self._thread.join(2)
            try:
                self._proc.stdout.close()
            except OSError:
                pass

    def __enter__(self) -> "Recorder":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    @property
    def recording(self) -> bool:
        return self._active


class _RecordEnv(Env):
    """The Env handed to the handler while recording."""

    def __init__(self, rec: Recorder):
        self._r = rec

    def now_ns(self) -> int:
        ns = self._r._clock_ns()
        self._r._rec(wire.clock_record(ns))
        return ns

    def random(self, n: int) -> bytes:
        if n <= 0:
            return b""
        data = self._r._random(n)
        self._r._rec(wire.rand_record(data))
        return data

    def query(self, gateway: str, request: bytes) -> bytes:
        request = to_bytes(request)
        gw = resolve_gateway(self._r._gateways, gateway)
        resp, err = call_gateway(gw, request)
        self._r._rec(wire.gateway_record(gateway, request, resp, err, gw.scope == LOCAL))
        if err:
            raise GatewayError(err)
        return resp

    def config(self, key: str) -> bytes | None:
        v = self._r._config(key)
        v = None if v is None else to_bytes(v)
        self._r._rec(wire.config_record(key, v, self._r._config_source))
        return v

    def emit(self, sink: str, data: bytes, local: bool = False) -> None:
        data = to_bytes(data)
        self._r._outputs.append(Output(sink, data, local))
        self._r._rec(wire.output_record(sink, data, local))
