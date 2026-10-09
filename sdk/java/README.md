# Kavach for Java

Java SDK for [Kavach](../../README.md): it records what a handler did before it
failed (SPEC §3.6, §10), and lets the `kavach` CLI replay that recording against
your code to check that a fix really fixes it (SPEC §9). Java 21 or later, JDK
only, no dependencies. Package `com.kavachlabs.kavach`.

You also need `kavach-recorder` (found through `$KAVACH_RECORDER`, else on
`PATH`) to write journals. Without it the SDK logs a SEVERE message on the
`kavach` logger and runs unrecorded.

Sandbox replay (SPEC §6.3) is not supported: a host that is told `mode:
"sandbox"` answers `fatal` with `sandbox mode not supported`.

## Build

```bash
./build.sh          # build/kavach.jar, build/classes and build/examples
./build.sh test     # build, then run every test and conformance suite
./build.sh bench    # step overhead over the pipe and over the ring
./build.sh clean
```

Only `javac` and `jar` are used. `build.sh test` runs, in order:

1. the unit tests (`AllTests`, plain `main`, no JUnit);
2. the seven SDK recorder cases of `spec/recorder/sdk/` through
   `conformance.RecorderCase` and the fake recorder, over the pipe and again
   over the ring;
3. the eighteen host transcripts of `spec/host/` through `run.py` and
   `conformance.ConformanceHost`.

`KAVACH_SPEC_DIR` locates the repository's `spec/` directory (default
the repository's `spec/`). Python 3 is needed for the
fake recorder and `run.py`; the ring tests and `bench` also need `go`, to build
the real `kavach-recorder` from the repository. To run them by hand:

```bash
python3 $KAVACH_SPEC_DIR/host/run.py \
  --host "java -cp build/classes com.kavachlabs.kavach.conformance.ConformanceHost"
java -cp build/classes com.kavachlabs.kavach.conformance.RecorderCase $KAVACH_SPEC_DIR/recorder/sdk/*.json
```

## A handler

```java
import com.kavachlabs.kavach.*;

final class Wallet implements Handler, Snapshotter {
    long balance;

    @Override
    public void handle(Env env, Input input) throws Exception {
        Instant now = env.now();                              // env.nowNanos() for nanoseconds
        byte[] txn = env.random(8);                           // never SecureRandom / Math.random
        byte[] rate = env.query("fx-rates", "EURUSD".getBytes());   // throws GatewayException on failure
        Optional<byte[]> limit = env.config("MAX_TRANSFER");
        env.emit("postgres:balances", "...".getBytes());      // delivered after the step succeeds
    }

    // optional
    @Override public byte[] snapshot() { ... }
    @Override public void restore(byte[] data) { ... }
    @Override public List<Invariant> invariants() {
        return List.of(Invariant.of("non_negative", () -> balance >= 0));
    }
}
```

A handler must reach the world only through `env`: `System.currentTimeMillis`,
`Instant.now`, `Random`, `SecureRandom`, `System.getenv`, network calls and
direct effects are not recorded and make replay nondeterministic. Handlers are
synchronous and called from one thread at a time.

| `Env` method | Records |
| --- | --- |
| `now()`, `nowNanos()` | `clock` |
| `random(n)` | `rand` |
| `query(gateway, request)` | `gateway` (request, response or error, scope) |
| `config(key)` | `config` |
| `emit(sink, data)`, `emit(sink, data, local)` | `output` (scope `local` if `local`) |

Gateways are registered by name with a scope:

```java
Gateways gateways = new Gateways()
    .register("fx-rates", request -> httpGet(request))                        // remote
    .register("local-cache", Gateway.local(request -> cache.get(request)));
```

A connection throws `GatewayException(error)` (or any exception, whose message
is recorded) to report a failure. `Gateways.any(gateway)` serves every name.
A name that is not registered throws `UnknownGatewayException`, which fails the
step as a panic.

An `Invariant(name, check)` holds when `check` returns normally and is violated
when it throws; `Invariant.of(name, supplier)` is violated when the supplier
returns false. Invariants are checked after every step that ended ok; the first
violated one, in declaration order, fails the step.

## Failure mapping (SPEC §4.5)

The same function maps failures when recording and when replaying, so a
recorded failure and its replay compare equal.

| The handler | Marker kind | Message |
| --- | --- | --- |
| throws any other `Exception` or `Error` `e` | `panic` | `e.toString()`, e.g. `java.lang.NullPointerException: ...`; the stack trace is the marker data |
| throws `Panic(msg)` | `panic` | exactly `msg` |
| throws `HandlerError(msg)` | `error` | exactly `msg` |
| ends ok but an invariant fails | `invariant` | the invariant's name |

`Panic` and `HandlerError` are `RuntimeException`s. A `Throwable` thrown in a
recorded step never propagates out of `Recorder.step`: it is recorded and
described by the returned `StepResult` (`throwIfFailed()` rethrows it wrapped).
While replaying, the host unwinds an aborted step (SPEC §9.4) with an internal
`Error` subclass so that `catch (Exception e)` in handler code cannot swallow
it, and reports `aborted` regardless of what the handler does next.

Note `e.toString()` of an `NullPointerException` includes the JVM's helpful
message, which names expressions and local variables; it is stable for a given
build, which is what matters for comparing a recording with its replay.

## Recording

```java
Recorder rec = new Recorder(new Wallet(), new Recorder.Options()
    .service("wallet")
    .gateways(gateways)
    .config(key -> /* byte[] or null */ null)
    .flags(() -> Map.of("flag.ledger-v2", "on".getBytes()))
    .dir("kavach")
    .deliver(outputs -> outputs.forEach(o -> publish(o)))   // after a successful step
    .required(false));                                      // true: fail construction without a recorder

StepResult r = rec.step(new Input("kafka:wallet-events", "3:1042", bytes));
rec.flush(true);   // between steps: close the block and wait until it is durable
rec.close();       // orderly shutdown; also run from a JVM shutdown hook
```

- The recorder is started with `ProcessBuilder`: the argument vector given with
  `recorderCommand(...)`, else `$KAVACH_RECORDER`, else `kavach-recorder` on
  `PATH`. The process environment is passed unchanged (SPEC §10.1).
- The `input` frame is written before the handler is called; the step's other
  frames are written together with `step_end`.
- The control stream is read on a daemon thread. A `fixture` message is logged
  at WARNING (`kavach: wrote fixture ...`) and passed to `onFixture`.
- If the recorder cannot be started, dies or reports a fatal error, the SDK
  logs at SEVERE once, stops recording and the steps carry on unrecorded.
  `required(true)` makes construction throw `RecorderException` instead.
- `flush(true)` blocks until the recorder answers `durable` (answers are
  paired with flushes by counting) or the timeout (10 s) passes; it returns
  false on timeout or when recording has stopped. `close()` waits up to 10 s
  for `closed`.
- A handler that implements `Snapshotter` answers the recorder's snapshot
  requests at the next step boundary, on the step's thread. `snapshots(false)`
  turns segments off.
- On Unix the record stream goes over a shared-memory ring (SPEC §10.7), so a
  step costs memory copies and no system call. The SDK creates the file in
  `/dev/shm` (else `java.io.tmpdir`), owner-only, maps it with
  `FileChannel.map`, and names it to the recorder in `ring_path`, since a JVM
  cannot pass descriptor 3; the recorder unlinks it, and the SDK deletes it
  itself if the recorder never starts. `ringBytes(n)` sets the capacity (power
  of two, at least 64 KiB, default 8 MiB). `noRing(true)` forces the pipe, which
  is also what the SDK falls back to, with a WARNING, when the ring cannot be
  set up. The mapping is released by the garbage collector. The data area is
  mapped once, so a frame that wraps is two copies (mapping it twice needs
  Java 22's FFM).
- Median step overhead on the benchmark handler (`./build.sh bench`, a clock
  read, an 8-byte random read and an emit per step): about 75 ns/event over the
  ring, about 415 over the pipe (JDK 25, macOS).
- The pipe's buffer is not enlarged (no `F_SETPIPE_SZ` from pure Java).

## Replay host

```java
public static void main(String[] args) {
    Kavach.maybeHost(args, Wallet::new,
        HostOptions.defaults().gateways(gateways));   // returns at once unless the last arg is kavach-host
    // ... the normal service
}
```

When the last argument is `kavach-host` the program takes the protocol stream
(`new FileOutputStream(FileDescriptor.out)`), points `System.out` at standard
error, serves the driver and exits the JVM. `ready.environment` is the output
of `kavach-recorder facts` (if the recorder can be found) plus `host.runtime`
(e.g. `java-25.0.1`).

The build you replay is chosen by how you launch it, never by an environment
variable: the driver serves the journal's environment variables to the host.

## Example: the ledger demo

`examples/ledger/` is a port of the Go demo. The bug is the same: an event with
`"amount": null` reaches an unboxing cast and throws `NullPointerException`.
`--fix` rejects it instead.

```bash
./build.sh
export KAVACH_RECORDER=/path/to/kavach-recorder
java -cp build/kavach.jar:build/examples ledger.Main \
     --in examples/ledger/events.jsonl --fixtures /tmp/ledger     # buggy, crashes at evt-008
kavach inspect /tmp/ledger/fixtures/*.kavach                      # ends with the evt-008 input and a panic marker
```

Replay hosts are `java -cp build/kavach.jar:build/examples ledger.Main` (old)
and the same with `--fix` (new).

## Layout

| Path | |
| --- | --- |
| `src/main/java/com/kavachlabs/kavach/` | the SDK: `Handler`, `Env`, `Recorder`, `Host`, `Kavach.maybeHost`, wire encoding, a small `Json` |
| `src/main/java/com/kavachlabs/kavach/conformance/` | the conformance handler (§9.6), host main and recorder-case runner |
| `src/test/java/` | `AllTests` |
| `examples/ledger/` | the demo service |
