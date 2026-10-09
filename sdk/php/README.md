# Kavach for PHP

PHP SDK for [Kavach](../../README.md): it records what a handler did before it
failed (SPEC §3.6, §10), and lets the `kavach` CLI replay that recording against
your code to check that a fix really fixes it (SPEC §9). PHP 8.2 or later; no
runtime or dev dependencies beyond PHP itself (it uses `json`, `mbstring` and
`pcre`, which ship with PHP).

Scope: **long-running CLI workers**, such as queue consumers. Recording a web
request per FPM worker is future work.

## Install

Without Composer:

```php
require '/path/to/sdk/php/src/autoload.php';
```

With Composer, add the directory as a path repository and `require
kavachlabs/kavach`; `composer.json` carries autoloading metadata only (PSR-4
`Kavach\` → `src/`, plus `src/functions.php`).

You also need `kavach-recorder` (found through `$KAVACH_RECORDER`, else on
`PATH`) to write journals, and the `kavach` CLI to replay them. Without the
recorder the SDK logs an error on stderr and runs unrecorded.

## A handler

```php
use Kavach\{Env, Handler, HasInvariants, Input, Invariant, Snapshotter};

final class Wallet implements Handler, Snapshotter, HasInvariants
{
    private int $balance = 0;

    public function handle(Env $env, Input $input): void
    {
        $now   = $env->now();                         // UTC DateTimeImmutable; $env->nowNanos() for the int
        $txn   = bin2hex($env->random(8));            // never random_bytes()
        $rate  = $env->query('fx-rates', 'EURUSD');   // throws Kavach\GatewayError on failure
        $limit = $env->config('MAX_TRANSFER');        // ?string
        $env->emit('postgres:balances', '...');       // delivered after the step succeeds
    }

    // optional: Snapshotter
    public function snapshot(): string { return (string) $this->balance; }
    public function restore(string $data): void { $this->balance = (int) $data; }

    // optional: HasInvariants
    public function invariants(): array
    {
        return [new Invariant('non_negative', fn () => $this->balance >= 0)];
    }
}
```

A handler must reach the world only through `$env`: `time()`, `microtime()`,
`random_bytes()`, `rand()`, `getenv()` for config, network calls and direct
effects are not recorded and make replay nondeterministic. Handlers are
synchronous, and strings are byte strings.

| `Env` method | Records |
| --- | --- |
| `now(): DateTimeImmutable`, `nowNanos(): int` | `clock` |
| `random(int $n): string` | `rand` |
| `query(string $gateway, string $request): string` | `gateway` (request, response or error, scope) |
| `config(string $key): ?string` | `config` |
| `emit(string $sink, string $data, bool $local = false)` | `output` (scope `local` if `$local`) |

`new Invariant($name, $check)` holds when `$check()` returns normally (or
returns anything but `false`) and fails when it throws or returns `false`.
Invariants are checked after every step that ended `ok`; the first violated one,
in declaration order, fails the step.

## Failure mapping (SPEC §4.5)

The same function maps failures when recording and when replaying, so a recorded
failure and its replay compare equal.

| The handler | Marker kind | Message |
| --- | --- | --- |
| throws any other `Throwable $e` | `panic` | `get_class($e) . ': ' . $e->getMessage()`, e.g. `TypeError: Foo::bar(): Argument #1 ($amount) must be of type int, null given`; the trace is the marker data |
| throws `Kavach\Panic($msg)` | `panic` | exactly `$msg` |
| throws `Kavach\HandlerError($msg)` | `error` | exactly `$msg` |
| ends `ok` but an invariant fails | `invariant` | the invariant's name |

Per SPEC §4.5 the message never contains locations: file paths, line numbers and memory addresses the engine adds (`, called in … on line 42`, ` in …:12`, ` on line 7`) are stripped from the message; the data keeps the full original message and the trace. The `TypeError` above reads `TypeError: Foo::bar(): Argument #1 ($amount) must be of type int, null given`.
Warnings and notices are not exceptions in PHP; if you want them to fail a step,
turn them into `ErrorException` with `set_error_handler` in your handler.

While replaying, the host unwinds an aborted step (§9.4) with an internal
`Error` subclass, so `catch (\Exception)` cannot swallow it. PHP cannot make an
exception uncatchable, so the host also tracks the aborted state and reports
`aborted` and sends no further requests even if the handler catches
`\Throwable`.

## Recording

```php
$rec = new Kavach\Recorder(
    new Wallet(),
    service: 'wallet',
    dir: 'kavach',                                   // journals; fixtures go in dir/fixtures/
    gateways: [                                      // name => Gateway | [connection, scope] | connection
        'fx-rates' => fn (string $req): string => $fx->get($req),
        'shm'      => new Kavach\Gateway($shmRead, 'local'),
    ],
    config: fn (string $key) => $flagService->get($key),   // default: getenv()
    flags: fn () => ['flag.ledger-v2' => 'on'],      // flag.* facts for the environment record
    deliver: fn (array $outputs) => array_map($send, $outputs),
    recorderCommand: null,                           // argv; else $KAVACH_RECORDER, else `kavach-recorder` on PATH
    required: false,                                 // true: throw Kavach\RecorderError if it cannot start
);
$result = $rec->step(new Kavach\Input('kafka:wallet-events', '3:1042', $payload));
if (!$result->ok) {                                  // ->kind: panic | error | invariant
    $result->raiseForFailure();                      // or log ->message / ->detail
}
$rec->flush(durable: true);                          // between steps; blocks until durable
$rec->close();                                       // waits for the recorder to finish the journal
```

* The `input` frame is written before the handler runs; the rest of the step is
  written with its `step_end`. Outputs are delivered only after a successful step.
* `step()` does not throw for handler failures; it returns a `StepResult`.
* There are no threads. The recorder's control stream is read without blocking
  at step boundaries, and blocking only inside `flush(durable: true)` and
  `close()`, so a step never waits on it (§10.3). `snapshot_request` is answered
  at the next step boundary.
* `start: 'snapshot'` starts the journal from `$handler->snapshot()`. Segments
  are cut when the handler is a `Snapshotter`; pass `snapshots: false` to turn
  them off.
* If the recorder cannot be started, dies, or reports a fatal error, the SDK
  logs on stderr (or through the `logger: fn ($level, $message)` option), stops
  recording and never fails a step. The recorder's `fixture` messages are logged
  at `warning` and passed to `onFixture`.
* At process exit (`register_shutdown_function`) the recorder is closed in an
  orderly way between steps; if the process ends inside a step, the pipe is just
  dropped so that the recorder marks the step as a `crash` (§10.5).
* The SDK does not enlarge the pipe buffer (PHP has no `F_SETPIPE_SZ`); a full
  pipe makes the step wait, it never drops records.
* All other §10.2 `open` options are named arguments (`compression`, `level`,
  `blockBytes`, `flushMs`, `segmentBytes`, `segmentSeconds`, `retainSegments`,
  `secretKeys`, `handlerId`).
* `host.runtime` is `php-` plus `PHP_VERSION`.

## Replay host

```php
require __DIR__ . '/vendor/autoload.php';   // or src/autoload.php

Kavach\maybeHost($argv, fn () => new Wallet(), ['gateways' => [/* ... */]]);
// ... normal startup
```

Call `maybeHost` first. When the last argument is `kavach-host` it serves the
CLI over stdin/stdout and exits; otherwise it returns. It writes the protocol on
a handle on file descriptor 1 opened first, turns `display_errors` to stderr and
routes everything the handler `echo`es or `print`s to stderr through an output
buffer, so printing is safe. (A handler that writes to `STDOUT` or
`php://stdout` itself would corrupt the stream; don't.) `ready.environment` is
the output of `kavach-recorder facts` if it can be found, plus `host.runtime`.

Sandbox mode (§6.3) is not supported: a `hello` with `mode: "sandbox"` is
answered with `fatal` ("sandbox mode not supported").

Choose old and new builds with a command-line flag or by running different
code, **never** an environment variable: `env.` facts are served from the
journal during replay.

## Example

`examples/ledger/` is the Go ledger demo with its `"amount": null` bug:

```bash
export KAVACH_RECORDER=/path/to/kavach-recorder
php examples/ledger/main.php                 # buggy: TypeError on evt-008, writes a fixture
php examples/ledger/main.php --fix           # fixed: rejects it
php examples/ledger/main.php --in FILE --fixtures DIR
```

The build is chosen by the `--fix` flag; the replay hosts are
`php examples/ledger/main.php` (old) and `... --fix` (new). The default input is
`examples/ledger/testdata/events.jsonl` of the repository.

## Tests and conformance

```bash
sdk/php/bin/test          # unit tests, the recorder cases, the host transcripts
```

The contract lives in `spec/`; point at it with `KAVACH_SPEC_DIR` (default: the
repository's `spec/`). `python3` is needed for the
spec's own runner and fake recorder.

```bash
export KAVACH_SPEC_DIR=/path/to/kavach/spec

# host transcripts (SPEC §9.6): expect 18/18
python3 $KAVACH_SPEC_DIR/host/run.py --host "php $PWD/sdk/php/bin/conformance-host"

# SDK recorder cases through the fake recorder (SPEC §10.6): expect 7/7
php sdk/php/bin/recorder-case                  # or: bin/recorder-case path/to/case.json
```

`bin/conformance-host` is the conformance handler (§9.6, `Kavach\Conformance\Handler`)
behind `maybeHost`; `bin/recorder-case` records the same handler with
`python3 fake_recorder.py` as the recorder command.
