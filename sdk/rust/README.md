# kavach (Rust SDK)

Flight recorder and replay host for deterministic handlers, written to the
contract in `SPEC.md` (sections 4.5, 9 and 10). No runtime dependencies, sync
API, Unix only. Minimum Rust 1.75. Async (tokio) support is future work.

## Install

```toml
[dependencies]
kavach = { path = "sdk/rust" }   # not yet published
```

## API

- `Handler`: `fn handle(&mut self, env: &mut dyn Env, input: &Input) -> Result<(), Box<dyn Error>>`.
  Optional `Snapshotter` (`snapshot`/`restore`) and `Checker`
  (`invariants() -> Vec<Invariant>`) are exposed by overriding
  `Handler::snapshotter` / `Handler::checker` to return `Some(self)`.
- `Env`: `now()`, `now_nanos()`, `fill_random(&mut [u8])`,
  `query(gateway, request)`, `config(key)`, `emit(sink, data, Scope)`. A handler
  must reach the world only through it.
- `Recorder::builder(service)...build(Box<dyn Handler>)`, then `step(&Input)`,
  `flush(durable, timeout)`, `close()`. Options include `recorder_command(argv)`
  (else `KAVACH_RECORDER`, else `kavach-recorder` on `PATH`), `required(true)`,
  `gateways(Gateways)`, `clock`, `rand`, `config(source, f)`, `flags(f)`,
  `deliver(f)`, `log(f)`, `start_from_snapshot(bytes)`, `recover_panics`,
  `capture_backtraces`.
- `Gateways::new().register(name, Scope, connection)`: name to connection and scope.
- `kavach::maybe_host(factory, HostOptions::new()...)`: call first in `main`. If the
  last argument is `kavach-host` it serves the host protocol and exits. Sandbox
  mode (SPEC.md section 6.3) is not supported: the host answers `fatal`.
  Fd 1 is duplicated for the protocol and then pointed at stderr, so `println!`
  in a handler cannot corrupt it.

See `examples/ledger.rs` (port of the Go ledger demo; `--fixed` selects the fixed
handler, a flag rather than an environment variable because env vars are served
from the journal on replay).

## Failure model

- `Err(e)`: an `error` marker, message `e.to_string()`.
- A panic is caught with `catch_unwind` at the step boundary: a `panic` marker.
  Message is the payload if it is a `&str` or `String`, else `"Box<dyn Any>"`.
  `kavach::panic(msg)` records exactly `msg`. Marker data is a backtrace only if
  `kavach::install_panic_hook()` is active (host mode installs it;
  `RecorderBuilder::capture_backtraces(true)` installs it for recording). It
  chains to the previously installed hook, so default stderr printing is
  unchanged.
- After recording, the recorder keeps unwinding (like the Go SDK re-panics) unless
  `recover_panics(true)`, which returns `StepError::Panic`.
- With `panic = "abort"` nothing unwinds: the process dies, and the recorder
  records a `crash` (SPEC 10.5). The `input` frame is therefore written before the
  handler runs.
- Invariants run after a step that ended ok; a violation is an `invariant` marker.
  Outputs are delivered only after a fully successful step.
- Recorder problems (cannot start, broken pipe, fatal error) are logged and
  recording stops; steps never fail because of them. `required(true)` makes
  `build` fail instead.
- Host mode: on `abort` the step unwinds with a private marker via
  `resume_unwind` (no hook runs) and reports `aborted`.

## Conformance

```bash
cd sdk/rust
cargo test --all-features
```

runs `spec/host/run.py` against `kavach-conformance-host` and every
`spec/recorder/sdk/*.json` case through `kavach-conformance-recorder` and the
fake recorder. The spec is read from `$KAVACH_SPEC_DIR` (default: the
repository's `spec/` directory). python3 is required.
