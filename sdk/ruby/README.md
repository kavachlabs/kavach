# Kavach for Ruby

The Ruby SDK for [Kavach](../../SPEC.md): record what a handler did before it
failed (the flight recorder, SPEC section 10), and act as a replay host so the
`kavach` CLI can reproduce the failure and verify a fix (SPEC section 9).

Ruby 3.1 or later. No gem dependencies (JSON and minitest ship with Ruby);
developed and tested on Ruby 4.0.7.

## Install

```ruby
# Gemfile
gem "kavach", path: "path/to/sdk/ruby"
```

or `ruby -I sdk/ruby/lib your_service.rb`. Recording needs the
`kavach-recorder` executable (`$KAVACH_RECORDER`, else on `PATH`).

## API

A handler is any object with `handle(env, input)`. It may also implement
`snapshot` -> String, `restore(data)` (together they make it snapshot-able,
so the recorder may start new segments) and `invariants` -> an array of
`Kavach::Invariant`, checked after every step that did not fail.

```ruby
class Counter
  def initialize = @n = 0

  def handle(env, input)
    at = env.now                                  # Time, UTC
    rate = env.query("fx-rates", "EURUSD")        # raises Kavach::GatewayError on failure
    limit = env.config("MAX_TRANSFER")            # String or nil
    @n += 1
    env.emit("db:counts", "#{@n}@#{at.iso8601}")  # delivered after the step succeeds
  end

  def snapshot = @n.to_s
  def restore(data) = @n = data.to_i

  def invariants
    [Kavach::Invariant.new("non_negative") { raise "negative" if @n.negative? }]
  end
end
```

`Kavach::Input` has `source`, `position` and `data` (a binary String).
`Kavach::Env` offers `now`, `now_ns`, `random(n)`, `query(gateway, request)`,
`config(key)` and `emit(sink, data, local: false)`. A handler must read time,
randomness and the outside world only through `env`, and never perform
effects directly.

Gateways are registered by name with a scope; anything that responds to `[]`
works as the registry:

```ruby
gateways = {
  "fx-rates" => Kavach::Gateway.new(:remote) { |request| http_get(request) },
  "shm"      => Kavach::Gateway.new(:local, ->(request) { read_shm(request) })
}
```

A bare callable is taken as a remote gateway. The connection returns the
response bytes; raising (or `raise Kavach::GatewayError, "timeout"`) records
the error text.

### Recording

```ruby
rec = Kavach::Recorder.new(
  Counter.new, service: "counter", dir: "kavach", gateways: gateways,
  deliver: ->(outputs) { outputs.each { |o| publish(o.sink, o.data) } },
  flags: -> { { "flag.beta" => "on" } },       # optional flag.* facts
  required: false                              # true: fail construction if the recorder cannot start
)
result = rec.step(Kavach::Input.new(source: "kafka:t", position: "0:1", data: bytes))
result.ok?; result.kind; result.message        # "", "panic", "error" or "invariant"
rec.flush(durable: true)                       # blocks until the recorder says durable (10 s timeout)
rec.close                                      # waits for `closed`; also runs at exit
```

Other options: `recorder_command:` (argument vector; else `$KAVACH_RECORDER`,
else `PATH`), `snapshots:`, `start: "snapshot"`, `handler_id:`, `compression:`,
`level:`, `block_bytes:`, `flush_ms:`, `segment_bytes:`, `segment_seconds:`,
`retain_segments:`, `secret_keys:`, `on_fixture:`, `config:` (a callable;
default `ENV`), `clock_ns:`, `random_bytes:`.

The recorder is started with the process's environment unchanged. The `input`
frame is written before the handler runs; the rest of the step is written
with its `step_end`. If the recorder cannot be started, dies or reports a
fatal error, the SDK logs it loudly (`Kavach.logger`, default standard
error), stops recording and never fails the step. Every `fixture` message the
recorder sends is logged too. `host.runtime` is `ruby-<RUBY_VERSION>`.

### Replay host

```ruby
require "kavach"

Kavach.maybe_host(gateways: gateways) { Counter.new }   # first thing in the program

# ... the normal service starts here
```

If the process's last argument is `kavach-host`, `maybe_host` serves the
driver on standard input/output and exits; otherwise it returns at once. It
takes the protocol stream for itself (`STDOUT.dup`), then points `STDOUT` and
`$stdout` at standard error, so `puts` in a handler cannot corrupt it. The
block creates a fresh handler. Choose old/new builds with command-line
flags, never environment variables: the driver serves the recorded
environment variables on replay. `ready.environment` is the output of
`kavach-recorder facts` plus `host.runtime`.

Sandbox mode (SPEC section 6.3) is not supported: a `hello` with
`mode: "sandbox"` is answered with `fatal` "sandbox mode not supported".

## Failure mapping (SPEC section 4.5)

| Raised in `handle` | Marker | Message |
| --- | --- | --- |
| `Kavach::Panic.new(msg)` | `panic` | exactly `msg` |
| `Kavach::HandlerError.new(msg)` | `error` | exactly `msg` |
| any other `StandardError` | `panic` | `"#{e.class}: #{e.message}"`, scrubbed |
| `NoMemoryError`, `NotImplementedError`, `SystemExit`, `SignalException`, ... | `panic` | as above; **re-raised** after the step is recorded (recorder) |
| a violated invariant | `invariant` | the invariant's name |

The marker `data` is the unscrubbed `"Class: message"` plus the backtrace.
The marker `message` is made location independent: object identities
(`#<Foo:0x000...>` becomes `#<Foo>`), other addresses (`0x...` becomes `0x`),
file paths (reduced to base names) and `.rb:LINE` numbers are removed
(`Kavach.scrub`). A recorded failure therefore compares equal to its replay
from another checkout. For example `NoMethodError: undefined method '<=' for
nil`. The same function produces the message when recording and when hosting.
`Panic` and `HandlerError` messages are never altered.

The host's `abort` unwinds the step with an internal `Exception` subclass
that is not a `StandardError`, so a bare `rescue` does not swallow it; if a
handler catches it anyway the step is still reported `aborted`.

## Tests and conformance

```sh
bin/test                                  # unit tests + both conformance suites
```

`bin/test` runs under `/opt/homebrew/opt/ruby/bin/ruby` (or `$RUBY`). It reads
the spec from `$KAVACH_SPEC_DIR` (default the repository's `spec/`)
and needs `python3` for the transcript runner and the fake recorder. It also
exercises the real recorder when `$KAVACH_RECORDER` is set.

```sh
python3 $KAVACH_SPEC_DIR/host/run.py \
  --host "ruby -I$PWD/lib $PWD/bin/conformance-host"      # 18/18 transcripts
bin/recorder-case                                          # 7/7 SDK recorder cases
```

The conformance handler (SPEC section 9.6) is `Kavach::Conformance::Handler`
in `lib/kavach/conformance.rb`. It builds its JSON by hand because
`JSON.generate` does not follow the escaping rule of section 9.6.

## Example: the ledger

`examples/ledger` is a port of the Go ledger demo, including its
`"amount": null` bug.

```sh
cd examples/ledger
KAVACH_RECORDER=/path/to/kavach-recorder ruby -I../../lib ledger.rb --in events.jsonl     # buggy: panics at evt-008, writes a fixture
ruby -I../../lib ledger.rb --fix --in events.jsonl                                         # fixed
```

Replay hosts are `ruby -I../../lib ledger.rb` (old) and the same with `--fix`
(new).
