# The ledger, without an SDK

This is the wallet ledger from [`../sdk`](../sdk), in Julia, integrated with Kavach as
[INTEGRATING.md](../../../INTEGRATING.md) describes for a language with no SDK.
It uses only Julia's standard library and loads nothing from Kavach. It writes
the recorder protocol's frames when live and answers the host protocol in
replay.

| File | Does |
| --- | --- |
| [handler.jl](handler.jl) | The handler. It touches the world only through `now_ns`, `randbytes` and `emit!`, and maps exceptions to failure markers. |
| [recorder.jl](recorder.jl) | Live: starts `kavach-recorder` and streams `open`, `facts`, `record` and `step_end` frames into it ([SPEC.md §10](../../../SPEC.md#10-recorder-protocol)). |
| [host.jl](host.jl) | Replay: answers the `kavach` CLI in JSON Lines when the last argument is `kavach-host` ([SPEC.md §9](../../../SPEC.md#9-host-protocol)). |
| [ledger.jl](ledger.jl) | The entry point: picks one of the two modes, and folds a file of events. |
| [json.jl](json.jl) | Just enough JSON, since the standard library has none. |

## Run it

From the repository root:

```sh
go build -o /tmp/kavach ./cmd/kavach
go build -o /tmp/kavach-recorder ./cmd/kavach-recorder
cd examples/ledger

# Live: the event with "amount": null throws in the handler, and the recorder
# logs the fixture it wrote.
KAVACH_RECORDER=/tmp/kavach-recorder julia protocol/ledger.jl --in testdata/events.jsonl --fixtures /tmp/journal

/tmp/kavach replay /tmp/journal/fixtures/*.kavach --bin "julia protocol/ledger.jl"   # still_failing@32
/tmp/kavach diff /tmp/journal/fixtures/*.kavach \
  --old "julia protocol/ledger.jl" --new "julia protocol/ledger.jl --fix"            # verdict fixed
```

`--fix` picks the fixed build. It is a flag and not an environment variable
because replay serves environment variables from the journal.

[check.sh](check.sh) runs all of the above and checks the verdicts.

## What it leaves out

The ledger reads no gateways or config, so it sends no `gateway` or `config`
records or requests. It also never answers `snapshot_request`, so its journal
is one file per run, and it does not implement the conformance handler, so it
does not run the suites in [spec/host](../../../spec/host) and
[spec/recorder/sdk](../../../spec/recorder/sdk). A real integration does all
of these.
