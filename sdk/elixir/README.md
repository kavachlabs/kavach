# Kavach for Elixir

Records what an Elixir handler did before it failed, and lets `kavach replay`
and `kavach diff` re-run it from the fixture. Standard library only (Elixir
>= 1.15, no Hex dependencies). Contract: [SPEC.md](../../SPEC.md) §4, §9, §10.

## A handler

A handler is a behaviour with explicit state, in the shape of a GenServer:

```elixir
defmodule Counter do
  @behaviour Kavach.Handler

  def init, do: 0

  def handle(env, %Kavach.Input{data: data}, n) do
    at = Kavach.Env.now(env)              # clock, randomness, queries and
    id = Kavach.Env.random(env, 8)        # config only through env
    Kavach.Env.emit(env, "log", "#{n} #{at} #{Base.encode16(id)}")
    if data == "bad", do: raise("boom"), else: {:ok, n + 1}
  end

  # optional
  def snapshot(n), do: Integer.to_string(n)
  def restore(bin), do: String.to_integer(bin)
  def invariants(n), do: [{"non_negative", fn -> if n >= 0, do: :ok, else: {:error, "n=#{n}"} end}]
end
```

`Kavach.Env`: `now/1`, `now_ns/1`, `random/2`, `query/3`
(`{:ok, binary} | {:error, string}`), `config/2` (`binary | nil`),
`emit/4`. Never call `System.os_time`, `:rand`, `:crypto` or perform effects
directly in a handler.

### Failure mapping (SPEC.md §4.5)

| Handler does | Marker | message |
| --- | --- | --- |
| returns `{:error, msg}` | `error` | exactly `msg` |
| `raise Kavach.Panic, message: msg` | `panic` | exactly `msg` |
| raises any other exception | `panic` | `"#{inspect(module)}: #{Exception.message(e)}"` |
| `throw v` / `exit r` | `panic` | `"throw: #{inspect(v)}"` / `"exit: #{inspect(r)}"` |

The stacktrace is the marker's data. Messages are normalised so they do not
depend on where the build runs: `#PID<..>`, `#Reference<..>`, `#Port<..>`,
`#Function<..>`, `<0.1.0>`, long `0x` addresses and `file.ex:12` locations are
replaced or removed (`Kavach.Failure`).

Immutable state means a failed step normally leaves the state as it was.
`{:error, msg, new_state}` and `raise Kavach.Panic, message: m, keep: {:state, s}`
carry on with a state anyway, for handlers that do not roll back.

## Recording

```elixir
{:ok, rec} =
  Kavach.Recorder.start_link(
    handler: Counter,
    service: "counter",
    open: %{"dir" => "kavach"},          # extra `open` keys (SPEC.md §10.2)
    # command: ["/path/to/kavach-recorder"],  # default: $KAVACH_RECORDER, else PATH
    # required: true,                         # fail to start rather than run unrecorded
    # clock:, random:, gateway:, config:, flags:, deliver:  (see the module doc)
  )

:ok = Kavach.Recorder.step(rec, %Kavach.Input{source: "queue", position: "0:1", data: "..."})
# or {:failed, :panic | :error | :invariant, message}

Kavach.Recorder.flush(rec, durable: true)
Kavach.Recorder.close(rec)
```

The recorder is a GenServer that owns the `Port` to `kavach-recorder` and
runs the handler. It writes the `input` frame before calling the handler and
the rest of the step with `step_end`. Control messages are handled between
steps; a `snapshot_request` is answered before the next input. If the recorder
cannot start or later fails, it logs with `Logger.error` and stops recording;
the step is never failed. Fixtures the recorder reports are logged as
warnings.

## Replay host

```elixir
def main(argv) do
  Kavach.maybe_host(argv, MyHandler)   # first thing; never returns for `kavach-host`
  ...
end
```

Build an escript (`mix escript.build`) and give `kavach replay --bin` the
path. Protocol frames are written through a raw port to fd 1; the process's
group leader is replaced by a device that writes to stderr, so `IO.puts` in a
handler cannot corrupt the stream. A host starts no recorder and does not
support sandbox mode (§6.3): it answers `hello` with `mode: "sandbox"` with
`fatal`.

## Tests and conformance

```bash
mix test                                   # unit tests + the 7 recorder cases
mix escript.build
python3 ../../spec/host/run.py --host "$PWD/kavach_conformance"   # 18/18
```

`KAVACH_SPEC_DIR` (default `/Users/koustav/code/kavach-labs/kavach/spec`)
points at the shared `spec/` directory. The recorder cases need `python3`.
`Kavach.Conformance.Handler` is the §9.6 handler; `Kavach.Conformance.Host`
is the escript entry point; `Kavach.Conformance.RecorderCases` runs a
`spec/recorder/sdk` case against `fake_recorder.py`.

BEAM startup of the escript is a few hundred milliseconds, well inside the
runner's 5 s per message.

## Demo: `examples/ledger`

A port of the Go ledger. An event with `"amount": null` crashes the buggy
handler (`ArithmeticError`); `--fix` selects the fixed handler (a CLI flag,
never an environment variable).

```bash
cd examples/ledger && mix escript.build
export KAVACH_RECORDER=/path/to/kavach-recorder
./ledger --in ../../../../examples/ledger/testdata/events.jsonl --fixtures /tmp/fx
kavach inspect /tmp/fx/fixtures/*.kavach          # ... evt-008 input, then a panic marker
kavach diff /tmp/fx/fixtures/*.kavach --old ./ledger --new "./ledger --fix"
```
