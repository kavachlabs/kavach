# Kavach

Kavach turns production crashes in journal-driven services, in any language,
into deterministic tests that AI coding agents can reproduce, fix, and *verify* — by re-deriving
state from the journal instead of replaying a tape.

**Scope:** deterministic replay and fix verification for single-writer,
journal-driven services. A service takes part through an SDK, or by speaking
two small protocols directly ([INTEGRATING.md](INTEGRATING.md)).

> **Status: pre-alpha.** The journal format ([SPEC.md](SPEC.md)), the flight
> recorder, replay and the CLI work on the demo service. APIs and the format
> will change before 1.0. Every performance number this project publishes is in
> [BENCHMARKS.md](BENCHMARKS.md) with the command that measured it.

## Try it

The demo is a wallet ledger that crashes when upstream sends `"amount": null`.
The commands below use its Go version; every SDK under [sdk](sdk) has a port
with the same bug. The CLI and the recorder are written in Go, so building them
needs Go; your service does not.

```bash
git clone https://github.com/kavachlabs/kavach && cd kavach
go install ./cmd/kavach ./cmd/kavach-recorder   # the recorder is a separate process the SDK starts
go build -o ledger-old ./examples/ledger/sdk
go build -tags ledgerfix -o ledger-new ./examples/ledger/sdk   # the fixed build
go build -tags ledgerpartialfix -o ledger-partial ./examples/ledger/sdk  # a fix that is too narrow

./ledger-old -in examples/ledger/testdata/events.jsonl     # crashes, writes kavach/fixtures/ledger-*.kavach
kavach inspect kavach/fixtures/ledger-*.kavach                 # what happened, record by record
kavach replay  kavach/fixtures/ledger-*.kavach --bin ./ledger-old # still_failing@32
kavach diff    kavach/fixtures/ledger-*.kavach --old ./ledger-old --new ./ledger-new      # fixed
kavach diff    kavach/fixtures/ledger-*.kavach --old ./ledger-old --new ./ledger-partial  # variant_failed(6)@23
```

The partial fix guards only deposits, the event type in the incident. It
passes the recorded crash, but `diff` also replays 44 variants of it, and the
old build crashes the same way on 41 of them; the partial fix crashes on 4,
starting with the same event as a `transfer`.

Replay runs no external services: the fixture holds every input, clock read and
random read the handler made.

The same demo reads from Kafka through [franz-go](https://github.com/twmb/franz-go).
The topic is the ledger's journal: on every start the service folds it from the
first offset. With a local broker (for example
`docker run -p 9092:9092 apache/kafka:3.9.0`):

```bash
./ledger-old -kafka localhost:9092 -seed examples/ledger/testdata/events.jsonl  # load the topic once
./ledger-old -kafka localhost:9092                                              # crashes at offset 0:7
```

The fixture's inputs carry their Kafka positions (`kafka:wallet-events @ 0:7`),
and it still replays with no broker running. Kafka is a dependency of the demo
only, not of the SDK.

## With an AI agent

`kavach mcp` is a [Model Context Protocol](https://modelcontextprotocol.io)
server with three tools, `kavach_list_incidents`, `kavach_replay` and
`kavach_diff`, returning the same verdicts as the CLI as structured JSON:

```bash
claude mcp add kavach -- kavach mcp
```

[AGENTS.md](AGENTS.md) is the loop an agent should follow.

## Using it in a service

| Language | SDK |
| --- | --- |
| Go | [sdk/go](sdk/go) |
| Python | [sdk/python](sdk/python) |
| TypeScript / JavaScript | [sdk/typescript](sdk/typescript) |
| Rust | [sdk/rust](sdk/rust) |
| Java | [sdk/java](sdk/java) |
| C and C++ | [sdk/c](sdk/c) |
| Julia | [sdk/julia](sdk/julia) |
| Ruby | [sdk/ruby](sdk/ruby) |
| PHP | [sdk/php](sdk/php) |
| Elixir | [sdk/elixir](sdk/elixir) |
| Haskell | [sdk/haskell](sdk/haskell) |
| OCaml | [sdk/ocaml](sdk/ocaml) |

Every SDK has the same shape:

- a **handler**, called once per input, that reads the clock, random bytes,
  gateways and config, and emits its effects, only through the env it is
  given;
- a **recorder** that wraps your consume loop: one step per input, with
  effects delivered after the step succeeds;
- a **host mode**: when the program is started with `kavach-host` as its last
  argument, it serves replays to the CLI instead of consuming.

A handler can also provide snapshots, so that fixtures start from recent state
instead of from the service's first event, and declare invariants that are
checked after every step, live and in replay. Each SDK's README shows the code.

No SDK for your language? An SDK is a wrapper over the recorder and host
protocols in [SPEC.md](SPEC.md) §9 and §10; [INTEGRATING.md](INTEGRATING.md)
shows how to speak them directly.

## How it works

1. **Record.** Your handler reads time and randomness, and emits effects,
   through the env the SDK gives it for each input. The SDK writes
   everything to `kavach-recorder`, a child process that keeps the journal on
   disk and writes a fixture file when the handler panics, returns an error, or
   violates an invariant, even if the service dies in the step.
2. **Replay.** `kavach replay <fixture> --bin <your-service>` runs your own
   build as a host process (`--bin ./ledger`, `--bin "python -m ledger"`,
   `--bin "node dist/main.js"`)
   and folds the recorded inputs through its handler, serving every clock,
   random, gateway and config read from the fixture. Outputs are captured as
   events, never executed, so replay touches no external system.
3. **Verify.** `kavach diff <fixture> --old <bin> --new <bin>` reports the first
   output where two builds diverge. A fix only counts when the original failure
   is gone, declared invariants hold, steps before the failure still produce
   the outputs production saw, and the fix also passes at least 10 **variants**
   of the incident: perturbed copies of the journal (fields of the failing
   event changed, events reordered, dropped or redelivered, the clock shifted)
   on which the old build fails exactly as it did in production. A fix that
   only handles the recorded event does not pass. See [SPEC.md §6.1](SPEC.md#61-verifying-a-fix).

## Not in scope

- Multi-writer or shared-state services. Kavach assumes one writer folds the
  journal into state.
- Record-and-replay of network traffic ("tapes"). Kavach re-derives state from
  inputs; it does not stub dependencies from recorded responses.

## Repository guide

| File | What it is |
| --- | --- |
| [SPEC.md](SPEC.md) | Journal and fixture format, and the host and recorder protocols, v0 |
| [AGENTS.md](AGENTS.md) | How an AI coding agent should use Kavach |
| [INTEGRATING.md](INTEGRATING.md) | Using Kavach from a language with no SDK |
| [sdk](sdk) | The SDKs, one per language |
| [llms.txt](llms.txt) | Index of these docs for LLM tools |
| [BENCHMARKS.md](BENCHMARKS.md) | Every measured number, with its command |
| [examples/ledger](examples/ledger) | The demo service: [sdk](examples/ledger/sdk) with the Go SDK, [protocol](examples/ledger/protocol) in Julia with no SDK |
| [bench](bench) | Ten planted bugs, each with a correct and a too-narrow fix |
| [CONTRIBUTING.md](CONTRIBUTING.md) | How to contribute (DCO sign-off required) |

## Open source

Kavach is licensed under [Apache 2.0](LICENSE). The core replayer, the flight
recorder, the CLI and the MCP server are Apache 2.0 and will stay that way.
Any future commercial offering will be built around them, not by moving them.

Contributions are accepted under the [Developer Certificate of Origin](DCO);
there is no CLA. Telemetry, if it is ever added, will be opt-in.
