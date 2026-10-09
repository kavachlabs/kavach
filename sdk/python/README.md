# Kavach for Python

Python SDK for [Kavach](../../README.md): it records what a handler did before
it failed (SPEC §3.6, §10), and lets the `kavach` CLI replay that recording
against your code to check that a fix really fixes it (SPEC §9). Python 3.10 or
later, standard library only.

## Install

```bash
pip install ./sdk/python        # or: pip install -e ./sdk/python
```

You also need `kavach-recorder` (found through `$KAVACH_RECORDER`, else on
`PATH`) to write journals, and the `kavach` CLI to replay them. Without the
recorder the SDK logs an error and runs unrecorded.

## A handler

```python
import kavach
from kavach import Input, Invariant

class Wallet:
    def __init__(self):
        self.balance = 0

    def handle(self, env: kavach.Env, input: Input) -> None:
        now = env.now()                              # aware UTC datetime; env.now_ns() for nanoseconds
        txn = env.random(8)                          # never os.urandom / random
        rate = env.query("fx-rates", b"EURUSD")      # raises kavach.GatewayError on failure
        limit = env.config("MAX_TRANSFER")           # bytes | None
        env.emit("postgres:balances", b"...")        # delivered after the step succeeds

    # optional
    def snapshot(self) -> bytes: ...
    def restore(self, data: bytes) -> None: ...
    def invariants(self) -> list[Invariant]:
        return [Invariant("non_negative", lambda: None if self.balance >= 0 else _raise())]
```

A handler must reach the world only through `env`: `time.time`, `random`,
`os.urandom`, network calls and direct effects are not recorded and make replay
nondeterministic. Handlers are **synchronous** and called from one thread at a
time. (asyncio handlers are future work.)

| `Env` method | Records |
| --- | --- |
| `now() -> datetime`, `now_ns() -> int` | `clock` |
| `random(n) -> bytes` | `rand` |
| `query(gateway, request) -> bytes` | `gateway` (request, response or error, scope) |
| `config(key) -> bytes \| None` | `config` |
| `emit(sink, data, local=False)` | `output` (scope `local` if `local=True`) |

An `Invariant(name, check)` holds when `check()` returns normally and fails when
it raises or returns `False`. Invariants are checked after every step that ended
`ok`; the first violated one, in declaration order, fails the step.

## Failure mapping (SPEC §4.5)

The same function maps failures when recording and when replaying, so a recorded
failure and its replay compare equal.

| The handler | Marker kind | Message |
| --- | --- | --- |
| raises any other exception `e` | `panic` | `f"{type(e).__name__}: {e}"`, e.g. `KeyError: 'amount'`; the traceback is the marker data |
| raises `kavach.Panic(msg)` | `panic` | exactly `msg` |
| raises `kavach.HandlerError(msg)` | `error` | exactly `msg` |
| ends `ok` but an invariant fails | `invariant` | the invariant's name |

`Panic` and `HandlerError` are ordinary `Exception` subclasses. A
`KeyboardInterrupt`/`SystemExit` in a step is recorded as a panic and then
re-raised. While replaying, the host unwinds an aborted step with a
`BaseException` subclass, so `except Exception` in handler code cannot swallow it.

## Recording

```python
rec = kavach.Recorder(
    Wallet(),
    service="wallet",
    dir="kavach",                                     # journals; fixtures go in dir/fixtures/
    gateways={"fx-rates": (fx_connection, "remote"),  # name -> (connection(request) -> bytes, scope)
              "shm": kavach.Gateway(shm_read, "local")},
    config=lambda key: flag_service.get(key),         # default: os.environ.get
    flags=lambda: {"flag.ledger-v2": "on"},           # flag.* facts for the environment record
    deliver=lambda outputs: [send(o.sink, o.data) for o in outputs],
    recorder_command=None,                            # else $KAVACH_RECORDER, else `kavach-recorder` on PATH
    required=False,                                   # True: raise RecorderError if it cannot start
)
result = rec.step(kavach.Input("kafka:wallet-events", "3:1042", payload))
if not result.ok:                                     # result.kind: panic | error | invariant
    result.raise_for_failure()                        # or log result.message / result.detail
rec.flush(durable=True)                               # between steps; blocks until durable
rec.close()                                           # waits for the recorder to finish the journal
```

* The `input` frame is written before the handler runs; the rest of the step is
  written with its `step_end`. Outputs are delivered only after a successful step.
* `step()` does not raise for handler failures; it returns a `StepResult`.
* `start="snapshot"` starts the journal from `handler.snapshot()`. The recorder
  can cut segments (`snapshot_request`) when the handler has `snapshot()` and
  `restore()`; it is answered at the next step boundary.
* If the recorder cannot be started, dies, or reports a fatal error, the SDK logs
  on the `kavach` logger at ERROR, stops recording and never fails a step. The
  recorder's `fixture` messages are logged at WARNING and passed to `on_fixture`.
* On Linux the pipe is enlarged to 1 MiB (`F_SETPIPE_SZ`).
* All other §10.2 `open` options are keyword arguments (`compression`, `level`,
  `block_bytes`, `flush_ms`, `segment_bytes`, `segment_seconds`,
  `retain_segments`, `secret_keys`).

## Replay host

```python
def main():
    kavach.maybe_host(lambda: Wallet(), gateways={...})
    ...  # normal startup
```

Call `maybe_host` first. When the last command-line argument is `kavach-host`
it serves the CLI over stdin/stdout and exits; otherwise it returns. It takes the
protocol stream for itself and points file descriptor 1 and `sys.stdout` at
standard error, so `print` in a handler is safe. `ready.environment` is
`host.runtime` plus the output of `kavach-recorder facts` if that command exists.
Sandbox mode (§6.3) is not supported: the host answers a sandbox `hello`, or a
`live` gateway answer, with `fatal`.

Choose old and new builds with a command-line flag or by running different
code, **never** an environment variable: `env.` facts are served from the
journal during replay.

## Example

`examples/ledger/` is the Go ledger demo with its `"amount": null` bug:

```bash
cd examples/ledger
PYTHONPATH=../.. python -m ledger          # buggy: TypeError on evt-008
PYTHONPATH=../.. python -m ledger --fix    # fixed: rejects it
```

The replay hosts are `python -m ledger` (old) and `python -m ledger --fix`
(new). End-to-end replay through the Go CLI is not wired up yet.

## Tests and conformance

```bash
cd sdk/python
python3 -m unittest                                 # unit tests + both conformance suites
```

The contract lives in the repository's `spec/`; point elsewhere with
`KAVACH_SPEC_DIR`:

```bash
export KAVACH_SPEC_DIR=/path/to/kavach/spec

# host transcripts (SPEC §9.6): expect 16/16
PYTHONPATH=$PWD python3 $KAVACH_SPEC_DIR/host/run.py \
    --host "python3 -m kavach.conformance" --pass-env PYTHONPATH

# one SDK recorder case through the fake recorder (SPEC §10.6)
python3 -m kavach.conformance.recorder_case $KAVACH_SPEC_DIR/recorder/sdk/reads.json
```
