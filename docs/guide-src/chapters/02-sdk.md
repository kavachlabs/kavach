# The SDK
> sdk/go/ — the thin protocol writer inside the service, where a handler's reads and outputs become frames

## The one rule

Chapter 1 described the records. This chapter is about where they come from: the only Kavach code that runs inside the service. Its job is small: encode a frame for each input, read and output, and hand it to the recorder, a separate process (Chapter 3). The requirement is the recorder protocol (§10), not a library. An SDK is a library that implements it; a service can also write the frames itself, as `INTEGRATING.md` describes and `examples/ledger/protocol` does in Julia with no SDK. The Go SDK is the reference and the one this chapter reads; the other eleven languages follow the same design, and the conformance suites of Chapter 6 hold them to it.

The SDK rests on a single rule, and everything in this chapter follows from it:

> **Everything that could differ between two runs goes through `env`.**

The time, randomness, answers from other systems, config values, and the effects the handler wants to cause. If the handler calls `time.Now()` directly, the SDK never sees it, the journal cannot hold it, and replay reports `nondeterministic@N` when the handler reads a different time. If it calls a database directly, replay will call that database again, against whatever state it is in today.

@flow Handler -> env (Now, Read, Query, Config, Emit) -> SDK records -> recorder | Figure 2.1: Every interaction with the world passes through env, which both performs it and records it.

## Code map

@graph files sdk/go/ | Figure 2.2: Files of the Go SDK and the calls between them. recorder.go records; host.go is the replay side of Chapter 5.
@graph calls go_recorder_step 1 | Figure 2.3: Everything Recorder.Step calls, and what calls it.
@graphify path recordEnv publish

## The contract with the service author

The whole public contract fits in one file. `Input` is an event; `Env` is the handler's only window to the world; `Handler` is the code under test.

@code sdk/go/kavach.go type:Input
@code sdk/go/kavach.go type:Env
@code sdk/go/kavach.go type:Handler

A handler is a **fold**: it takes state and an event and produces new state and effects. Kavach assumes it is deterministic given the `Env`: the same state, input and read values must give the same outputs. That assumption is what makes replay a test rather than a guess.

Two optional interfaces extend it.

**`Snapshotter`** lets the handler save and restore its state. Without it, a journal can only start from *genesis*, and reproducing a crash on day ten means replaying ten days of events. With it, the recorder can start a new segment whenever it likes (Chapter 3), and a fixture starts from the most recent snapshot.

@code sdk/go/kavach.go type:Snapshotter

**`Checker`** declares invariants: named properties that must hold after every step. A broken invariant is a failure even when nothing crashed, which catches the bugs that corrupt state quietly.

@code sdk/go/kavach.go type:Invariant
@code sdk/go/kavach.go type:Checker
@code sdk/go/kavach.go func:CheckInvariants

## A worked example: the ledger

The demo service is a wallet ledger. Its handler decodes an event, reads the clock, and posts entries:

@code examples/ledger/sdk/ledger.go func:Handle

The bug is on the line `amount := *ev.Amount`. When upstream sends `"amount": null`, `ev.Amount` is a nil pointer and the handler panics. Two build tags switch on two fixes. `ledgerfix` rejects a missing amount for every event type. `ledgerpartialfix` is the plausible wrong fix an agent might write after reading the incident: it guards only `deposit`, the type that happened to crash.

@code examples/ledger/sdk/fix_on.go all
@code examples/ledger/sdk/partial_on.go all

The ledger declares two invariants and is a `Snapshotter`:

@code examples/ledger/sdk/ledger.go func:Invariants
@code examples/ledger/sdk/ledger.go 135-136

Its `main` shows both of the SDK's modes in a few lines:

@code examples/ledger/sdk/main.go 26-82

1. `kavach.MaybeReplay` comes first. If the binary was started with `kavach-host` as its last argument, it serves the replay protocol of Chapter 5 and exits; otherwise it returns at once. The same binary records in production and replays under test, so replay always exercises the build you are testing, not a copy of its logic.
2. `kavach.NewRecorder` wraps the handler for production. From then on every event goes through `rec.Step`.

@code sdk/go/entry.go func:MaybeReplay

## Configuring a Recorder

`Options` is where a service plugs in its real clock, randomness, gateways and config, and where it passes settings through to the recorder process. Every field has a working default, so `kavach.Options{Service: "ledger"}` is a complete configuration.

@code sdk/go/recorder.go type:Options

`NewRecorder` fills in defaults, refuses a snapshot-start journal for a handler that cannot snapshot, starts the recorder process, and then waits up to two seconds for the recorder's `ready` message.

@code sdk/go/recorder.go func:NewRecorder

:::note Why wait for ready at all
A freshly started recorder takes around 170 ms to come up on macOS. A service that steps back to back in that time fills the 8 MiB ring before anyone reads it, and its next step blocks for 150–400 ms. Waiting once, at construction, moves that cost to startup, where nobody measures latency. The specification's rule that a step must never wait on the control stream is untouched: this is not a step.
:::

@spec 10.1

## What Step does

`Step` is the heart of the SDK. Read it slowly; each line exists for a reason.

@code sdk/go/recorder.go func:Step

In order:

1. **Answer a pending snapshot request.** If the recorder asked for a new segment, the SDK snapshots the handler now, at a clean boundary between steps, never in the middle of one.
2. **Publish the `input` frame on its own, before the handler runs.** If the handler then kills the process (a segmentation fault, an abort, the out-of-memory killer), the input has already left the process, and the recorder marks the step as a crash. Over the ring this costs no system call, so there is no reason not to.
3. **Run the handler** inside `run`, which turns a panic into a value and a stack.
4. **Classify the outcome.** A panic becomes a `marker` of kind `panic` carrying the stack; a returned error becomes `error`; a failed invariant becomes `invariant`, with the invariant's name as the message.
5. **Publish the rest of the step and `step_end`** in one go: every read, every output and the marker.
6. **Deliver outputs only if the step succeeded.** A failed step never produces half of its side effects.
7. **On a panic, make the fixture durable, then re-panic** with the original value. The service crashes exactly as it would have without Kavach, but its fixture is already on disk.

@code sdk/go/recorder.go func:run
@code sdk/go/recorder.go func:answerSnapshotRequest

### The recording Env

Each `Env` method does the real thing *and* appends a record to the step's buffer. Reads are recorded with the value the handler saw; outputs are recorded and held back for delivery.

@code sdk/go/recorder.go func:add
@code sdk/go/recorder.go 618-646
@code sdk/go/recorder.go func:Config
@code sdk/go/recorder.go func:Emit

Notice that `gateway` and `config` records carry `FlagCritical`, as Chapter 1's rules require. A failed gateway query is recorded with its error, not dropped: replay must serve the same failure, because the handler's reaction to a failing dependency is often where the bug is.

:::trap Common trap
A handler must make its `env` reads from one goroutine at a time, in a deterministic order. Two concurrent `env.Query` calls whose order depends on scheduling are recorded in one order and may be asked in another during replay, which reports `nondeterministic`. The specification lists this as an open question (§11, "Concurrency inside a step").
:::

## Getting records out of the process

The SDK never writes a file. It starts `kavach-recorder` as a child process and streams **frames** to it. A frame is a record, or a control item such as `step_end`, `snapshot` or `flush`. The recorder (Chapter 3) owns everything slow: sequence numbers, blocks, compression, fsync, segments and fixtures. Keeping it in a separate process has two benefits. The handler's step pays only for encoding and one memory copy. And when the service crashes, the records it already handed over survive, because they live in another process.

@spec 3.6

### Starting the recorder

@code sdk/go/recorder.go func:start

The record stream goes over a shared-memory ring when one can be set up, and over the recorder's standard input otherwise. The ring is the subject of Chapter 4; for this chapter, `publish` is a function that takes bytes and returns when the recorder can see them.

@code sdk/go/recorder.go func:newRing
@code sdk/go/recorder.go func:publish

### The control stream

The recorder answers on its standard output with one JSON object per line: `ready`, `snapshot_request`, `durable`, `fixture`, `error` and `closed`. A goroutine reads them; steps never wait on it.

@code sdk/go/recorder.go func:control
@code sdk/go/recorder.go func:onControl

### When the recorder fails

If the recorder cannot start, dies, or reports a fatal error, the SDK logs it, stops recording, and lets steps carry on unrecorded. Observability must never take production down.

@code sdk/go/recorder.go func:fail

## Flushing and closing

`Flush(true)` asks the recorder to make everything recorded so far durable, and waits for its `durable` answer. A service calls it between steps when it is about to acknowledge something upstream, for example before committing a Kafka offset. `Close` shuts the recorder down in order; without it, the recorder sees standard input close and ends the journal with an `exit` marker.

@code sdk/go/recorder.go func:Flush
@code sdk/go/recorder.go func:Close

## The other eleven SDKs

Every SDK implements the same two halves: the recording side of this chapter and the host side of Chapter 5. The protocol between SDK and recorder is the same in every language, so there is one recorder binary for all of them. An SDK is a convenience, not a requirement: a program that writes the same frames by hand gets the same journals, and is held to the same conformance suites (Chapter 6).

| SDK | Transport | Notes |
| --- | --- | --- |
| Go | ring (mapped twice), pipe | reference implementation |
| C / C++ | ring (mapped twice), pipe | C++ wrapper over the C API |
| Rust | ring (mapped twice), pipe | no libc crate; raw system calls |
| Haskell | ring (mapped twice, via C bits), pipe | ring passed by file name |
| Julia | ring (mapped twice), pipe | ring passed by file name |
| Java | ring (split copy), pipe | Java 21 cannot map at a fixed address |
| Python, TypeScript, OCaml, Ruby, PHP, Elixir | pipe | no ring yet; would need a native helper |

## Cost

On the ring, recording costs about 81 ns per event on an Apple M3 Pro (the figure published in `BENCHMARKS.md`). That is encoding the frame plus one memory copy into shared memory, with no system call. Over the pipe it is about 1.5 µs per step at the median, because every publish is a `write` system call. Chapter 4 has the measurements and how they were taken.

:::try Try it
```
go build -o /tmp/ledger-old ./examples/ledger/sdk
/tmp/ledger-old -in examples/ledger/testdata/events.jsonl   # records, crashes, writes a fixture
graphify explain "Step" --graph graphify-out-core/graph.json
graphify path "recordEnv" "publish" --graph graphify-out-core/graph.json
```
:::
