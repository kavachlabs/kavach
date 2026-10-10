# The Recorder
> cmd/kavach-recorder, internal/recorder, internal/envfacts — the process that turns frames into journals and fixtures

## Why the recorder is a separate process

Chapter 2 ended with the SDK handing **frames** to something else. That something is `kavach-recorder`, a separate executable that the service, through its SDK or its own protocol code, starts as its child. Everything slow or risky about recording lives there: assigning sequence numbers, grouping records into blocks, zstd compression, `fsync`, rotating files into segments, cutting fixtures, and collecting the environment.

There are three reasons for the split, and all three come from the specification:

- **The step must not wait on the disk.** The SDK's only job inside a step is to encode records and copy them out. Compression and `fsync` happen in another process, on other cores.
- **Records survive the crash.** Once the SDK has written a frame into the pipe or published it into the ring (Chapter 4), the bytes are outside the service's address space. If the service is killed by a segmentation fault or the OOM killer, the recorder still reads what was handed over, sees the stream end, and finishes the journal with a `crash` marker.
- **It is written once.** Whatever runs inside the service, one of twelve SDKs or a program speaking the protocol by hand, implements only the cheap half: encode a frame, write it. The journal writer, the segment logic and the fixture cutter exist only in Go, in this one program, so every language gets identical journals.

@spec-only 10
@spec 3.6

The pipe in the diagram is the reference picture. Over the ring of Chapter 4 the arrow from SDK to recorder becomes shared memory, and standard input carries only wake-up bytes, but nothing in this chapter changes: the recorder reads the same frames either way.

@flow handler -> SDK: encode frame -> ring or pipe -> reader goroutine -> loop: number, buffer step -> journal.Writer -> compressor goroutine -> segment file | The recorder's pipeline. Everything right of the ring or pipe runs in kavach-recorder.

## Code map

@graph files internal/recorder/ internal/recstream/ internal/envfacts/ cmd/kavach-recorder/ | Figure 3.1: The recorder's files and the calls between them.
@graph calls recorder_recorder_loop 1 | Figure 3.2: The main loop and everything it calls directly.
@graph calls recorder_recorder_handlesnapshot 1 | Figure 3.3: Rotation: handleSnapshot and the background seal.
@graphify explain recorder_recorder_writefixture

## Starting the recorder

@spec 10.1

The executable is small. `main` parses two flags, ignores the four signals the specification names, starts a session of its own, and hands everything else to `internal/recorder`. The ring file, if the SDK passed one, is always descriptor 3: `os.NewFile(3, "ring")` is built unconditionally, and only used if the `open` frame asks for a ring.

@code cmd/kavach-recorder/main.go func:run
@code cmd/kavach-recorder/detach_unix.go 7-10

`--test-facts` is the deterministic mode of §10.6: the run identifier, the header's `recorded_at`, and every `env.` and `host.` fact come from a JSON file instead of the machine, and host watching is off. The recorder conformance suite and `examples/ledger/sdk/regen-fixture.sh` both use it, so that a checked-in fixture holds nothing of the machine it was made on.

`Config` is how the library is driven, from `main` or from a test. Tests pass an in-memory pipe as `In` and a fake `Collector`:

@code internal/recorder/recorder.go type:Config
@code internal/recorder/recorder.go type:recorder
@code internal/recorder/recorder.go func:Run

The `recorder` struct is the whole state of the program. Group its fields as you read: the control stream (`ctl`, `ctlOK`), what the `open` frame said (`open`, `run`), the files (`seg`, `standby`, `work`), numbering (`nextSeq`, `lastSeq`), the environment (`env`, `pending`, `watcher`), the step in progress (`inStep`, `step`, `stepMarker`, `snapRequest`), and the flush timer (`timer`, `armed`). Apart from the control-stream mutex and the background seal described later, all of it is touched by one goroutine, the main loop, so none of it needs locks.

## The record stream

@spec 10.2

### Frames in Go

`internal/recstream` is shared by both ends: the recorder decodes with its `Reader`, and the Go SDK encodes with `AppendFrame` and `AppendRecordFrame`.

@code internal/recstream/recstream.go 17-26
@code internal/recstream/recstream.go type:Open

Zero values of the optional fields mean "use the default", which `Defaults` fills in after parsing. `Validate` rejects what the recorder cannot honour: another protocol version, a missing service, an unknown compression, and a `ring` that is not a power of two of at least 64 KiB (the reason for the power of two is in Chapter 4).

@code internal/recstream/recstream.go func:Validate

The SDK encodes a record frame in place, straight into its step buffer. It reserves one byte for the frame length, encodes the payload after it, and only if the frame turned out to be 128 bytes or longer does it shift the payload up to make room for a longer varint. Most records are short, so most frames are encoded with no second copy.

@code internal/recstream/recstream.go func:AppendRecordFrame

### The decoder

The `Reader` wraps its source in a 1 MiB `bufio.Reader`. To avoid one allocation per frame, small frames (up to 16 KiB) are carved out of a 64 KiB **slab**: a byte slice that is handed out piece by piece and replaced with a fresh one when it runs low. The slab is never reused, so a frame's payload stays valid after the next `Next` call. This matters because the recorder keeps the payloads of a step in memory until the step ends.

@code internal/recstream/recstream.go type:Reader
@code internal/recstream/recstream.go func:Next

Note the three outcomes at the end of a stream. `io.EOF` means it ended between frames, which is normal. `io.ErrUnexpectedEOF` means it ended inside a frame: the service died while writing it. The recorder treats both as the end of the service (§10.5) and discards the partial frame. Anything else, such as a zero length or one over `MaxFrame` (64 MiB), is a corrupt stream and a fatal error.

## The main loop

The recorder runs two goroutines that matter. A **reader goroutine** decodes frames and sends them to the **loop**, which does everything else. A channel operation costs more than handling a typical frame, so frames cross in **batches**: the reader sends a batch when it holds 256 frames, when it hits an error, or when its buffer holds no further bytes (`rd.Buffered() == 0`), which means it would have to block for the next frame. Under load, batches are large and the channel cost is spread out; when the service is quiet, a single frame goes across at once and is not held back.

The reader goroutine also switches transport. The first frame always arrives on standard input. If it is an `open` frame with `ring` set, the goroutine maps the ring, starts `ringBell` on standard input (every byte read from it is a doorbell ring, and its end is the end of the service), and replaces its decoder with one that reads from the ring. Chapter 4 covers the ring itself.

The loop has two inputs: a batch of frames, or the **flush timer**. After every frame, if the open block holds records and the timer is not already running, it is started for `flush_ms`. When it fires, the open block is written. That is how §3.6's rule "the oldest record waits at most a flush interval" is implemented, approximately: the timer starts at the first frame that leaves records in an empty block.

This is the main loop in full:

@code internal/recorder/recorder.go func:loop

Then `handle` dispatches one frame. Read it as a state check per kind: `facts`, `snapshot`, `flush` and `close` are only legal between steps, and `step_end` only inside one. A frame in the wrong place is a fatal protocol error.

@code internal/recorder/recorder.go func:handle

:::trap Common trap
The loop must not block on anything slow. Everything it waits on stalls the reader goroutine behind it, and behind that the SDK: once the 8 MiB ring (or the pipe) is full, the service's next step waits. That is why compression runs on the journal writer's goroutine and segment sealing on another (both below). Anything new you add to the loop, such as a network call or a large file read, has the same effect.
:::

## The control stream

@spec 10.3

`send` writes one JSON line under a mutex, because the background seal can also report a warning. If standard output fails (the SDK closed its end), recording continues: the journal is the product, the control stream is a courtesy.

@code internal/recorder/recorder.go func:send
@code internal/recorder/recorder.go func:warn

The `open` frame produces `ready`. The recorder fills in defaults, invents a **run** identifier (UTC time plus four random bytes, unique per process start), creates `<dir>/fixtures`, and reports the path the first segment will have. It does not create that file yet; that waits for the first record (§10.4).

@code internal/recorder/recorder.go func:handleOpen
@code internal/recorder/recorder.go func:newRun

On the SDK side (Chapter 2), `control` reads these lines on a goroutine of its own. `ready` releases `NewRecorder`, which waits up to 2 s for it before the first step (Chapter 4 explains why). `durable` is counted and paired with flushes by order. `snapshot_request` sets an atomic flag that the next `Step` checks. `fixture` is logged. A fatal `error`, or the recorder exiting, stops recording. No step ever waits on the control stream, except a step that panicked, which waits up to 5 s for its own failure to be durable before re-panicking.

## From frames to journal blocks

A `record` frame holds a record without its `seq`: `type`, `flags`, payload. The recorder's job is to number it and append it to the open block. It does so without decoding the record into a `journal.Record`:

1. `journal.ValidatePayload` runs the same parser as `ParsePayload` in a *dry* mode that checks every field without building the record. A malformed payload is caught here, before it can reach a journal.
2. The payload is kept as the SDK encoded it. At the end of the step, `Writer.WriteEncoded` (Chapter 1) adds `type`, `flags` and the assigned `seq` in front of it and appends it to the block, again without a decode and re-encode.

Only markers are decoded, because the recorder needs their kind to know whether the step failed.

@code journal/record.go func:ValidatePayload
@code internal/recorder/recorder.go func:handleRecord

Notice what `handleRecord` enforces. Inside a step, a second `input` or anything after the step's marker is an error. Between steps, only an `input` (which opens a step) or a `trigger` or `dropped` marker may appear. An SDK may never send `environment` or `snapshot` as a record, because the recorder writes those. And `gateway` and `config` records get the critical flag whatever the SDK sent, because the recorder is the writer of record and Chapter 1's rules require it.

### A step is buffered until it ends

Records of a step are not written as they arrive. `addStepRecord` appends them to `r.step`, and only `endStep` numbers and writes them. This is what lets a fatal error "finish the journal up to the last complete step" (§10.2): the in-progress step is simply dropped. It also means the records of a step whose handler hangs sit in the recorder's memory, not in a block, until the step ends or the stream does.

@code internal/recorder/recorder.go type:stepRecord
@code internal/recorder/recorder.go func:addStepRecord
@code internal/recorder/recorder.go func:writeEncoded
@code internal/recorder/recorder.go func:write

`endStep` is the step boundary. It writes the step, then does the three things that may only happen between steps: cut a fixture if the step failed, write an environment change record if facts changed, and ask for a new segment if the current one is full.

@code internal/recorder/recorder.go func:endStep

### Compression off the loop

Every segment's `journal.Writer` is created with `Async: true`. When a block fills, `flushBlock` hands the raw bytes to the writer's own goroutine, which compresses and writes them, and the loop carries on filling a fresh buffer. The job channel holds one block, so the loop waits only if two blocks are already ahead of the compressor. `Flush` waits for every handed-over block, which is what makes `sync` below mean what it says. The code is in Chapter 1 (`Writer.run`, `flushBlock`, `Flush`).

## The environment

§4.8 (reproduced in Chapter 1) defines the `environment` record: `env.` facts for every environment variable, secrets hashed; `flag.` facts from the SDK; `host.` facts about the machine. §10.4 says who supplies which and when they are written.

@spec 10.4

### Collecting facts

`internal/envfacts` gathers facts. The recorder inherits the service's environment unchanged (the SDK must not filter it), so `os.Environ()` in the recorder is the service's environment. Any variable whose name contains one of the secret words, or which the `open` frame lists in `secret_keys`, is stored as its SHA-256.

@code internal/envfacts/envfacts.go func:IsSecret
@code internal/envfacts/envfacts.go func:Env
@code internal/envfacts/envfacts.go type:Collector
@code internal/envfacts/envfacts.go func:Facts

`Collector.Host` (in `host.go`) collects the `host.` facts: OS, architecture, kernel, host name, user, CPUs and memory limit after cgroup limits, time zone, locale, soft ulimits, and on Linux every mount, a list of sysctls, the systemd-logind settings `RemoveIPC` and `KillUserProcesses`, login sessions, and the container image. Collection is best effort: a fact that cannot be read is left out. Everything goes through the `Collector`'s `Root`, `Run` and `Environ`, so the parsers are tested against captured `/proc` files on any platform. `host.runtime` is not collected here: the recorder is a Go program, and the service may be Python, so the SDK sends it in a `facts` frame.

### The genesis environment and change records

The recorder writes the header and the genesis environment when the journal starts: at the first `input`, or at the first `snapshot` for a journal that starts from one. Before then, `facts` frames only accumulate in `pending`.

@code internal/recorder/recorder.go func:collect
@code internal/recorder/recorder.go func:mergePending
@code internal/recorder/recorder.go func:fullEnvironment

`mergePending` is the one place where the environment changes. It folds pending facts (from the SDK, and from the host watcher) into `r.env` and returns only those that really differ, so a watcher that reports the same value twice writes nothing. `endStep` writes the result as a change record, after the step and before the next `input`, as §4.8 requires.

The **watcher** polls `host.` facts on its own goroutine, every 5 s by default (`--watch-interval`), and remembers what changed. `Take` only swaps a map under a mutex, so a step boundary never waits for a poll. `env.` facts are not watched: a process's environment changes only if the process changes it, which a handler must not do.

@code internal/envfacts/watcher.go func:Poll
@code internal/envfacts/watcher.go func:Take

:::note Collection runs on the loop once
`startJournal` calls `collect` on the loop, at the first `input`, after `ready` has already been sent. On Linux with systemd this can run `busctl` and `loginctl` (each with a 3 s timeout); on macOS it runs `uname` and `sysctl`. On a laptop it takes about 10 ms (`time kavach-recorder facts`). The SDK keeps publishing meanwhile, so a slow collection is absorbed by the ring, not by the step, unless it lasts long enough to fill it.
:::

## Segments

A **segment** is one journal file. Each is complete on its own: it has a header with the same `run` and the next `segment` index, starts with a `snapshot` and the full environment, and continues the run's sequence numbers. Segments are named `<dir>/<service>-<run>-<segment>.kavach`, the index as six digits.

@code internal/recorder/segment.go type:segment
@code internal/recorder/segment.go func:newSegment

`countWriter` counts the compressed bytes written to the file. The compressor goroutine adds to it while the loop reads it, hence the atomic. It is what the size limit is checked against.

### Starting the journal

@code internal/recorder/recorder.go func:startJournal
@code internal/recorder/recorder.go func:beginSegment

Segment 0 is created on the loop. If the handler is a `Snapshotter` (the `open` frame said `snapshots: true`), `startJournal` immediately starts the first background job: preparing segment 1 as a **standby**.

### Hot and standby segments

Rotating used to be done on the loop: close the old segment (flush the last block, wait for the compressor, `fsync`, close), create the next file, prune old ones. The close alone costs about 10 ms, and the loop does not read frames while it runs. PR #6 moved all of it off the loop. At any time there are up to three files:

- the **hot** segment, `r.seg`, being written;
- a **standby** segment, `r.standby`: the next file, already created under the name `<segment>.kavach.standby` with its header written and, on Linux, space reserved with `fallocate` (up to 64 MiB), so the first writes into it do not wait on the file system's allocator;
- the **sealing** segment: the previous hot one, being closed by a goroutine.

The `.standby` suffix keeps a half-prepared file invisible: `prune` matches only names ending in `.kavach`, and a reader looking for segments never sees it.

@code internal/recorder/segment.go func:activate
@code internal/recorder/recorder.go type:sealResult
@code internal/recorder/recorder.go func:seal
@code internal/recorder/recorder.go func:reap

`seal` starts the background job and `reap` collects its result: the next standby, or an error. At most one job runs at a time; `r.work` is its channel. `reap(false)` only looks; `reap(true)` waits.

### Asking for a snapshot

Rotation needs the handler's state, and only the SDK has it, on the step's own thread. So the recorder asks, with `snapshot_request`, and keeps writing to the hot segment until the answer comes:

@code internal/recorder/recorder.go func:maybeRequestSnapshot

The size check is cheap and runs after every step. The age check reads the clock, so it runs only every 256th step.

:::trap Common trap
The age limit is checked every 256 *steps*, not every so many seconds. A service that handles a few events an hour can stay in one segment far longer than `segment_seconds`. And the size is of compressed bytes already written, so a segment overshoots its limit by up to the block still open. Neither is wrong by the specification ("a size or age limit"), but do not rely on segments being cut on time.
:::

### Rotating

The SDK answers at its next step boundary with a `snapshot` frame. `handleSnapshot` does the swap:

@code internal/recorder/recorder.go func:handleSnapshot

Step by step:

1. `reap(true)`: take the standby, waiting if the job that prepares it is still running (the `TODO` in the code: two rotations in quick succession make the loop wait for the first).
2. `activate` renames the standby to its real name. The rename is the only file system call in the swap.
3. `mergePending` brings the environment up to date, so the new segment starts from the environment as it stands.
4. `seal(old, next)` starts the background job: close the old segment, prune, and create the standby after `next`.
5. `beginSegment` writes the `snapshot` record and the full environment into the new segment, and the recorder announces it with `segment`.

@flow step_end -> snapshot_request -> SDK: snapshot frame -> activate standby -> seal old in background -> next standby ready | One rotation. Only the rename and the first two records happen on the loop.

### Durable flushes wait for the seal

A `flush` frame with `1` asks the recorder to make everything so far durable and answer `durable`. Since rotation, "everything so far" can include records in the segment being sealed: its last block might still be in the compressor, not yet synced. So a durable flush first waits for the background job. A non-durable flush does not care and does not wait.

@code internal/recorder/recorder.go func:flush
@code internal/recorder/segment.go func:sync
@code internal/recorder/segment.go func:close

`settle` is the variant used when the recorder ends: wait for the job, then delete the standby, which is not part of the journal.

@code internal/recorder/recorder.go func:settle

### Retention

`prune` deletes the oldest segments of this service beyond `retain_segments`, counting the hot one. It opens each candidate and checks the header's service name, because one service's name can be a prefix of another's. It never touches `fixtures/`, which the recorder never deletes.

@code internal/recorder/segment.go func:prune

## Fixtures

When a step ends with a `panic`, `error`, `invariant` or `crash` marker, `endStep` (above) does three things in order. It `sync`s the segment, so the failure is on disk before anything else. It cuts the fixture. And it reports the fixture with a `fixture` control message, whose `failure` is the marker kind and message (`"panic: runtime error: …"`), or just `"crash"`.

The cut itself reads the hot segment back from disk and copies records into a new journal from the segment's first record (its snapshot, or the genesis environment) through the last record of the failing step:

@code internal/recorder/segment.go func:writeFixture

Points to notice:

- **`seq` is kept.** Records are copied with their production numbers, as §3.6 requires, so `still_failing@32` in a replay refers to the same input as record 32 in production.
- **The header says where it came from.** `cut_from` holds the run, the segment index and the first and last `seq` copied.
- **It appears atomically.** The fixture is written to a hidden temporary file in `fixtures/`, synced, and renamed into place. A reader never sees half a fixture.
- **Its name is the failing input's `seq`**: `<service>-<run>-<seq>.kavach`.
- A failure to write the fixture is a warning, not a fatal error: the journal itself is intact, and the failure is in it.

:::trap Common trap
`writeFixture` runs on the loop and re-reads the whole hot segment, up to `segment_bytes` (256 MiB by default), decoding and re-encoding every record, after an `fsync`. That is fine for a crash, which happens once. But every step that *returns an error* also counts as a failure. A service that returns errors for a stream of bad inputs cuts a fixture per input, each one rereading the segment, and the loop's stall fills the ring and slows the service. Keep this in mind before making more marker kinds count as failures, or when a service treats ordinary rejections as errors.
:::

## Ending

@spec 10.5

There are three ways to end, plus fatal errors. `close` (in `handle` above) calls `finish` and then answers `closed`. The other two are the stream ending without `close`:

@code internal/recorder/recorder.go func:eof
@code internal/recorder/recorder.go func:finish

If the stream ends **inside a step**, the service died while handling an input. The recorder adds the `crash` marker (unless the step already has a marker), and `endStep(true)` writes the step, syncs it, and cuts the fixture. `final` tells `endStep` to skip the environment and segment work, since nothing follows.

If it ends **between steps**, the service stopped without closing the recorder, for example a Go program whose handler panicked: the SDK flushed the failure, re-panicked, and the process exited. The recorder appends an `exit` marker, which is not a failure.

Run the ledger example and look at both files to see this:

```
$ go run ./cmd/kavach inspect /tmp/kv-out/ledger-*.kavach | tail -3
    32  input     file:events.jsonl @ 8  {"id":"evt-008","type":"deposit","account":"bob","amount":null}
    33  marker    panic  runtime error: invalid memory address or nil pointer dereference  ...
    34  marker    exit  process ended without closing the recorder
$ go run ./cmd/kavach inspect /tmp/kv-out/fixtures/*.kavach | tail -1
    33  marker    panic  runtime error: invalid memory address or nil pointer dereference  ...
```

The segment goes on past the failure to the `exit` marker; the fixture stops at the failing step.

## Fatal errors

A **fatal error** is a protocol violation (a frame out of place, an unknown kind, a malformed payload, a bad ring header) or a failure to write. Every such path returns a `fatalError`, and the loop hands it to `fail`:

@code internal/recorder/recorder.go type:fatalError
@code internal/recorder/recorder.go func:fail

`fail` reports the error on the control stream, drops the step in progress (it was never written, so nothing needs undoing), waits for any background seal, closes the hot segment, and exits with status 1. On the SDK side, the fatal `error` message, or the recorder's exit, stops recording: steps carry on unrecorded and the service's log says so (Chapter 2).

## Testing the recorder

Three layers of tests cover this chapter:

- `internal/recorder/recorder_test.go` drives `Run` in process: the genesis environment from a fake collector, the failure being on disk before `close`, the flush interval, host change records, a failing control stream, and rotation sealing in the background (`TestRotationSealsInTheBackground` rotates three times with a durable flush after each, then checks four segments and no `.standby` file left behind).
- `cmd/kavach-recorder` builds the binary and runs it: `smoke_test.go`, the ring's fatal errors and a ring that ends inside a step (`ring_test.go`), and the conformance runner.
- `spec/recorder/journal/` holds the recorder half of the conformance suite of §10.6: record streams with the control messages, journals and fixtures they must produce, compared decoded. Chapter 6 covers it.

:::try Try it
```
go build -o /tmp/kavach-recorder ./cmd/kavach-recorder
go build -o /tmp/ledger ./examples/ledger/sdk
KAVACH_RECORDER=/tmp/kavach-recorder /tmp/ledger -in examples/ledger/testdata/events.jsonl -fixtures /tmp/kv-out
ls /tmp/kv-out /tmp/kv-out/fixtures
go run ./cmd/kavach inspect --json /tmp/kv-out/fixtures/*.kavach | jq -c .meta.cut_from
/tmp/kavach-recorder facts | jq 'with_entries(select(.key|startswith("host.")) | .value |= (.value|@base64d))'
go test ./internal/recorder -run 'Rotation|FailureIsOnDisk' -v
go test ./cmd/kavach-recorder -run 'Smoke|Ring|Conformance'
```
:::
