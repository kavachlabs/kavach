# Kavach

Kavach turns production crashes in journal-driven Go services into deterministic
tests that AI coding agents can reproduce, fix, and *verify* — by re-deriving
state from the journal instead of replaying a tape.

**Scope:** deterministic replay and fix verification for single-writer,
journal-driven Go services.

> **Status: pre-alpha.** The journal format ([SPEC.md](SPEC.md)), the flight
> recorder, replay and the CLI work on the demo service. APIs and the format
> will change before 1.0. Every performance number this project publishes is in
> [BENCHMARKS.md](BENCHMARKS.md) with the command that measured it.

## Try it

The demo is a wallet ledger that crashes when upstream sends `"amount": null`.

```bash
git clone https://github.com/kavachlabs/kavach && cd kavach
go install ./cmd/kavach ./cmd/kavach-recorder   # the recorder is a separate process the SDK starts
go build -o ledger-old ./examples/ledger
go build -tags ledgerfix -o ledger-new ./examples/ledger   # the fixed build
go build -tags ledgerpartialfix -o ledger-partial ./examples/ledger  # a fix that is too narrow

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
and it still replays with no broker running. The demo is its own Go module, so
Kafka is not a dependency of the Go SDK, which depends only on
`github.com/klauspost/compress`.

## With an AI agent

`kavach mcp` is a [Model Context Protocol](https://modelcontextprotocol.io)
server with three tools, `kavach_list_incidents`, `kavach_replay` and
`kavach_diff`, returning the same verdicts as the CLI as structured JSON:

```bash
claude mcp add kavach -- kavach mcp
```

[AGENTS.md](AGENTS.md) is the loop an agent should follow.

## Using it in a service

The Go SDK is `github.com/kavachlabs/kavach/sdk/go` (package `kavach`); the
other languages are under [sdk](sdk).

```go
func main() {
	// Lets the kavach CLI replay fixtures with this binary.
	kavach.MaybeReplay(func() kavach.Handler { return NewLedger() })

	rec := kavach.NewRecorder(NewLedger(), kavach.Options{Service: "ledger", Deliver: publish})
	defer rec.Close()
	for msg := range consume() {
		rec.Step(kavach.Input{Source: "kafka:wallet", Position: msg.Offset, Data: msg.Value})
	}
}

// Handle reads time and randomness through env and emits effects through it.
func (l *Ledger) Handle(env kavach.Env, in kavach.Input) error { ... }
```

A handler can also implement `kavach.Snapshotter`, so that fixtures start from
recent state instead of from the service's first event, and `kavach.Checker` to
declare invariants that are checked after every step, live and in replay. Use
[`kavachtest.Run`](sdk/go/kavachtest) to run captured fixtures as Go tests.

## How it works

1. **Record.** Your handler reads time and randomness, and emits effects,
   through the `kavach.Env` it is given for each input. The SDK writes
   everything to `kavach-recorder`, a child process that keeps the journal on
   disk and writes a fixture file when the handler panics, returns an error, or
   violates an invariant, even if the service dies in the step.
2. **Replay.** `kavach replay <fixture> --bin <your-service>` runs your own
   build as a host process (any language; `--bin "python -m ledger"` works too)
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
- Languages other than Go, for now. The format is language-neutral by design.

## Repository guide

| File | What it is |
| --- | --- |
| [SPEC.md](SPEC.md) | Journal and fixture format, v0 |
| [AGENTS.md](AGENTS.md) | How an AI coding agent should use Kavach |
| [llms.txt](llms.txt) | Index of these docs for LLM tools |
| [BENCHMARKS.md](BENCHMARKS.md) | Every measured number, with its command |
| [examples/ledger](examples/ledger) | The demo service |
| [bench](bench) | Ten planted bugs, each with a correct and a too-narrow fix |
| [CONTRIBUTING.md](CONTRIBUTING.md) | How to contribute (DCO sign-off required) |

## Open source

Kavach is licensed under [Apache 2.0](LICENSE). The core replayer, the flight
recorder, the CLI and the MCP server are Apache 2.0 and will stay that way.
Any future commercial offering will be built around them, not by moving them.

Contributions are accepted under the [Developer Certificate of Origin](DCO);
there is no CLA. Telemetry, if it is ever added, will be opt-in.
