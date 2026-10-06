# Kavach

Kavach turns production crashes in journal-driven Go services into deterministic
tests that AI coding agents can reproduce, fix, and *verify* — by re-deriving
state from the journal instead of replaying a tape.

**Scope:** deterministic replay and fix verification for single-writer,
journal-driven Go services.

> **Status: pre-alpha.** The journal format ([SPEC.md](SPEC.md)) is the first
> artifact. The recorder, replayer and CLI are being built in the open; nothing
> here is ready for production use yet. Every performance number this project
> publishes will be linked to a benchmark you can run.

## How it works

1. **Record.** Your service reads time and randomness through injected
   `kavach.Clock` and `kavach.Rand`, and consumes its inputs from a journal. An
   in-process flight recorder keeps recent journal events in memory and flushes
   them to a fixture file when the handler panics, returns an error, or you
   trigger it.
2. **Replay.** `kavach replay <fixture>` folds the recorded inputs through your
   handler, serving every clock and random read from the fixture. Outputs are
   captured as events, never executed, so replay touches no external system.
3. **Verify.** `kavach diff <fixture> --old <bin> --new <bin>` reports the first
   output where two builds diverge. A fix only counts when the original failure
   is gone, declared invariants hold, and mutated variants of the incident
   journal also pass.

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
| [CONTRIBUTING.md](CONTRIBUTING.md) | How to contribute (DCO sign-off required) |

## Open source

Kavach is licensed under [Apache 2.0](LICENSE). The core replayer, the flight
recorder, the CLI and the MCP server are Apache 2.0 and will stay that way.
Any future commercial offering will be built around them, not by moving them.

Contributions are accepted under the [Developer Certificate of Origin](DCO);
there is no CLA. Telemetry, if it is ever added, will be opt-in.
