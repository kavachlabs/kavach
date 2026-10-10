# The Journal Format
> journal/ — the file every other part of Kavach reads or writes

## What a journal is

A **journal** is a recording of everything one handler saw and did: every event it consumed, every time it asked the clock, every random number it drew, every call it made to another system, every config value it read, and every effect it asked for. A **fixture** is a journal, or a piece cut out of one, that ends in a failure, small enough to replay on a laptop.

Every other part of Kavach depends on this one format. The SDK and the recorder *write* it (Chapters 2 and 3). Replay *reads* it and plays it back (Chapter 5). `kavach diff` *mutates* it into variants to test a fix (also Chapter 5). If you understand the journal, every other chapter is a question of who produces which records and who consumes them.

## Code map

The package is four files. Figure 1.1 is drawn from the Graphify code graph: each box is a file, and an arrow means functions in one file call functions in the other, thicker for more calls. Figures 1.2 and 1.3 follow the two paths through the package, writing and reading, from the call graph.

@graph files journal/ | Figure 1.1: Files of the journal package and the calls between them (from graphify-out-core/graph.json).
@graph calls journal_writer_write 2 | Figure 1.2: What Writer.Write calls, two levels deep. Dark: the function; orange: its callers.
@graph calls journal_reader_next 2 | Figure 1.3: What Reader.Next calls, two levels deep.
@graphify explain journal_order_check

## A real fixture, read top to bottom

The demo service in `examples/ledger/sdk` is a wallet ledger that crashes when an upstream system sends `"amount": null`. Its fixture is checked in. `kavach inspect` prints one line per record:

```
$ go run ./cmd/kavach inspect examples/ledger/testdata/null-amount.kavach
service ledger · start genesis · 34 records

 0  environment  19 facts
 1  input     file:events.jsonl @ 1  {"id":"evt-001","type":"deposit","account":"alice","amount":5000}
 2  clock     2026-10-09T19:15:08.116401Z
 3  rand      8 bytes  e75cadcbeb3804ee
 4  output    ledger.entries  {"txn":"e75cadcbeb3804ee","event":"evt-001","account":"alice","delta":5000,...}
 5  input     file:events.jsonl @ 2  {"id":"evt-002","type":"deposit","account":"bob","amount":1200}
 6  clock     ...
 ...
33  marker    panic: ...
```

Read it as the script of the handler's life:

- **Each `input` begins a step.** The records up to the next `input` are what happened while the handler processed that event.
- **Reads** (`clock`, `rand`, `gateway`, `config`) are the answers the world gave the handler. Replay serves exactly these answers back, in the same order. The handler cannot tell it is not in production, so it takes the same path and reaches the same bug.
- **Outputs** are what the handler asked to happen. Replay never performs them; it compares them with what production emitted. If a step *before* the failure emits something different, a fix has changed behaviour it should not have, and the verdict is `diverged@N`.
- **Markers** record how a step ended badly: `panic`, `error`, `invariant`, or `crash` when the process died mid-step.
- **`environment`** records the facts around the run: environment variables, feature flags, host and kernel facts. Replay serves the ones the software controls and compares the ones it cannot recreate, and reports the difference as **drift**.
- **`snapshot`** (absent here, because this journal starts from *genesis*) holds the handler's serialized state, so a journal can start in the middle of a service's life instead of at its first event.

The order is fixed by the specification:

@spec 5

## The bytes, from the outside in

```
$ xxd examples/ledger/testdata/null-amount.kavach | head -2
00000000: 5a2a 4d18 8d01 0000 894b 564a 0d0a 1a0a  Z*M......KVJ....
00000010: 0000 0200 7901 0000 7b22 7365 7276 6963  ....u...{"servic
```

The file is built in four layers:

```
file   = header-frame block*
block  = block-frame data-frame
record = body_len | type | flags | seq | payload
```

1. **The whole file is a valid Zstandard stream.** The header and the block headers are zstd *skippable frames* (magic `0x184D2A5A` and `0x184D2A5B`, which you can see byte-reversed as `5a2a4d18` at offset 0): frames every zstd decoder is required to skip. So `zstd -d` on a `.kavach` file prints the raw records with no Kavach tooling at all. The 1,954-byte demo fixture decompresses to 4,167 bytes of records.
2. **The header** carries a PNG-style magic number (`\x89KVJ\r\n\x1a\n`), the format version (0.2), a JSON metadata object and a CRC.
3. **Blocks** group records, up to about 4 MiB raw, and compress each group as an independent zstd frame with its own content checksum.
4. **Records** are length-prefixed, so a reader can skip a type it does not know.

@spec-only 3
@spec 2

### The header

@spec 3.1

The magic is the same trick PNG uses. Its first byte has the high bit set, so a transfer that strips the eighth bit breaks it; it contains CR LF, which text-mode transfers rewrite; and `^Z` stops `type` on DOS. A file damaged by being treated as text fails on its first eight bytes instead of somewhere in the middle.

@code journal/record.go 14-29

The metadata is JSON because it is read rarely, by people as often as by programs, and must be extensible: readers ignore keys they do not recognise.

@spec 3.2
@code journal/io.go type:Meta
@code journal/io.go func:appendHeader

### Blocks

@spec 3.3

Two properties fall out of compressing each block on its own. **Damage stays local**: a corrupted block costs its own records and nothing else. **Seeking is cheap**: every block frame states `first_seq`, `count` and `data_len`, so a reader looking for record five million hops from block header to block header, skipping data frames unread, and decompresses only the block it needs.

@code journal/block.go 13-32
@code journal/block.go type:blockHeader
@code journal/block.go func:appendBlock

### Records

@spec 3.4

Records carry no checksum of their own: the block's zstd content checksum already covers every byte of them. The `body_len` prefix is what makes the format extensible. A reader from version 0.2 that meets record type `0x0A` from a future 0.3 can skip it, unless it is marked *critical*, in which case it must refuse (§7), because replaying without understanding it would give a wrong answer.

@code journal/record.go 31-48

`Record` is a single struct that holds the fields of every type. Which fields mean something depends on `Type`, and the comment above it is the table to keep in mind:

@code journal/record.go type:Record
@code journal/record.go func:AppendPayload
@code journal/record.go func:AppendFrame

Decoding is the mirror image. `payload` is a small cursor over the bytes; each read method records the first error and returns zero values afterwards, so `parsePayload` can be written as straight-line code and check the error once at the end.

@code journal/record.go func:parsePayload

## The record types

Every type's payload is a fixed sequence of fields. Read these sections once now and come back to them; Chapters 2 and 5 refer to them constantly.

@spec 4

## The rules that make a journal trustworthy

The `Writer` refuses to write, and the `Reader` refuses to read, a journal that breaks these rules. All of them live in one method:

@code journal/io.go func:check

- **`seq` increases by exactly one.** A gap means a lost record, and a journal with a hole replays to a wrong answer. This is why, in Chapter 4, the SDK waits for space when the ring is full instead of silently dropping records.
- **Two ways to start.** A *genesis* journal starts at `seq` 0 from empty state. A *snapshot* journal starts with exactly one `snapshot` record. Either way, the journal is replayable on its own, which is what makes segments and fixtures self-contained.
- **The environment comes first.** No `input` before the first `environment` record.
- **Some records must be critical.** `gateway`, `environment` and `config` change what replay serves, so an older reader must not skip them.

### Truncated is fine; corrupt is not

@spec 3.5

This distinction is deliberate. A recorder can be killed at any moment, so a file that simply *ends* early is normal, and the reader uses every complete block. A *complete* block that fails its checksum is different: something rewrote bytes that were once correct. Replaying on top of that would produce a confident, wrong verdict, which is the worst thing Kavach can do. So it fails loudly instead.

:::trap Common trap
Do not "repair" a journal by skipping a bad block. A replay that silently drops a block replays a different history from the one production lived, and every verdict after that point is meaningless. The only safe response to corruption is to stop.
:::

## The Go API

| You want to | Use |
| --- | --- |
| Read a whole file | `journal.ReadFile(path)` returns the header and every record |
| Stream through a large one | `NewReader(r)`, then `Next()` until `io.EOF`; `Truncated()` says whether the end was cut off |
| Write a stream | `NewWriterOptions(w, meta, opts)`, then `Write(rec)` or `WriteEncoded(...)`, `Flush()`, `Close()` |
| Write a file with fsync | `OpenFile(path, meta, opts)`, then `Append`, `Flush`, `Close` |

### Writing

The `Writer` gathers records into a raw buffer and turns it into a block when it reaches the target size or when it is flushed. With `WriterOptions.Async`, compression runs on a goroutine of its own, so the caller (the recorder, in Chapter 3) does not wait for zstd. `WriteEncoded` is the recorder's fast path: it takes a payload that is already encoded, as it arrives from the SDK, and validates it without decoding it into a `Record`.

@code journal/io.go type:WriterOptions
@code journal/io.go type:Writer
@code journal/io.go func:Write
@code journal/io.go func:WriteEncoded
@code journal/io.go func:flushBlock
@code journal/io.go func:run

### Reading

The `Reader` walks frames. It skips skippable frames it does not know, reads a block frame, checks its CRC, decompresses the data frame, and splits it into records. `cut` is where truncation is handled: when the file ends inside a block, the reader remembers how many bytes it ignored and reports a clean end.

@code journal/io.go func:Next
@code journal/io.go func:nextBlock
@code journal/io.go func:records

### Files

`File` wraps a `Writer` around an `*os.File` and adds fsync. Opening an existing journal keeps its header and cuts off a block left half-written by a crash, so a service that restarts can keep appending.

@code journal/file.go 12-54
@code journal/file.go func:Flush

## Conformance: one format, many readers

`spec/testdata/valid` and `spec/testdata/invalid` hold golden files: journals that every reader must accept and decode to the listed records, and journals that every reader must reject. `journal/conformance_test.go` runs the Go reader against all of them; the reader in every other language is held to the same files. Chapter 6 returns to this.

:::try Try it
```
zstd -dc examples/ledger/testdata/null-amount.kavach | xxd | head
go run ./cmd/kavach inspect --full examples/ledger/testdata/null-amount.kavach
go test ./journal -run Conformance -v
graphify explain "Record" --graph graphify-out-core/graph.json
```
:::
