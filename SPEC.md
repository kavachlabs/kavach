# Kavach Journal Format — v0

Status: **draft, format version 0.1.** Until 1.0, any change may break
compatibility (see [§7](#7-versioning-and-compatibility)).

The key words MUST, MUST NOT, SHOULD and MAY are used as in RFC 2119.

## 1. Purpose and model

A Kavach journal records everything a single-writer handler needs to
re-derive its state and outputs deterministically:

- the **inputs** it consumed, in order;
- every **clock** and **random** value it read, in the order read;
- the **outputs** it produced, so a replay can be compared against them;
- **markers** that annotate the run (a panic, an error, a manual trigger).

The handler is treated as a pure function of its starting state and this
event sequence. Replay re-runs the handler against the journal; it does not
replay recorded responses from external systems, and it executes no side
effects.

A **fixture** is a journal file written to disk, usually by the flight recorder
when a failure happens. Fixtures use the extension `.kavach`.

## 2. Conventions

- All fixed-width integers are little-endian.
- `u8`, `u16`, `u32`, `u64` are unsigned; `i64` is two's-complement signed.
- `uvarint` is an unsigned LEB128 varint, as Go's `encoding/binary.PutUvarint`
  writes it. A uvarint longer than 10 bytes is invalid.
- `bytes` is a `uvarint` length followed by that many bytes.
- `string` is `bytes` whose content MUST be valid UTF-8.
- `crc32c` is CRC-32 with the Castagnoli polynomial (Go's `crc32.Castagnoli`).

## 3. File layout

```
file   = header record*
```

### 3.1 Header

| Field | Type | Value |
| --- | --- | --- |
| magic | 8 bytes | `89 4B 56 4A 0D 0A 1A 0A` (`\x89KVJ\r\n\x1a\n`) |
| major | `u16` | `0` |
| minor | `u16` | `1` |
| meta_len | `u32` | length of `meta` in bytes |
| meta | `meta_len` bytes | UTF-8 JSON object (§3.2) |
| header_crc | `u32` | `crc32c` over `major`, `minor`, `meta_len` and `meta` |

The magic's high first byte and its CR/LF/^Z bytes catch files that were
mangled by text-mode transfers, in the same way as PNG.

A reader MUST reject a file whose magic or `header_crc` does not match, or
whose `meta_len` exceeds 1 MiB.

### 3.2 Header metadata

`meta` is a JSON object. Readers MUST ignore keys they do not recognise.

| Key | Type | Required | Meaning |
| --- | --- | --- | --- |
| `service` | string | yes | Name of the service that produced the journal. |
| `start` | string | yes | Where the journal starts. `"genesis"`: the first record is the first event the handler ever consumed, from an empty state. `"snapshot"`: the first record is a `snapshot` record (§4.6) holding the handler's state. |
| `handler` | string | no | Build identity of the handler that wrote it, e.g. a module version or VCS revision. |
| `producer` | string | no | Library that wrote the file, e.g. `"kavach-go/0.1.0"`. |
| `recorded_at` | string | no | RFC 3339 wall-clock time of the flush. Informational only; replay MUST NOT read it. |
| `variant` | object | no | Present only on a **variant** (§6.1): a journal derived from a recorded incident by perturbing it. Keys: `id` (number), `mutation` (string, human-readable), `incident` (number, the `seq` of the input on which the old build is expected to fail) and `failure` (string, the recorded failure: `"panic: <message>"`, `"error: <message>"` or `"invariant: <name>"`). |

### 3.3 Records

Each record is framed as:

| Field | Type | Meaning |
| --- | --- | --- |
| body_len | `u32` | length of `body` in bytes, at least 10 and at most 16 MiB |
| body | `body_len` bytes | see below |
| crc | `u32` | `crc32c` over `body` |

`body` is:

| Field | Type | Meaning |
| --- | --- | --- |
| type | `u8` | record type (§4) |
| flags | `u8` | bit 0 = **critical** (§7); bits 1–7 MUST be zero in 0.1 |
| seq | `u64` | sequence number |
| payload | rest of body | type-specific (§4) |

Sequence numbers MUST increase by exactly one from each record to the next. In
a journal with `start: "genesis"` the first record has `seq` 0 and no record is
a `snapshot`. In a journal with `start: "snapshot"` the first record is a
`snapshot`, its `seq` may be any value, and no later record is a `snapshot`. A
gap or repeat, or a misplaced `snapshot`, is a fatal error.

The fixed `body_len` prefix lets a reader skip any record, including one of an
unknown type, without parsing it.

### 3.4 Truncation and corruption

A flight recorder can be interrupted mid-flush, so:

- If the file ends inside a record (fewer bytes remain than the frame
  requires), a reader MUST treat the journal as ending after the last complete
  record and SHOULD report a warning.
- If a complete record's `crc` does not match, or its `body_len` is out of
  range, the reader MUST fail. Corruption is never silently skipped.

## 4. Record types

Type codes `0x01`–`0x7F` are defined by this spec. `0x80`–`0xFF` are reserved
for extensions and MAY be used by applications. `0x00` is invalid.

Payloads are a fixed sequence of fields. A later minor version MAY append
fields to an existing payload; readers MUST ignore payload bytes after the
fields they know.

### 4.1 `input` — `0x01`

One event consumed by the handler. Each `input` begins one handler step.

| Field | Type | Meaning |
| --- | --- | --- |
| source | `string` | Where the event came from, e.g. `"kafka:wallet-events"`. |
| position | `string` | Its position in that source, e.g. `"3:1042"` for partition 3, offset 1042. Opaque to Kavach. |
| data | `bytes` | The event exactly as the handler received it. Opaque to Kavach. |

### 4.2 `clock` — `0x02`

One read of the injected clock.

| Field | Type | Meaning |
| --- | --- | --- |
| unix_nanos | `i64` | Nanoseconds since the Unix epoch, UTC. |

### 4.3 `rand` — `0x03`

One read of the injected random source.

| Field | Type | Meaning |
| --- | --- | --- |
| data | `bytes` | The random bytes returned, in order. |

### 4.4 `output` — `0x04`

One effect the handler requested, such as a write, a publish or a reply. During
replay, outputs are captured and compared; they are never executed.

| Field | Type | Meaning |
| --- | --- | --- |
| sink | `string` | Where the effect was directed, e.g. `"postgres:balances"`. |
| data | `bytes` | The effect's content in the application's own encoding. |

### 4.5 `marker` — `0x05`

An annotation. Markers are never served to the handler.

| Field | Type | Meaning |
| --- | --- | --- |
| kind | `string` | See below. |
| message | `string` | Human-readable summary. |
| data | `bytes` | Optional detail, e.g. a stack trace. May be empty. |

Defined kinds: `panic` (the handler panicked), `error` (the handler returned an
error), `trigger` (a manual flush), `invariant` (a declared invariant failed;
`message` names it). Other kinds MAY be used; readers MUST accept unknown kinds.

### 4.6 `snapshot` — `0x06`

The handler's state, so that a journal can start somewhere other than genesis.
Only valid as the first record of a journal with `start: "snapshot"`. Writers
MUST set the critical flag on it.

| Field | Type | Meaning |
| --- | --- | --- |
| data | `bytes` | The state in the handler's own encoding. Opaque to Kavach. |

A flight recorder keeps a bounded window of history by taking a snapshot every
N steps and discarding the records before it. Its `seq` keeps counting from the
journal it was cut from, so record numbers in a fixture match production.

## 5. Ordering

Within one handler step, records appear in the order they happened:

```
input  (clock | rand | output)*  [marker]
```

That is, an `input` is followed by every clock read, random read and output of
the step that handled it, interleaved in program order, optionally followed by
a `marker` if the step ended in a panic, error or invariant failure. A `marker`
of kind `trigger` MAY appear between steps.

A writer MUST NOT reorder records within a step.

## 6. Replay semantics

To replay a journal, a replayer starts the handler from the state named by
`start` (empty, or restored from the `snapshot` record) and, for each `input`
in order, runs one handler step:

1. When the handler reads the clock, the next unread read of the step (its
   `clock` and `rand` records, in order) MUST be a `clock`; its value is
   returned. Likewise for `rand`: the handler receives exactly the recorded
   bytes, and requesting a different number of bytes is nondeterminism.
2. When the handler produces an output, it is captured. It is not compared
   against the next recorded `output` until the step ends.
3. If the handler reads the clock or random source and the next record is of
   another type, or the step has no records left, replay stops with a
   **nondeterminism** result naming the `seq` where the expectation failed.
   The same applies if the step ends while `clock` or `rand` records of that
   step remain unread.

**The failing step.** The step whose input is followed by a `panic`, `error`
or `invariant` marker is the **recorded failure**. A fixed handler is expected
to behave differently there, and may read more than the failing handler did
before it stopped. For that step only, rules 1 and 3 are relaxed: `clock` and
`rand` reads are served from the step's records of that type in recorded order;
once those run out, a `clock` read returns the last time served and a `rand`
read returns bytes derived deterministically from the step's `seq` and a read
counter. A replayer MUST report how many reads it synthesized.

A replay result reports, at least: the steps run, every captured output, any
panic or error, and its status:

| Status | Meaning |
| --- | --- |
| `nondeterministic@N` | Reads did not match the journal at record `N`. |
| `still_failing@N` | The step of input `N` panicked or returned an error. |
| `invariant_violated(X)@N` | Invariant `X` failed after the step of input `N`. |
| `diverged@N` | A step other than the recorded failure, at input `N`, produced outputs different from those recorded (the **divergence point**). |
| `fixed` | A failure was recorded; every step ran without any of the above. |
| `ok` | No failure was recorded; every step ran without any of the above. |

Checks are applied in the order of the table, per step, and replay stops at the
first one that applies.

Two builds of a handler are compared by replaying the same journal under both
and reporting the first output where their captured outputs differ.

**Variants.** A journal whose header has a `variant` key never happened, so it
holds no `output` or `marker` records and its `clock` and `rand` records are
only a source of plausible values. A replayer MUST treat every step of a
variant like the failing step above (lenient reads), MUST NOT report
`diverged`, and reports `ok` when no other status applies.

### 6.1 Verifying a fix

A candidate fix is checked against an incident journal with two builds: the
**old** build, which failed in production, and the **new** one. The fix counts
only if:

1. the new build replays the recorded journal as `fixed`;
2. at least *M* variants of the incident **reproduce** it: replayed under the
   old build, each fails at its `incident` input exactly as recorded (same
   `failure` string, compared in full); variants that do not reproduce say
   nothing about the fix and are ignored; and
3. the new build passes every reproducing variant: it replays as `ok`, and
   every step before the `incident` input produces the old build's outputs.

The reference implementation uses *M* = 10 and derives up to 64 variants from
an incident: the failing input's JSON fields set to values the same field
takes in other inputs and to nearby values, removed, or added from other
inputs; the failing input moved earlier; earlier inputs dropped or delivered
twice; and every clock read shifted. Variant generation is deterministic.

| Verdict | Meaning |
| --- | --- |
| `variant_failed(K)@N` | Rule 3 failed for variant `K`, at input `N` of the variant. |
| `unverified` | Rules 1 and 3 hold, but fewer than *M* variants reproduce the incident. |

Otherwise the verdict is the new build's replay status on the recorded journal
(`fixed` when all three rules hold).

## 7. Versioning and compatibility

The format version is `major.minor`, stored in the header.

These rules apply from 1.0:

- A reader MUST reject a file with a `major` it does not know.
- A minor version MAY add record types, add header keys, and append fields to
  existing payloads. It MUST NOT change the meaning of existing fields.
- A reader MUST skip records of a type it does not know, unless the record's
  critical flag is set, in which case it MUST reject the file. Writers set the
  critical flag on records that change how the journal must be replayed.
- A reader MUST ignore payload bytes after the fields it knows.

Before 1.0 (major `0`), a reader SHOULD reject a file whose `minor` is newer
than the newest it knows, and formats may change between minors without
notice. Conformance fixtures are regenerated on every 0.x change.

## 8. Conformance

The conformance suite lives in [`spec/testdata/`](spec/testdata): each file in
`valid/` is paired with a `.json` file holding the expected decoding (byte
fields base64-encoded, record types by name), and every file in `invalid/` MUST
be rejected. The Go implementation regenerates the suite with
`go test ./journal -update`. Any implementation, in any language, that decodes
every valid file to the expected JSON and rejects every invalid one conforms
to this version.

## 9. Open questions

- **Starting mid-stream** — *resolved in 0.1 by the `snapshot` record (§4.6).*
  Referencing the service's own durable log by position was rejected: the
  fixture would no longer replay on its own, without access to that log.
- **Concurrency inside a step.** 0.1 assumes the handler reads the clock and
  random source from one goroutine. Concurrent reads within a step would make
  their order nondeterministic.
- **Large inputs.** The 16 MiB record limit may be too small for some batch
  events.
