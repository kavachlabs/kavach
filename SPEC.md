# Kavach Journal Format — v0

Status: **draft, format version 0.2.** Until 1.0, any change may break
compatibility (see [§7](#7-versioning-and-compatibility)).

> **Implementation status.** The reference implementation writes and reads
> 0.1. Everything 0.2 adds is not yet implemented: the Zstandard file layout
> and continuous recording ([§3](#3-file-layout)), the recorder protocol
> ([§10](#10-recorder-protocol)), the `gateway`, `environment` and `config` records
> ([§4.7](#47-gateway--0x07)–[§4.9](#49-config--0x09)), the `scope` of
> outputs, environment drift ([§6.2](#62-environment-drift)), sandbox replay
> ([§6.3](#63-sandbox-replay)), the variants they add in
> [§6.1](#61-verifying-a-fix) and the host protocol ([§9](#9-host-protocol)).

The key words MUST, MUST NOT, SHOULD and MAY are used as in RFC 2119.

## 1. Purpose and model

A Kavach journal records everything a single-writer handler needs to
re-derive its state and outputs deterministically:

- the **environment** the process ran in: its environment variables, its
  feature flags, and facts about the host and kernel, captured before the
  first input and again whenever they changed;
- the **inputs** it consumed, in order;
- every **read** it made of the world, in the order made: the clock, the
  random source, the responses to **gateway** queries, and **config** values
  such as feature flags;
- the **outputs** it produced, so a replay can be compared against them;
- **markers** that annotate the run (a panic, an error, a manual trigger).

The handler is treated as a pure function of its starting state, its
environment and this event sequence. Replay re-runs the handler against the
journal and executes no side effects.

The environment is treated in two ways. What the software controls, its
environment variables and config values, is **served**: replay runs with the
values production had. What it does not, the host and kernel, cannot be
recreated on the replay machine, so it is **compared**: replay reports every
fact that differs between production and the replay (§6.2). Failures caused
from outside the process, such as a file removed by the init system, then
show up as a difference in the environment next to the read that failed.
Replayed in a **sandbox** built from the recorded environment (§6.3), they
happen again, and a fix to the environment can be verified like a fix to
code.

The handler reaches the outside world only through two boundaries, which
follow the external queries and external updates of event sourcing:

- **Queries** go through a gateway (§4.7). Live, the call is made and its
  response recorded; in replay, the recorded response is returned and nothing
  is called. A journal records one response per query the handler asked, in
  the bytes its connection received, not the network traffic that carried it.
- **Updates** are outputs (§4.4). Live, they are delivered after the step
  succeeds; in replay, they are captured and compared, never delivered.

The **flight recorder** is a pipe. The process writes records into one end,
cheaply and without waiting on disk. At the other end, the **recorder**, a
separate process, batches them into compressed blocks and appends them to a
journal file (§3.6, §10). The journal is written continuously, not only when
something fails.

A **fixture** is a journal file, or a part cut from one, that holds a failure.
Fixtures and journals use the extension `.kavach`. Every `.kavach` file is a
valid Zstandard stream (RFC 8878), compressed by default (§3).

## 2. Conventions

- All fixed-width integers are little-endian.
- `u8`, `u16`, `u32`, `u64` are unsigned; `i64` is two's-complement signed.
- `uvarint` is an unsigned LEB128 varint, as Go's `encoding/binary.PutUvarint`
  writes it. A uvarint longer than 10 bytes is invalid.
- `bytes` is a `uvarint` length followed by that many bytes.
- `string` is `bytes` whose content MUST be valid UTF-8.
- `crc32c` is CRC-32 with the Castagnoli polynomial (Go's `crc32.Castagnoli`).
- A **zstd frame** is a Zstandard frame (RFC 8878 §3.1.1). A **skippable
  frame** is a Zstandard skippable frame (RFC 8878 §3.1.2): a `u32` magic
  number from `0x184D2A50` to `0x184D2A5F`, a `u32` length, and that many bytes
  of user data, which Zstandard decoders skip.

## 3. File layout

```
file   = header-frame block*
block  = block-frame data-frame
```

| Frame | Kind | Holds |
| --- | --- | --- |
| header-frame | skippable, magic `0x184D2A5A` | the header (§3.1) |
| block-frame | skippable, magic `0x184D2A5B` | the block header (§3.3) |
| data-frame | zstd frame | the block's records (§3.4) |

A file is a sequence of frames, so every `.kavach` file is a valid Zstandard
stream: `zstd -d` accepts it, skips the header and block headers, and writes
out the records of every block back to back. Kavach readers decode the frames
themselves and need the header and block headers.

A file is either **compressed**, the default, or **uncompressed**. They differ
only in how data frames are encoded (§3.3); the layout, and how a reader reads
them, is the same. A recorder MUST compress unless it is configured not to.

### 3.1 Header

The header frame's user data is:

| Field | Type | Value |
| --- | --- | --- |
| magic | 8 bytes | `89 4B 56 4A 0D 0A 1A 0A` (`\x89KVJ\r\n\x1a\n`) |
| major | `u16` | `0` |
| minor | `u16` | `2` |
| meta_len | `u32` | length of `meta` in bytes |
| meta | `meta_len` bytes | UTF-8 JSON object (§3.2) |
| header_crc | `u32` | `crc32c` over `major`, `minor`, `meta_len` and `meta` |

A file therefore begins with the bytes `5A 2A 4D 18`, the header frame's
length, and the Kavach magic at offset 8. The magic's high first byte and its
CR/LF/^Z bytes catch files that were mangled by text-mode transfers, in the
same way as PNG.

A reader MUST reject a file that does not begin with a header frame, whose
magic or `header_crc` does not match, or whose `meta_len` exceeds 1 MiB.

### 3.2 Header metadata

`meta` is a JSON object. Readers MUST ignore keys they do not recognise.

| Key | Type | Required | Meaning |
| --- | --- | --- | --- |
| `service` | string | yes | Name of the service that produced the journal. |
| `start` | string | yes | Where the journal starts. `"genesis"`: the first record is the first event the handler ever consumed, from an empty state. `"snapshot"`: the first record is a `snapshot` record (§4.6) holding the handler's state. |
| `compression` | string | yes | `"zstd"` or `"none"` (§3.3). Informational: a reader decodes data frames the same way either way. |
| `handler` | string | no | Build identity of the handler that wrote it, e.g. a module version or VCS revision. |
| `producer` | string | no | SDK that produced the records, e.g. `"kavach-python/0.1.0"`. |
| `recorder` | string | no | Recorder that wrote the file, e.g. `"kavach-recorder/0.2.0"`. |
| `recorded_at` | string | no | RFC 3339 wall-clock time the file was started. Informational only; replay MUST NOT read it. |
| `run` | string | no | Identifier of the process run that wrote the journal, unique per process start. Segments of one run (§3.6) share it. |
| `segment` | number | no | Index of this file among the segments of its run, from 0. |
| `cut_from` | object | no | Present on a fixture cut from a longer journal (§3.6). Keys: `run`, `segment`, `first_seq`, `last_seq`. |
| `variant` | object | no | Present only on a **variant** (§6.1): a journal derived from a recorded incident by perturbing it. Keys: `id` (number), `mutation` (string, human-readable), `incident` (number, the `seq` of the input on which the old build is expected to fail) and `failure` (string, the recorded failure: `"panic: <message>"`, `"error: <message>"`, `"invariant: <name>"` or `"crash"`). |

### 3.3 Blocks

A block is a block frame followed by a data frame. The block frame's user
data is:

| Field | Type | Meaning |
| --- | --- | --- |
| first_seq | `u64` | `seq` of the block's first record. |
| count | `u32` | number of records in the block, at least 1. |
| raw_len | `u32` | length of the block's records, uncompressed, at most 64 MiB. |
| data_len | `u32` | length in bytes of the data frame that follows, at most 64 MiB. |
| header_crc | `u32` | `crc32c` over the fields above. |

The data frame is one zstd frame whose content is the block's records (§3.4),
concatenated. It MUST declare its content size, equal to `raw_len`, and MUST
carry a content checksum (RFC 8878 §3.1.1.1.1), which covers the records. It
MUST NOT use a dictionary.

- In a **compressed** file, data frames are compressed. The reference
  recorder uses compression level 3.
- In an **uncompressed** file, data frames hold only raw blocks (RFC 8878
  §3.1.1.2.2): the records are stored as they are, in pieces of at most
  128 KiB, each behind a 3-byte block header. A hex dump shows them, and
  writing them needs no compressor.

Each block is a frame of its own, sharing no window with other blocks, so a
reader can decode any block alone and a damaged block loses only its own
records. `first_seq`, `count` and `data_len` let a reader find the block
holding a given `seq` by reading block frames and skipping data frames,
without decompressing anything.

Writers choose block size freely within the limits; the reference recorder
closes a block when its raw records reach 4 MiB, and earlier when §3.6
requires it, so blocks are often much smaller. A block holding one record
that is larger than the target is valid.

A file MAY contain skippable frames with other magic numbers between blocks.
Readers MUST skip them. A future version may use them, for example for an
index.

### 3.4 Records

Inside a data frame's content, each record is framed as:

| Field | Type | Meaning |
| --- | --- | --- |
| body_len | `uvarint` | length of `body` in bytes, at least 10 and at most 16 MiB |
| body | `body_len` bytes | see below |

Records have no checksum of their own: the data frame's content checksum
covers them.

`body` is:

| Field | Type | Meaning |
| --- | --- | --- |
| type | `u8` | record type (§4) |
| flags | `u8` | bit 0 = **critical** (§7); bits 1–7 MUST be zero in 0.2 |
| seq | `u64` | sequence number |
| payload | rest of body | type-specific (§4) |

Sequence numbers MUST increase by exactly one from each record to the next. In
a journal with `start: "genesis"` the first record has `seq` 0 and no record is
a `snapshot`. In a journal with `start: "snapshot"` the first record is a
`snapshot`, its `seq` may be any value, and no later record is a `snapshot`. A
gap or repeat, or a misplaced `snapshot`, is a fatal error. Sequence numbers
continue across blocks: a block's `first_seq` is one more than the last `seq`
of the block before it.

The `body_len` prefix lets a reader skip any record, including one of an
unknown type, without parsing it. A block whose records do not add up to
exactly `raw_len` bytes and `count` records is corrupt.

### 3.5 Truncation and corruption

A recorder can be interrupted mid-block, so:

- If the file ends inside a block (inside its block frame, or before its data
  frame is complete), a reader MUST treat the journal as ending after the last
  complete block and SHOULD report a warning saying how many bytes it ignored.
- If a complete block's `header_crc` does not match, its data frame does not
  decode, its content checksum or content size does not match, or its records
  are malformed, the reader MUST fail. Corruption is never silently skipped.

### 3.6 Recording

The flight recorder is a pipe from the process to the journal file:

```
handler ─▶ SDK: encode ─▶ OS pipe ─▶ recorder: number ▸ batch ▸ compress ▸ append ─▶ .kavach
            (in the step)              (a separate process, §10)
```

**The process end** is the SDK, inside the service. It encodes each record
and writes it into the pipe. It MUST NOT wait on compression or on the disk;
it waits only if the pipe is full.

**The recorder end** is a separate process, `kavach-recorder`, that the SDK
starts and speaks to as §10 describes. It is written once, for every
language, and owns everything that does not need to be inside the service:
assigning sequence numbers, blocks, compression, durability, segments,
fixtures, and collecting the environment (§4.8). Records the SDK has written
into the pipe survive the service being killed: the recorder reads them, sees
the pipe close, and finishes the journal.

The recorder MUST close and write the open block:

- when its raw records reach the target size;
- when the oldest record in it has waited a **flush interval** (reference
  default 1 second);
- after a `marker` of kind `panic`, `error`, `invariant` or `crash`, so that
  the failure is on disk before anything else happens;
- when the pipe closes, or the SDK asks it to (§10).

A recorder SHOULD make each written block durable (as `fsync` does) after a
failure marker and when the pipe closes, and MAY do so less often otherwise.

**When the pipe or ring (§10.7) is full**, the SDK MUST NOT silently drop a
record: a journal with a hole in it replays to a wrong answer. It either waits
for space, which the reference SDKs do, or it drops records and then writes a
`marker` of kind `dropped` (§4.5) in their place, with the number dropped.
Records after a `dropped` marker cannot be replayed until the next snapshot
(§4.6).

**Segments.** A journal written continuously grows without bound, so the
recorder starts a new file, a **segment**, when the current one reaches a
size or age limit. Every segment is a complete journal: it has its own header
with the same `run` and the next `segment` index, starts with a `snapshot` of
the handler's state followed by the full `environment` (§5), and continues the
run's sequence numbers. The snapshot comes from the SDK, on request (§10.4). A
handler that is not a `Snapshotter` cannot be segmented, and its journal is one
file per run. The recorder keeps segments for a configured retention.

**Fixtures.** A fixture is the journal of a failure, ready to replay on its
own. When a step ends in a failure marker, the recorder writes one: the
records from the current segment's snapshot through the end of the failing
step, in a new file whose header names its origin in `cut_from` and MAY
repeat the segment's `run` and `segment`. Cutting MUST
keep `seq` values, so that records in a fixture are numbered as in production.

## 4. Record types

Type codes `0x01`–`0x7F` are defined by this spec. `0x80`–`0xFF` are reserved
for extensions and MAY be used by applications. `0x00` is invalid.

Payloads are a fixed sequence of fields. A later minor version MAY append
fields to an existing payload; readers MUST ignore payload bytes after the
fields they know.

`clock`, `rand`, `gateway` and `config` records are **reads**: values the
handler took from the world during a step, which replay serves back to it
(§6). `environment` records are not reads; the handler never asks for them.

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

The clock is processing time: when the handler ran. When the time an event
happened matters to the domain (its effective date), it belongs in the
input's `data`, where it is replayed as part of the event and not shifted by
clock variants (§6.1).

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
| scope | `u8` | `0`: **remote**, a system on another host. `1`: **local**, a resource of the host the process runs on, such as a file, shared memory or a local socket. See §6.3. |

### 4.5 `marker` — `0x05`

An annotation. Markers are never served to the handler.

| Field | Type | Meaning |
| --- | --- | --- |
| kind | `string` | See below. |
| message | `string` | Human-readable summary. |
| data | `bytes` | Optional detail, e.g. a stack trace. May be empty. |

Defined kinds: `panic` (the handler panicked or threw), `error` (the handler
returned an error), `trigger` (a manual flush), `invariant` (a declared
invariant failed; `message` names it), `crash` (the process ended during
the step without unwinding; written by the recorder, §10.5), `exit` (the
process ended between steps without closing the recorder; not a failure),
`dropped` (the SDK dropped records here because the pipe was full; `message`
gives how many, §3.6). Other kinds MAY be used; readers MUST accept unknown
kinds.

What counts as `panic` or `error` depends on the language. An SDK MUST
document its mapping, and MUST produce `message` with the same function when
recording and when replaying, so that a recorded failure and its replay
compare equal (§6.1). `message` MUST NOT depend on where or how the build
runs: an SDK removes file paths, line and column numbers, memory addresses and
object identities that its runtime puts into exception messages, and keeps
them in `data`. Otherwise a failure recorded in production would never compare
equal to its replay from another checkout. For example, a Python SDK might record an uncaught
exception as `panic` with message `"KeyError: 'amount'"`.

### 4.6 `snapshot` — `0x06`

The handler's state, so that a journal can start somewhere other than genesis.
Only valid as the first record of a journal with `start: "snapshot"`. Writers
MUST set the critical flag on it.

| Field | Type | Meaning |
| --- | --- | --- |
| data | `bytes` | The state in the handler's own encoding. Opaque to Kavach. |

The recorder starts each segment with a snapshot the SDK takes on request (§3.6, §10.4). Its `seq` keeps
counting from the run's journal, so record numbers in every segment and
fixture match production.

### 4.7 `gateway` — `0x07`

One query the handler made of an external system, and what came back. Writers
MUST set the critical flag on it: a replayer that skipped it would call the
handler without the response it needs.

| Field | Type | Meaning |
| --- | --- | --- |
| gateway | `string` | Which external system, e.g. `"fx-rates"` or `"postgres:accounts"`. Names are chosen by the application and MUST be stable across builds. |
| request | `bytes` | The query as the handler's connection sent it, in the application's encoding. |
| response | `bytes` | The response as the connection received it. Empty when `error` is set. |
| error | `string` | Empty if the query succeeded; otherwise the failure the connection reported, e.g. `"timeout"` or `"status 503"`. |
| scope | `u8` | `0`: remote. `1`: local, as for `output`. The application declares the scope of each gateway; it MUST NOT change between builds. |

What the record holds is the response at the **connection**, below the
gateway's translation into domain terms. That translation is handler code: it
runs again in replay, so a crash in parsing an upstream response reproduces.

One record is one logical query: transport retries, redirects and connection
reuse happen below the connection and are not recorded. A record's position
in the step is where the handler *issued* the query, not where its response
arrived, so concurrent queries are recorded in call order.

A query executes immediately when recording, not after the step like an
output. A call that both changes the external system and returns something the
handler needs (for example, creating a charge and reading its id) MAY be made
through a gateway; it is then never called in replay, and if the step later
fails, the change has already happened.

### 4.8 `environment` — `0x08`

The process's environment, as a set of facts. The recorder writes one with
every fact when the process starts, before the first `input`; this is the
journal's **genesis environment**. Afterwards, between steps, it writes one
holding only the facts that changed, so that the journal shows when the world
around the process moved. Writers MUST set the critical flag on it: a replayer
that skipped it would run with the wrong environment variables.

| Field | Type | Meaning |
| --- | --- | --- |
| count | `uvarint` | Number of facts that follow. |
| facts | `count` × fact | See below. |

Each fact is:

| Field | Type | Meaning |
| --- | --- | --- |
| key | `string` | Namespaced name, see below. Keys within one record are distinct. |
| form | `u8` | `0`: `value` is the value. `1`: `value` is the SHA-256 of the value. `2`: the fact was removed or is unset; `value` is empty. |
| value | `bytes` | Per `form`. |

Keys are namespaced:

| Prefix | Facts | Replay |
| --- | --- | --- |
| `env.` | Every environment variable of the process, e.g. `env.MAX_TRANSFER`. | Served (§9.1). |
| `flag.` | Every feature flag the process could evaluate, with the value it would have got at that moment, e.g. `flag.ledger-v2`. | Compared. Flag values the handler reads are served through `config` records (§4.9). |
| `host.` | Facts about the host, OS and kernel. | Compared. |

A writer MUST record secrets with form `1`: the hash still tells a replay
whether the value differs, without the fixture holding it. A writer MUST treat
as secret every `env.` key whose name contains `KEY`, `SECRET`, `TOKEN`,
`PASSWORD`, `PASSWD`, `CREDENTIAL`, `PRIVATE`, `AUTH`, `DSN` or `_URL`, ignoring
case, and every key the application marks secret; it MAY treat more as secret.
Low-entropy values are not secret and their hashes are not a protection.

The `host.` facts a writer SHOULD collect where the platform has them, so that
fixtures from different SDKs compare:

| Key | Value |
| --- | --- |
| `host.os`, `host.arch`, `host.kernel` | e.g. `linux`, `amd64`, `6.8.0-45-generic` |
| `host.hostname`, `host.user`, `host.uid` | the process's host and user |
| `host.runtime` | the service's language runtime and version, e.g. `go1.24.7`, `cpython-3.13.1`. Sent by the SDK (§10.4): the recorder runs a runtime of its own. |
| `host.cpus`, `host.memory_limit` | CPUs and memory limit available to the process, after cgroup limits |
| `host.tz`, `host.locale` | time zone and locale |
| `host.ulimit.<name>` | soft resource limits, e.g. `host.ulimit.nofile` |
| `host.mount.<path>` | file system type and options of each mount the process can see, e.g. `host.mount./dev/shm` = `tmpfs rw,nosuid,nodev` |
| `host.systemd.<setting>` | effective settings of the init system's login manager that act on processes, at least `RemoveIPC` and `KillUserProcesses` |
| `host.sessions.<user>` | number of active login sessions of the user running the process |
| `host.sysctl.<name>` | kernel parameters that bound what the process can use, at least `kernel.shmmax`, `kernel.shmall`, `fs.file-max` and `vm.overcommit_memory` |
| `host.container.image` | image reference, when in a container |

Writers MAY add facts under `host.x.`.

The recorder SHOULD watch `host.` and `flag.` facts while the process runs and
write a change record at the next step boundary after one changes. It SHOULD
NOT block a step to do so. `env.` facts of a process change only if the
process changes them itself, which a handler MUST NOT do.

### 4.9 `config` — `0x09`

One read of a config value that can change what the handler does: a feature
flag, a limit, a rollout percentage. The handler reads such values through its
`Env`, never directly from environment variables or a flag service, so that
replay can serve the value production saw at that moment. Writers MUST set the
critical flag on it.

| Field | Type | Meaning |
| --- | --- | --- |
| key | `string` | Which value, e.g. `"flag.ledger-v2"` or `"MAX_TRANSFER"`. |
| present | `u8` | `1` if the value is set, `0` if not. |
| value | `bytes` | The value; empty when `present` is `0`. |
| source | `string` | Where it came from, e.g. `"env"` or `"launchdarkly"`. Informational. |

Configuration that only connections use, such as connection strings and
credentials, is not read by the handler and has no `config` record; replay
never runs connections (§4.7).

## 5. Ordering

Within one handler step, records appear in the order they happened:

```
journal = [snapshot] environment step*
step    = input  (clock | rand | gateway | config | output)*  [marker]
          (environment | marker)*
```

That is, the journal's first `environment` record, the genesis environment,
comes before the first `input` (after the `snapshot`, if any). Each `input` is
followed by every read and output of the step that handled it, interleaved in
program order, optionally followed by a `marker` if the step ended in a panic,
error, invariant failure or crash. Between steps there MAY be `environment`
change records and `marker` records of kind `trigger`, `dropped` or `exit`.

When the recorder starts a segment (§3.6), it MUST write, right after the
`snapshot`, one `environment` record holding every fact as it stood at that
point, so that every journal starts with a complete environment.

A `dropped` marker MAY appear between steps. A replayer MUST refuse to replay
across one: a journal whose steps to be replayed include one cannot be
replayed, as with a corrupt file.

Records MUST NOT be reordered within a step. A journal without an
`environment` record before its first `input` is invalid.

## 6. Replay semantics

To replay a journal, a replayer starts the handler from the state named by
`start` (empty, or restored from the `snapshot` record) and, for each `input`
in order, runs one handler step:

1. When the handler reads the clock, the next unread read of the step MUST be
   a `clock`; its value is returned. Likewise for `rand`: the handler receives
   exactly the recorded bytes, and requesting a different number of bytes is
   nondeterminism. Likewise for a gateway query: the next unread read MUST be
   a `gateway` record with the same `gateway` and byte-identical `request`;
   the handler receives its `response`, or a failure carrying its `error`.
   Likewise for a config read: the next unread read MUST be a `config` record
   with the same `key`; the handler receives its value, or "not set".
2. When the handler produces an output, it is captured. It is not compared
   against the next recorded `output` until the step ends.
3. If the handler makes a read and the next record is of another type, or does
   not match as rule 1 requires, or the step has no records left, replay stops
   with a **nondeterminism** result naming the `seq` where the expectation
   failed. The same applies if the step ends while reads of that step remain
   unread.

A request that differs from the recorded one is nondeterminism, not a new
query: the recorded response answered a different question, and serving it
would make the replay meaningless. A build that changes what it asks an
external system in steps before the failure cannot be checked against that
journal.

**The failing step.** The step whose input is followed by a `panic`, `error`,
`invariant` or `crash` marker is the **recorded failure**. A fixed handler is expected
to behave differently there, and may read more than the failing handler did
before it stopped. For that step only, rules 1 and 3 are relaxed, and each
kind of read is served from the step's records of that type:

- `clock` reads are served in recorded order; once those run out, a read
  returns the last time served.
- `rand` reads are served in recorded order; once those run out, a read returns
  bytes derived deterministically from the step's `seq` and a read counter.
- A gateway query is served the first unread `gateway` record of the step with
  the same `gateway` and byte-identical `request`; failing that, the first
  unread record with the same `gateway`; failing that, the query fails with the
  error `"kavach: no recorded response"`.
- A config read is served the first unread `config` record of the step with
  the same `key`; failing that, the value last recorded for that key anywhere
  earlier in the journal, by a `config` record or as a `flag.` or `env.`
  fact of the latest `environment` record that holds that fact (a change
  record holds only the facts that changed); failing that, "not set".

A replayer MUST report how many reads it synthesized (served from no record,
or a `gateway` record whose `request` differed).

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
holds no `output` or `marker` records, and its reads are only a source of
plausible values. A replayer MUST treat every step of a variant like the
failing step above (lenient reads), MUST NOT report `diverged`, and reports
`ok` when no other status applies.

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

Responses are inputs from the world too, so from 0.2 variants also perturb
the `gateway` records of the failing step: the response's JSON fields mutated
as the failing input's are, with values taken from other responses of the
same `gateway` in the journal; a successful response replaced by an `error`
recorded for the same `gateway` elsewhere in the journal; and a failed one
replaced by a successful response recorded elsewhere. When the failing input
moves or earlier inputs are dropped, each step keeps its own `gateway`
records.

Variants also perturb the `config` reads of the failing step: a boolean value
flipped, the value replaced by another one recorded for the same key, and the
key unset. A flag that the failure depends on usually stops the variant from
reproducing it, and the variant is ignored; a flag that it does not depend on
tests that the fix does not depend on it either. Variants never change
`environment` records.

| Verdict | Meaning |
| --- | --- |
| `variant_failed(K)@N` | Rule 3 failed for variant `K`, at input `N` of the variant. |
| `unverified` | Rules 1 and 3 hold, but fewer than *M* variants reproduce the incident. |

Otherwise the verdict is the new build's replay status on the recorded journal
(`fixed` when all three rules hold).

### 6.2 Environment drift

A replayer runs the handler in an environment of its own, and MUST report how
it differs from the one recorded: the **drift**. It compares the replay's
facts, collected the same way (§9.2, `ready`), with the journal's genesis
environment, key by key, and reports each key that is missing on one side or
whose value differs, comparing a hashed fact by hashing the replay's value.
`env.` facts are served (§9.1), so they drift only where a secret's hash
differs or a variable could not be set.

The replayer MUST also list every `environment` change record in the journal
with the `seq` of the step after which it was written, so that a reader sees,
for example:

```
genesis   host.systemd.RemoveIPC = yes      (replay: no)
genesis   host.mount./dev/shm = tmpfs …
after 409 host.sessions.wfinfra 1 → 0
seq 413   gateway shm:/dev/shm/orderbook → error "ENOENT"
seq 414   marker panic: segment vanished
```

Drift does not change a replay's status: it explains it. A status reached with
drift in `host.` or `flag.` facts means only that the handler, given what
production gave it, behaves as reported. A process replay does not recreate
the host; a sandbox replay (§6.3) does, as far as its level allows. A replayer
MAY offer to treat drift in chosen keys as an error.

### 6.3 Sandbox replay

Replay as described so far is a **process replay**: the handler runs on the
replay machine, and everything outside the process is either served from the
journal or compared with it. A failure caused by the host, such as shared
memory removed by the init system when a user logs out, is explained by drift
but does not happen again, and a fix to the host cannot be checked, since the
old and new builds are the same binary.

A **sandbox replay** runs the host process (§9) inside a disposable machine
built from the journal's genesis environment, and makes the recorded changes
to that environment happen again at the steps where they happened.

#### Levels

| Level | Built as | Recreates |
| --- | --- | --- |
| `process` | No sandbox. | `env.` facts (§9.1). |
| `container` | A container on the replay machine's kernel. | Also: `host.container.image` as the base image when recorded, `host.user`, `host.uid`, `host.tz`, `host.locale`, `host.ulimit.`, `host.cpus`, `host.memory_limit`, `host.mount.` (except mounts of the host's own devices). |
| `vm` | A virtual machine with its own kernel and init system. | Also: `host.kernel` (the same version, or the nearest the replayer has, reported as drift), `host.sysctl.`, `host.systemd.`, `host.sessions.`. |

The replayer builds the sandbox for the level it is asked for and reports as
drift (§6.2) every fact the level does not recreate, or that it could not set.
Building is deterministic: the same journal and level produce the same
sandbox. A replayer SHOULD cache sandboxes by their facts.

At the `vm` level the replayer creates `host.user` with `host.uid`, starts the
init system with the recorded `host.systemd.` settings, opens as many login
sessions for the user as `host.sessions.<user>` records, and starts the host
process as that user, inside one of those sessions if there is any.

A sandbox MUST have no network. Remote gateways are served from the journal
and remote outputs are captured, as in a process replay, so the host needs
none; a handler that reaches the network without a gateway fails visibly in
the sandbox, where in a process replay it would have succeeded silently.

#### Local and remote

In a process replay, both scopes are treated alike. In a sandbox replay, what
is local runs for real, against the sandbox:

- A **local gateway** query is executed by the host, and the handler gets the
  live result. The driver compares it with the recorded `response` or `error`
  and reports a difference as a **host divergence**, with the step's `seq`;
  it is not nondeterminism, and its `request` is not matched (§6, rule 1),
  since an environment fix may legitimately change it.
- A **local output** is delivered by the host, after the step succeeds, as in
  production. It is also captured and compared.
- Before answering `hello`, the host runs the application's **local setup**:
  the startup code, declared to the SDK, that creates local resources the
  handler uses (a shared memory segment, a socket, a directory). Setup MUST
  NOT reach the network.

Remote reads and outputs are served and captured as in a process replay.

#### Replaying environment changes

Between steps, for each `environment` change record (§4.8) at that point, the
replayer makes the change happen in the sandbox through an **actuator**:

| Change | Actuator |
| --- | --- |
| `host.sessions.<user>` decreases | Terminate that many of the user's sessions, most recent first. |
| `host.sessions.<user>` increases | Open that many sessions for the user. |
| `host.mount.<path>` added, removed or changed | Mount, unmount or remount with the recorded type and options. |
| `host.ulimit.<name>` | Set the host process's limit (as `prlimit` does). |
| `host.memory_limit`, `host.cpus` | Set the sandbox's cgroup limits. |
| `host.sysctl.<name>` | Set the kernel parameter. |
| `host.systemd.<setting>` | Change the setting and reload the login manager. |
| `flag.` | Nothing; flag values the handler uses are served by `config` reads. |

After applying the changes at a step boundary, the replayer waits until the
sandbox's own facts for those keys equal the recorded values, then for a
**settle time** (reference default 1 second) so that the init system and
kernel can act on them, and then sends the next `step`. A change with no
actuator, or whose facts do not reach the recorded values within a timeout,
is reported as drift and replay continues.

#### Repetition

A sandbox runs a real kernel and init system, which do not run in a fixed
order with the handler. A replayer MUST therefore run a sandbox replay *R*
times, each in a fresh sandbox (reference default *R* = 3). If every run
reaches the same status, that is the status. If they differ, the status is:

| Status | Meaning |
| --- | --- |
| `unstable@N` | Runs of the same sandbox replay reached different statuses; `N` is the earliest `seq` at which any of them stopped. |

`unstable` is checked after all others, and only in sandbox replays.

#### Verifying a fix to the environment

A fix to the environment is an **environment patch**: a JSON object from key
to fact, in the form of `ready.environment` (§9.2), that overrides or adds
facts of the genesis environment, for example:

```json
{
  "host.mount./mnt/global_shm": {"value": "dG1wZnMgcncsc2l6ZT0xRw=="},
  "env.SHM_DIR": {"value": "L21udC9nbG9iYWxfc2ht"}
}
```

A patch MAY set `env.` facts that the application reads to find local
resources; it MUST NOT set facts with form `1`. It is verified as in §6.1,
with these differences: the old and new builds are the same binary; the
**old** replay runs in a sandbox of the genesis environment and the **new**
replay in a sandbox of the patched one, both at the same level; and a variant
reproduces the incident only if the old replay fails as recorded in every one
of its *R* runs.

Variants of an incident whose failure follows an `environment` change also
perturb that change: moved one step earlier, moved one step later, repeated
at a later step boundary. A fix that only works when the logout arrives at
the recorded step fails one of them. A code fix can be verified in a sandbox
too; then the old and new builds differ and both replay in the same sandbox.

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

Before 1.0 (major `0`), every minor version is its own format: any 0.x may
change or remove anything in any other 0.y, with no migration path. A reader
MUST reject a file whose `minor` is not one it implements, and a reader that
implements one minor need not read any other. Conformance fixtures are
regenerated on every 0.x change.

0.2 changes the file layout: a file is a Zstandard stream of skippable frames
and zstd frames, compressed by default (§3), and records are framed with a
`uvarint` length and no checksum of their own (§3.4). It adds continuous
recording by a separate recorder process, with segments and fixtures (§3.6,
§10), the `gateway`, `environment` and `config` records (§4.7–§4.9), the
`crash`, `exit` and `dropped` markers, `scope` on `output`, environment drift
(§6.2), sandbox replay (§6.3) and the host protocol (§9).
0.1 files cannot be read as 0.2.

## 8. Conformance

The conformance suite lives in [`spec/testdata/`](spec/testdata): each file in
`valid/` is paired with a `.json` file holding the expected decoding (byte
fields base64-encoded, record types by name), and every file in `invalid/` MUST
be rejected. The Go implementation regenerates the suite with
`go test ./journal -update`. Any implementation, in any language, that decodes
every valid file to the expected JSON and rejects every invalid one conforms
to this version of the format.

From 0.2 every file in the suite is a Zstandard stream, and the suite adds
valid journals with several blocks, compressed and uncompressed, with an
unknown skippable frame between blocks, and ending inside a block; and
invalid ones that do not begin with a header frame, with a bad `header_crc`
on a block, with a data frame that fails its content checksum, lacks a
content size or decodes to the wrong length, holding the wrong number of
records, with a record spanning two blocks, and with a `first_seq` that does
not continue the previous block. Every valid file that does not end inside a
block also passes the reference `zstd` tool's integrity check (`zstd -t`). It also adds valid journals with `gateway` records (including one
with a non-empty `error`), with `config` records, and with a genesis
`environment` record using every `form` followed by change records; and
invalid ones whose `gateway` or `environment` record lacks the critical flag,
whose `environment` record repeats a key, or whose first `environment` record
comes after the first `input`.

A host (§9) conforms if it passes the transcripts in `spec/host/` (§9.6). An
SDK's recorder half and `kavach-recorder` conform as §10.6 describes.

## 9. Host protocol

The `kavach` CLI is the only replayer and the only verifier. A service in any
language takes part by acting as a **host**: a process that runs its own
handler one step at a time while the CLI, the **driver**, serves every read and
captures every output over a pipe. The rules of §6 and §6.1 (lenient reads,
status order, output comparison, variants) are implemented once, in the
driver, and an SDK only implements this section.

### 9.1 Starting a host

The driver is given a **host command**: an argument vector, such as
`./ledger` or `python -m ledger` or `node dist/main.js`. As a single string it
is split into words like a POSIX shell would, with quotes honoured and no
expansion. The driver runs the command with one more argument appended,
`kavach-host`, and speaks the protocol on the process's standard input and
output. Standard error is free for logs; the driver MAY show it on failure.

The driver starts the host with the journal's environment variables, not its
own: every `env.` fact of the genesis environment recorded with form `0`,
exactly as recorded. A variable recorded with form `1` is set to the driver's
own value of it, if it has one, and its drift is reported (§6.2); a variable
recorded with form `2` is unset. Variables the journal does not mention are
not passed, except that the driver MAY pass `PATH` and `HOME` from its own
environment when the journal has none, and reports that as drift. Code
that reads an environment variable at startup, before the handler exists,
therefore sees what it saw in production.

In a sandbox replay (§6.3) the driver stays outside the sandbox and starts the
host command inside it, carrying the protocol over the sandbox's console or a
virtual socket. The messages are the same.

A program that sees `kavach-host` as its last argument MUST act as a host and
nothing else: no consuming, no serving, and no live effects except local ones
in `sandbox` mode. Nothing but protocol
messages may be written to standard output. An SDK SHOULD take the protocol
stream for itself when it starts and point the language's standard output at
standard error, so that a handler that prints cannot corrupt it.

### 9.2 Messages

Each message is one JSON object on one line, terminated by `\n`, encoded as
UTF-8. Every message has a string field `t`, its type. Fields of type bytes are
base64 (RFC 4648 §4, padded). Integers that can exceed 2<sup>53</sup> (`seq`,
`unix_nanos`) are JSON strings of decimal digits. Receivers MUST ignore fields
they do not know.

From driver to host:

| `t` | Fields | Meaning |
| --- | --- | --- |
| `hello` | `protocol` (number, `1`), `service`, `start`, `snapshot` (bytes, only when `start` is `"snapshot"`), `mode` (`"process"` or `"sandbox"`) | Create a fresh handler, restore the snapshot if any. In `sandbox` mode, run local setup first (§6.3). |
| `step` | `seq`, `source`, `position`, `data` (bytes) | Run one step on this input. |
| `clock` | `unix_nanos` | Answer to a `clock` request. |
| `rand` | `data` (bytes) | Answer to a `rand` request, exactly `n` bytes. |
| `gateway` | `response` (bytes) or `error` (string), or `live` (`true`) | Answer to a `gateway` request. `live` tells the host to execute a local query itself (§6.3) and report the result with `observed`. |
| `config` | `present` (boolean), `value` (bytes, when present) | Answer to a `config` request. |
| `abort` | `detail` | Replay of this step has stopped; see §9.4. |
| `end` | — | No more steps. The host exits with status 0. |

From host to driver:

| `t` | Fields | Meaning |
| --- | --- | --- |
| `ready` | `protocol` (number, `1`), `sdk` (e.g. `"kavach-python/0.1.0"`), `invariants` (array of names, in check order), `environment` (object, see below) | Answer to `hello`. |
| `clock` | — | The handler read the clock. |
| `rand` | `n` (number, at least 1) | The handler read `n` random bytes. |
| `gateway` | `gateway`, `request` (bytes), `scope` (`"remote"` or `"local"`) | The handler queried a gateway. |
| `observed` | `response` (bytes) or `error` (string) | The live result of a query answered with `live`. Not answered. |
| `config` | `key` | The handler read a config value. |
| `emit` | `sink`, `data` (bytes), `scope` (`"remote"` or `"local"`) | The handler produced an output. Not answered. In `sandbox` mode the host delivers local outputs after the step succeeds. |
| `done` | `outcome`, `message`, `detail` | The step finished; see below. |
| `fatal` | `message` | The host cannot continue (the handler could not be created, the snapshot could not be restored, a malformed message). |

`done.outcome` is `"ok"`, `"panic"`, `"error"`, `"invariant"`, or
`"aborted"` after an `abort` (§9.4). For `panic` and `error`, `message` is the
marker message the SDK would have recorded (§4.5) and `detail` MAY carry a
stack trace. `message` is omitted for `ok` and `aborted`, and `detail` is
omitted when empty. For `invariant`, `message` is the
name of the first declared invariant, in `ready.invariants` order, that failed
after the step, and `detail` its failure. A host checks invariants only after
a step that ended `ok`, and reports `ok` only if all of them hold.

`ready.environment` is the replay machine's environment, as an object from key
to fact: `{"value": <bytes>}`, `{"sha256": <bytes>}` or `{"unset": true}`. The
host gets the `env.` and `host.` facts by running `kavach-recorder facts`,
which prints them in this form, as one JSON object on one line, on its
standard output, collected by the same
code the recorder uses for a genesis environment (§4.8); it adds
`host.runtime` itself. If `kavach-recorder` cannot be found, the host sends
only `host.runtime`, and the driver treats the missing keys as not
collected. It is what the driver compares for drift (§6.2). The host
MUST NOT collect `flag.` facts from a live flag service; the driver treats
them as not collected rather than as drift.

### 9.3 A session

```
driver → host   {"t":"hello","protocol":1,"service":"ledger","start":"genesis"}
host → driver   {"t":"ready","protocol":1,"sdk":"kavach-python/0.1.0","invariants":["balance_non_negative"]}
driver → host   {"t":"step","seq":"0","source":"kafka:wallet-events","position":"0:0","data":"eyJ0eXBlIjoi..."}
host → driver   {"t":"clock"}
driver → host   {"t":"clock","unix_nanos":"1759752000000000000"}
host → driver   {"t":"gateway","gateway":"fx-rates","request":"eyJwYWlyIjoi..."}
driver → host   {"t":"gateway","response":"eyJyYXRlIjoi..."}
host → driver   {"t":"emit","sink":"postgres:balances","data":"eyJ3YWxsZXQi..."}
host → driver   {"t":"done","outcome":"ok"}
  … one step per input …
driver → host   {"t":"end"}
```

Within a step the host sends requests one at a time, each `clock`, `rand`,
`gateway` and `config` request waiting for its answer (for a query answered
`live`, until the host has sent its `observed`), in the order the handler made them.
The driver answers by the rules of §6, applies the step's checks when `done`
arrives, and either sends the next `step` or stops.

### 9.4 Aborts

When a request cannot be served without nondeterminism (§6, rule 3), the
driver answers it with `abort` instead. The replay's status is then
`nondeterministic@N` whatever the host does next. The host SHOULD unwind the
step in a way handler code is unlikely to catch (a `BaseException` subclass in
Python, a panic in Go), and SHOULD send no further requests before the step's
`done`, whose outcome is `"aborted"`. The driver answers any requests that
arrive with `abort`.

### 9.5 Failures of the host

The driver MUST apply a timeout to each step; the reference default is 10
seconds. A step that times out, or a host that exits or closes its output
during a step, makes the status `still_failing@N` for that step, with a
`detail` saying which. A host that exits during a step fails with the failure
string `"crash"`, so that a replay of a step recorded with a `crash` marker
reproduces it (§6.1). A step that times out fails with the failure string
`"timeout"`; no recorded failure has that string, so a timeout never
reproduces a recorded failure. The driver kills a host that timed out. A host
that exits before `ready`, sends `fatal`, or sends a message that is not valid
for the protocol state makes the replay fail to run, as a missing or
unreadable fixture does; it produces no status.

### 9.6 Host conformance

Every SDK implements the same **conformance handler**, so that one set of
transcripts tests them all. Its state is a count, starting at 0, to which it
adds 1 at the end of every step that does not fail; its snapshot is the count
in decimal ASCII; it declares one invariant, `below_limit`, which fails once
the count reaches 1000. For each input it parses `data` as a JSON array of
operations and performs them in order, stopping at the first that fails the
step. Like a real handler, it does not roll back: a `count` operation takes
effect immediately, even if a later operation fails the step; only the 1 added
at the end of the step depends on the step not failing. Everything it emits is
exact bytes: JSON it emits is compact (no whitespace) with keys in the order
shown, its strings escape `"` as `\"`, `\` as `\\`, newline, carriage return
and tab as `\n`, `\r` and `\t`, other characters below U+0020 as `\u00xx`
(lowercase hex), and nothing else, so other characters, `<`, `>` and `&`
included, appear as UTF-8. Values read from the world are emitted as they are,
with no encoding. All its gateways are remote.

| Operation | Effect |
| --- | --- |
| `{"op":"clock"}` | Reads the clock and emits `{"clock":<unix_nanos as a string>}` to sink `trace`. |
| `{"op":"rand","n":N}` | Reads `N` random bytes and emits them to sink `trace`. |
| `{"op":"gateway","gateway":G,"request":R}` | Queries gateway `G` with `R` (UTF-8). Emits the response to sink `trace`, or `{"error":<error>}` if it failed. |
| `{"op":"config","key":K}` | Reads config value `K` and emits it to sink `trace`, or `{"unset":true}`. |
| `{"op":"getenv","name":V}` | Reads environment variable `V` directly from the process and emits it to sink `trace`, or `{"unset":true}`. Tests §9.1. |
| `{"op":"emit","sink":S,"data":D}` | Emits `D` (UTF-8) to sink `S`. |
| `{"op":"panic","message":M}` | Fails the step as a `panic` whose marker message is exactly `M`. The SDK raises it through whatever mechanism its §4.5 mapping turns into `M` unchanged, for example `panic(M)` in Go or a dedicated panic exception class in Python. |
| `{"op":"error","message":M}` | Fails the step as an `error` whose marker message is exactly `M`, raised through the SDK's documented way of returning an error. |
| `{"op":"print","text":T}` | Writes `T` and a newline to the process's standard output by the language's ordinary means (`print`, `console.log`, `printf`, `println!`). Emits nothing. |
| `{"op":"count","n":N}` | Adds `N` to the step count. |

`spec/host/` holds transcripts: JSON Lines files of the driver's messages and
the host messages expected in reply, in the format its `README.md` describes,
and a runner that plays them against any host command. A host conforms if, run with its
conformance handler, it produces every transcript's expected messages, compared
as JSON values, ignoring `ready.sdk`, `ready.environment` and `done.detail`.
The transcripts cover every operation, a snapshot start, each `done` outcome,
an `abort`, and a handler that writes to standard output. A transcript MAY
name environment variables the host is to be started with.

## 10. Recorder protocol

The SDK inside a service and `kavach-recorder` are the two ends of the flight
recorder's pipe (§3.6). This section is the contract between them. An SDK
implements its half in each language; the recorder is written once.

### 10.1 Starting the recorder

The SDK starts the recorder as a child process before the handler consumes
its first input. It finds the executable in the `KAVACH_RECORDER` environment
variable or, failing that, on `PATH`. It connects three streams:

| Stream | Direction | Carries |
| --- | --- | --- |
| standard input | SDK → recorder | the **record stream** (§10.2), binary |
| standard output | recorder → SDK | the **control stream** (§10.3), JSON Lines |
| standard error | recorder → inherited | the recorder's own logs |

The recorder collects the `env.` facts of the environment (§4.8) from its own
environment variables, which it inherits from the service. The SDK MUST
therefore start it with the process's environment unchanged. It collects
`host.` facts itself, since it runs on the same host, as the same user, in the
same mount namespace and cgroup.

The SDK SHOULD wait for `ready` (§10.3), up to a bounded time (a couple of
seconds), before its first step, so that it never fills the ring or the pipe
before the recorder reads it. If the time passes, it goes on.

The recorder MUST outlive the service long enough to finish the journal. It
MUST ignore `SIGINT`, `SIGTERM`, `SIGHUP` and `SIGPIPE`, and SHOULD start a
session of its own, so that a signal sent to the service's process group does
not stop it before it has read the pipe to the end. It exits when the record
stream ends and the journal is finished (§10.5). A `SIGKILL` sent to every
process in the service's cgroup still stops it; §11 discusses this.

If the recorder cannot be started, or later reports a fatal error or stops
reading, the SDK MUST say so on the service's own log and stop recording. It
MUST NOT fail the step it is in. It MAY refuse to start the service when the
application asks for recording to be required. It MUST NOT continue without
recording silently.

An SDK acting as a host (§9) starts no recorder.

### 10.2 Record stream

The record stream is a sequence of frames:

| Field | Type | Meaning |
| --- | --- | --- |
| len | `uvarint` | length of `kind` and `payload` together |
| kind | `u8` | see below |
| payload | `len − 1` bytes | per `kind` |

| Kind | Name | Payload | Meaning |
| --- | --- | --- | --- |
| `0x01` | `open` | UTF-8 JSON object, see below | First frame, exactly once. |
| `0x02` | `record` | a record `body` (§3.4) without its `seq`: `type` (`u8`), `flags` (`u8`), `payload` | One record. The recorder assigns its `seq`. |
| `0x03` | `step_end` | empty | The current step is over, including its `marker`, if any. |
| `0x04` | `facts` | an `environment` record payload (§4.8) | Facts only the SDK can collect: `flag.` keys, `host.runtime`, and any `host.x.` keys it adds. The recorder merges them into the environment. |
| `0x05` | `snapshot` | `bytes`: the handler's state | Answer to `snapshot_request` (§10.3), or the journal's starting state (§10.4). |
| `0x06` | `flush` | `u8`: `1` to make it durable, `0` not to | Close the open block now. When `1`, also make it durable and answer with `durable`. |
| `0x07` | `close` | empty | Orderly shutdown (§10.5). |

The `open` object:

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `protocol` | number | required | `1`. |
| `service` | string | required | As in the header (§3.2). |
| `start` | string | required | `"genesis"` or `"snapshot"`. |
| `producer`, `handler` | string | — | As in the header. |
| `snapshots` | boolean | `false` | Whether the SDK will answer `snapshot_request`. An SDK sends `true` when the handler is a `Snapshotter`, unless the application turns segments off. Without it, the recorder never starts a new segment. |
| `dir` | string | `"kavach"` | Directory for journal segments. Fixtures go in its `fixtures/` subdirectory. |
| `compression` | string | `"zstd"` | `"zstd"` or `"none"` (§3.3). |
| `level` | number | `3` | zstd compression level. |
| `block_bytes` | number | 4 MiB | Target raw size of a block. |
| `flush_ms` | number | `1000` | Flush interval (§3.6). |
| `segment_bytes`, `segment_seconds` | number | 256 MiB, 3600 | When to start a new segment. |
| `retain_segments` | number | `24` | Segments to keep per service, oldest deleted first. Fixtures are never deleted by the recorder. |
| `secret_keys` | array of strings | `[]` | Environment variable names to hash (§4.8) besides those the spec requires. |
| `ring` | number | — | Capacity of the shared-memory ring that carries every later frame (§10.7). |
| `ring_path` | string | — | Name of the ring file, when the SDK cannot pass it as descriptor 3 (§10.7). |

The frames after `open` follow the shape of the journal (§5). An `input`
record begins a step; the step's reads, outputs and `marker` follow as
`record` frames; `step_end` ends it. Between steps, only `facts`, `snapshot`,
`flush`, `close` and `record` frames holding a `marker` of kind `trigger` or
`dropped` may appear. The SDK never sends `environment` records: the recorder
writes them. A recorder that receives a frame out of place, an unknown
`kind`, or a malformed payload MUST report a fatal error (§10.3) and finish
the journal up to the last complete step.

**Writing.** The SDK SHOULD write the `input` frame of a step as soon as it
has it, before calling the handler, and MAY gather the rest of the step's
frames and write them together with its `step_end`. Then a step that ends the
process without unwinding (a segmentation fault, an abort, the process killed
for memory) still leaves its input on record, and the recorder marks it
(§10.5). The SDK SHOULD enlarge the pipe's buffer where the platform allows
it (on Linux, `F_SETPIPE_SZ`, to 1 MiB).

### 10.3 Control stream

The recorder writes JSON Lines to its standard output, one object per line,
each with a string field `t`. The SDK reads them on a thread of its own and
MUST NOT make a step wait on them.

| `t` | Fields | Meaning |
| --- | --- | --- |
| `ready` | `protocol`, `recorder` (e.g. `"kavach-recorder/0.2.0"`), `run`, `file` | Sent after `open`. `file` is the first segment's path. |
| `snapshot_request` | — | The recorder wants to start a new segment. |
| `durable` | `seq` (string) | Every record up to `seq` is durable. Sent once in answer to each `flush` with `1`, in the order the flushes arrived, and at no other time, so an SDK pairs answers with its flushes by counting. |
| `segment` | `file`, `first_seq` (string) | A new segment was started. |
| `fixture` | `file`, `seq` (string), `failure` | A fixture was written for the failure at input `seq`. `failure` is as in a variant's header (§3.2). |
| `error` | `message`, `fatal` (boolean) | Something went wrong. After a fatal error the recorder finishes the journal and exits; the SDK stops recording (§10.1). |
| `closed` | — | Answer to `close`. The recorder exits after sending it. |

The SDK SHOULD log every `fixture` message on the service's own log, so that
an operator, or an agent reading the logs, sees where the fixture of a failure
is.

### 10.4 Environment, snapshots and segments

The recorder writes a journal's header and genesis `environment` (§4.8) when
it receives the first `record` frame holding an `input`, or the first
`snapshot` frame, whichever comes first. The environment holds the `env.` and
`host.` facts it collected and every fact the SDK sent in `facts` frames
before then, so an SDK SHOULD send `host.runtime` and its `flag.` facts right
after `open`.

When `start` is `"snapshot"`, the SDK's first frame after `open` and any
`facts` frames MUST be a `snapshot` holding the state the handler starts from.

The recorder watches `host.` facts while the service runs. It writes change
records only right after a `step_end`, merging changes it observed and
`facts` frames received since the last one.

To start a new segment, the recorder sends `snapshot_request`. The SDK
answers at the next step boundary, after a `step_end` and before the next
`input`, with a `snapshot` frame taken at that point, on the step's own
thread. The recorder ends the current segment there and starts the next one
with the snapshot and the full environment (§3.6). Until the answer arrives,
recording continues in the current segment.

### 10.5 Ending

- **`close`.** The recorder writes what remains, makes it durable, answers
  `closed` and exits with status 0. An SDK SHOULD send `close` when the
  service shuts down, and wait for `closed` for a bounded time (reference
  default 10 seconds).
- **The record stream ends inside a step** (after an `input`, with no
  `step_end`). The service died during the step. The recorder appends a
  `marker` of kind `crash` with message `"process ended during step"` as the
  step's marker, writes a fixture for it, and makes both durable. That step
  becomes the journal's recorded failure, with the failure string `"crash"`.
- **The record stream ends between steps** without `close`. The service
  stopped without shutting down, for example killed while idle. The recorder
  appends a `marker` of kind `exit` with message `"process ended without
  closing the recorder"`, which is not a failure, and finishes the journal.

### 10.6 Recorder conformance

The two halves are tested separately, so a failure is found in one half and
fixed once.

- **SDK half.** `spec/recorder/sdk/` holds cases: inputs for the
  conformance handler (§9.6), the clock and random values and gateway and
  config answers to give it, and the frames the SDK is expected to write,
  decoded as JSON, in the format its `README.md` describes. It also holds a
  fake recorder that an SDK is started with instead of `kavach-recorder`,
  which checks the frames it receives against the case. An SDK conforms if it
  passes every case.
- **Recorder half.** `spec/recorder/journal/` holds record streams, each
  paired with the journal and fixtures that `kavach-recorder` must write for
  it, decoded as JSON (§8). For this, the recorder has a test mode in which it
  takes its `env.` and `host.` facts, `run` and `recorded_at` from a file
  instead of from the host, so its output is deterministic. The pairs cover
  each frame kind, a segment change, each way of ending, and both
  compressions. Journals are compared decoded, never byte for byte, since
  compressed bytes depend on the zstd version.

### 10.7 Shared-memory transport

An SDK MAY carry the record stream over a shared-memory ring instead of the
pipe, so that recording a step costs a few memory copies and no system call.
The frames are those of §10.2; only their transport changes. A recorder MUST
support both transports, and the SDK chooses.

**Setting up.** Before starting the recorder, the SDK creates a memory-backed
file of `65536 + capacity` bytes, where `capacity` is a power of two of at
least 64 KiB (reference default 8 MiB). Where the platform has anonymous
memory files it uses one (`memfd_create` on Linux), so the file never has a
name; otherwise it creates the file in a memory-backed file system
(`/dev/shm`) or the temporary directory and unlinks it at once. It maps the
file shared and passes it to the recorder as file descriptor 3; standard
input, output and error are connected as in §10.1. Because the file has no
name, nothing can remove it from under the two processes, and nothing is left
behind when they exit. The SDK writes the `open` frame on standard input as
before, with the key `ring` set to `capacity`. Every later frame goes into the
ring. A recorder that cannot map the ring, or finds the header invalid, MUST
report a fatal error (§10.3).

An SDK whose runtime cannot pass a descriptor to a child process instead keeps
the file's name, creating it readable and writable by its own user only, and
sends that name in the `open` key `ring_path` besides `ring`. The recorder
opens the file, unlinks it, and maps it before reading the ring. If the
recorder never starts, the SDK unlinks the file itself.

**Layout.** Integers are little-endian `u64`s at fixed offsets, `write` and
`read` on cache lines of their own. The data area starts at 65536, a multiple
of every page size in use, so that a process MAY map it twice, back to back
(at `d` and at `d + capacity`), and read or write any run of at most
`capacity` bytes as one contiguous copy however it wraps:

| Offset | Field | Written by | Meaning |
| --- | --- | --- | --- |
| 0 | magic | SDK | The 8 ASCII bytes `KVRING02`. |
| 8 | capacity | SDK | Size of the data area in bytes. |
| 64 | write | SDK | Bytes of the record stream published so far. |
| 128 | read | recorder | Bytes of the record stream consumed so far. |
| 65536 | data | SDK | Byte `i` of the record stream (counting from the first frame after `open`) is at `65536 + i mod capacity`. A frame may wrap. |

The SDK initializes the header before starting the recorder. Neither process
writes the other's field.

**Publishing.** To publish `n` bytes, the SDK checks that `n ≤ capacity −
(write − read)`, copies them into the data area at `write`, and then stores
`write + n` into `write` with release ordering. The recorder loads `write`
with acquire ordering, decodes the frames in `[read, write)`, and stores the
new `read` with release ordering once it no longer needs their bytes. Bytes
are visible to the recorder only once `write` covers them, so a service that
dies while copying a frame leaves none of it. The SDK SHOULD publish a step's
`input` frame on its own before calling the handler, as §10.2 asks of the
pipe; over the ring this costs no system call. A frame larger than the whole
ring is published in pieces as space frees up. If the record stream ends
inside a frame, over either transport, the recorder discards the partial frame
and handles the end as §10.5 says. A recorder that finds `write − read`
greater than `capacity`, or `write` moving backwards, MUST report a fatal
error.

**Waking.** The recorder polls the ring, at an interval of its choosing no
longer than 10 ms. Standard input carries no frames after `open`; it is a
doorbell. The SDK writes one byte of any value to it after publishing a
`flush` or `close` frame, and when it finds the ring more than half full. A
recorder that reads from standard input drains the ring at once.

**When the ring is full**, the rule for a full pipe (§3.6) applies: the SDK
rings the doorbell and waits for space, or drops records and publishes a
`dropped` marker. If the recorder has exited, the SDK stops recording
(§10.1).

**Ending.** End of standard input still means that the service has ended
(§10.5). The mapping outlives the service, so on end of input the recorder
first drains the ring up to `write`, then applies §10.5: a step whose `input`
was published before the service died ends with a `crash` marker, exactly as
over the pipe.

**Conformance.** The cases of §10.6 apply to both transports. An SDK that
offers the ring passes the SDK cases over it, and a recorder passes the
recorder cases over both.

## 11. Open questions

- **Starting mid-stream** — *resolved in 0.1 by the `snapshot` record (§4.6).*
  Referencing the service's own durable log by position was rejected: the
  fixture would no longer replay on its own, without access to that log.
- **External queries** — *resolved in 0.2 by the `gateway` record (§4.7).*
  Recording at the gateway's domain interface, after translation, was
  rejected: bugs in translating an upstream response would not replay.
  Recording network traffic was rejected: it is bulky, tied to a transport,
  and too low-level to mutate meaningfully in variants.
- **Self-replay.** Go binaries can also replay a fixture themselves
  (`<bin> kavach-replay <fixture> <out.json>`), which the CLI uses today. Once
  the Go SDK speaks the host protocol, the CLI should use it for every
  language, and self-replay becomes a Go convenience outside this spec.
- **Concurrency inside a step.** 0.2 assumes the handler makes its reads from
  one goroutine or thread at a time, or at least issues them in a
  deterministic order. Concurrent queries whose issue order depends on
  scheduling would make replay nondeterministic.
- **Sensitive data.** Inputs and gateway responses are production data. A
  fixture that is to be shared or committed needs a way to redact fields at
  record time without breaking replay; 0.2 defines one only for secrets in the
  environment (§4.8).
- **Recreating the host** — *addressed in 0.2 by sandbox replay (§6.3).*
  Open: which kernels a replayer must be able to boot, how far "nearest
  kernel" may be from the recorded one before a `vm` replay means little, and
  whether the settle time should be derived per actuator rather than fixed.
- **Platforms.** The `vm` level and its actuators are defined for Linux with
  systemd. Other init systems, macOS and Windows hosts need their own facts
  and actuators.
- **Local state outside the journal.** A sandbox starts empty apart from local
  setup. Local state the process built before the journal's first record,
  such as a shared memory segment filled over days, is not in the journal; a
  snapshot (§4.6) holds only the handler's state.
- **Watching the host.** Which facts can be watched cheaply, and how promptly
  a change must be recorded, is per platform. A change recorded a step late
  still precedes the failure it explains, but may be recorded after it.
- **Flags evaluated outside the handler.** `flag.` facts record what a flag
  would have returned for the process. Flags evaluated per user or per request
  depend on input data; only `config` reads capture the value actually used.
- **Reporting compensations.** When a fix changes the outputs of the failing
  step, the difference between the old and new build's outputs there is what
  production should be told: updates the old build would have made that need
  undoing, and updates the new one adds. `diff` could report this set.
- **Large inputs.** The 16 MiB record limit may be too small for some batch
  events and gateway responses.
- **Losing the tail.** Records the SDK has written into the pipe survive the
  service being killed (§10.5), but records it was still gathering for the
  current step do not, and a `SIGKILL` sent to every process in the cgroup
  (as `systemd` and container runtimes do on a hard stop) kills the recorder
  too, losing its open block. Running the recorder outside the service's
  cgroup, as a node-level daemon receiving the pipe, would close this gap at
  the cost of deployment.
- **Pipe cost.** Each step costs the SDK one or two writes into the pipe. For
  handlers whose steps take microseconds, a shared-memory ring between SDK
  and recorder may be needed; it would replace the record stream's transport,
  not its frames.
- **Cost of continuous recording.** Writing every record of every step, rather
  than keeping a window in memory, costs disk bandwidth and storage in
  proportion to traffic. The compression ratio of real journals, and the
  retention to default to, need measuring.
- **Snapshots within a segment.** Fixtures are cut from the start of a
  segment, so a fixture can hold up to a whole segment of history before the
  failure. Allowing snapshots mid-segment would make fixtures shorter at the
  cost of a more complex reader.
