# Kavach for Julia

The Julia SDK for [Kavach](../../README.md): a flight recorder
([SPEC.md §10](../../SPEC.md#10-recorder-protocol)) and a replay host
([§9](../../SPEC.md#9-host-protocol)). Julia 1.10 or later, standard library
only (`Base64`, `Dates`, `Logging`, `Mmap`, `Random`).

```bash
julia --project=sdk/julia -e 'using Pkg; Pkg.test()'
julia --project=sdk/julia sdk/julia/bench/recorder_step.jl   # ns/event, pipe vs ring; needs kavach-recorder
```

## Writing a handler

Define methods on your own type; dispatch does the rest.

```julia
using Kavach

mutable struct Counter; n::Int; end

function Kavach.handle!(h::Counter, env::Env, input::Input)
    h.n += 1
    at = Kavach.now(env)                                   # or now_ns(env)
    emit!(env, "log", "event $(h.n) at $at")
end

Kavach.snapshot(h::Counter) = Vector{UInt8}(codeunits(string(h.n)))      # optional
Kavach.restore!(h::Counter, data) = (h.n = parse(Int, String(copy(data)))) # optional
Kavach.invariants(h::Counter) = Pair{String,Function}["non_negative" => () -> h.n >= 0] # optional
```

Handlers reach the world only through `env`: `now_ns(env)`, `Kavach.now(env)`,
`Kavach.random(env, n)`, `query(env, gateway, request)` (throws `GatewayError`),
`config(env, key)` (`Vector{UInt8}` or `nothing`) and
`emit!(env, sink, data; islocal=false)`. (`local` is a reserved word in Julia,
so the keyword is `islocal`.) Never use `time`, `rand` or direct effects.

An invariant holds when its function returns without throwing and not
`false`; they are checked after every step that ended ok.

### Failure mapping (SPEC §4.5)

| In the handler | Recorded as |
| --- | --- |
| any exception | `panic`; `message` is the first line of `sprint(showerror, e)` with `@ file:line` / `at file:line` fragments and pointer addresses removed (so it is the same in production and in any checkout); `data` is the full text and the stack trace |
| `throw(Kavach.Panic(msg))` | `panic` with exactly `msg` |
| `throw(Kavach.HandlerError(msg))` | `error` with exactly `msg` |
| an invariant fails | `invariant`, `message` the invariant's name |

On replay an abort (§9.4) unwinds the step with a private exception and the
step is reported `aborted` whatever the handler does with it.

## Recording

```julia
rec = Recorder(Counter(0); service="counter", dir="kavach",
               gateways=Dict("fx-rates" => Gateway(req -> fetch_rate(req))),
               deliver=outs -> foreach(deliver_to_world, outs))
result = step!(rec, Input("kafka:topic", "0:42", data))   # StepResult; never throws for a handler failure
flush!(rec; durable=true)
close(rec)
```

The recorder is `kavach-recorder` from the `recorder_command` option (an
argument vector), else `$KAVACH_RECORDER`, else `PATH`. The `input` frame is
written before the handler runs. If the recorder cannot be started, dies or
reports a fatal error, the SDK `@error`s loudly, stops recording and carries on;
`required=true` makes the constructor throw `RecorderError` instead. `fixture`
messages are logged with `@warn` (and passed to `on_fixture`). `host.runtime`
is `julia-<VERSION>`.

On Unix the record stream travels over a shared-memory ring (§10.7): a file
in `/dev/shm` (else `tempdir()`), created owner-only and mapped shared, which
the recorder opens by name (`ring_path`; Julia cannot pass a child descriptor 3)
and unlinks. `ring_bytes` sets its capacity (default 8 MiB); `ring=false`
forces the pipe, which is also the fallback, with a `@warn`, when the ring
cannot be set up. A full ring makes the step wait for the recorder, unless the
recorder has exited, which stops recording.

Known limit: the pipe keeps its default size instead of the 1 MiB `F_SETPIPE_SZ`
buffer §10.2 suggests.

## Replaying: the host

```julia
Kavach.maybe_host(ARGS, () -> Counter(0); gateways=Dict(...))
```

Call it first in `main`. When the last argument is `kavach-host` it serves the
driver and exits; otherwise it returns. It takes the protocol stream for itself
(a `dup` of fd 1, then fd 1 is pointed at stderr, and fd 0 at `/dev/null`) so a
handler that prints cannot corrupt it. Sandbox mode (§6.3) is not supported:
the host answers `hello` with `fatal` `"sandbox mode not supported"`. Unix only.

Choose old and new builds with command-line flags, never environment variables:
the journal's `env.` facts are served to the host on replay.

## Startup latency

The package carries a precompile workload (a whole host session and recorder
run on a probe handler), so after the one-time `Pkg.precompile()` (about 6 s,
at install) a host answers `hello` with `ready` in about 0.25 s from process
start (0.35 s with `kavach-recorder facts`), well inside a driver's 5 s
message timeout. Precompile the environment in your image build; a host
started on a cold, never-precompiled depot would spend those 6 s on its first
`ready`.

## Conformance

| What | How |
| --- | --- |
| Host transcripts (§9.6) | `python3 spec/host/run.py --host "julia --project=sdk/julia sdk/julia/conformance/host.jl"` |
| Recorder cases (§10.6) | `julia --project=sdk/julia sdk/julia/conformance/recorder_case.jl` |

`Pkg.test()` runs both. `KAVACH_SPEC_DIR` points at the spec directory
(default the repository's `spec/`).

## Example

`examples/ledger/` is the Go ledger demo with its `"amount": null` bug:

```bash
export KAVACH_RECORDER=/path/to/kavach-recorder
julia --project=sdk/julia sdk/julia/examples/ledger/ledger.jl --fixtures /tmp/fx   # crashes, writes a fixture
julia --project=sdk/julia sdk/julia/examples/ledger/ledger.jl --fix                 # the fixed build
kavach inspect /tmp/fx/fixtures/*.kavach
```
