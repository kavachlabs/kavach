# Integrating Kavach without an SDK

Kavach is two protocols and a file format. The SDKs under [sdk](sdk) are
wrappers over the protocols; any program, in any language, that speaks them
records journals and replays fixtures exactly as an SDK does. This guide is for
an agent or developer adding Kavach to a language or application that has no
SDK.

[SPEC.md](SPEC.md) is the contract. This file tells you which parts of it you
implement, in what order, and how you know you are done. Where the two
disagree, SPEC.md wins.

## What you implement, and what you don't

| Side | Protocol | You write | Written once, for every language |
| --- | --- | --- | --- |
| Live | Recorder protocol, [§10](SPEC.md#10-recorder-protocol) | Frames into `kavach-recorder`'s standard input | Sequence numbers, blocks, compression, segments, fixtures, the `environment` record, crash markers |
| Replay | Host protocol, [§9](SPEC.md#9-host-protocol) | JSON Lines answers when started with `kavach-host` as the last argument | Serving reads, comparing outputs, verdicts, variants |

**Never write `.kavach` files yourself.** Before 1.0 every minor version of the
file format is a different format ([§7](SPEC.md#7-versioning-and-compatibility)),
and a file writer has to reimplement everything in the last column above. A
process that writes its own journal also cannot record its own death: only
the separate recorder sees the pipe close mid-step and writes the `crash`
marker ([§10.5](SPEC.md#105-ending)).

## The handler model

Kavach replays a **handler**: code that consumes one input at a time (a
**step**), folds it into state, and touches the world only through five
calls. Your integration provides those calls; the handler must use nothing
else.

| Handler does | Live: you | Replay: you |
| --- | --- | --- |
| reads the clock | read it, send a `clock` record | send `{"t":"clock"}`, return the answer |
| reads random bytes | read them, send a `rand` record | send `{"t":"rand","n":N}`, return the answer |
| queries an external system | call it, send a `gateway` record with the raw request and response | send `{"t":"gateway",...}`, return the answer; never call it |
| reads a flag or limit | read it, send a `config` record | send `{"t":"config","key":K}`, return the answer |
| produces an effect | send an `output` record; deliver it after the step succeeds | send `{"t":"emit",...}`; never deliver it |

Reads must happen in a deterministic order, one at a time
([§11](SPEC.md#11-open-questions), concurrency inside a step). Gateway records
hold the bytes at the **connection**, below any parsing, so a parsing bug
replays ([§4.7](SPEC.md#47-gateway--0x07)).

## Recording: the record stream

Start `kavach-recorder` as a child process (from `KAVACH_RECORDER`, else
`PATH`) with the process's environment unchanged
([§10.1](SPEC.md#101-starting-the-recorder)). Write frames to its standard
input; read JSON Lines from its standard output on a separate thread, and never
make a step wait on them.

Every frame is `uvarint len`, `u8 kind`, then `len − 1` bytes of payload
([§10.2](SPEC.md#102-record-stream)). Integers are little-endian; `string` and
`bytes` are a uvarint length then the bytes ([§2](SPEC.md#2-conventions)).

```
open         0x01  JSON: {"protocol":1,"service":"ledger","start":"genesis", ...}
facts        0x04  host.runtime and flag. facts, right after open
  per step:
record       0x02  input            — write it before calling the handler
record       0x02  clock | rand | gateway | config | output, in program order
record       0x02  marker           — only if the step failed
step_end     0x03
close        0x07  at shutdown; wait (bounded) for {"t":"closed"}
```

A `record` frame's payload is a record body ([§3.4](SPEC.md#34-records))
without its `seq`: `u8 type`, `u8 flags`, then the type's fields from
[§4](SPEC.md#4-record-types). The recorder assigns `seq`. Set flags to `0x01`
(critical) on `gateway`, `config` and `snapshot` records, and `0x00` on the
rest.

For example, an `input` from source `q`, position `0`, data `{}`:

```
0a           len = 10
02           kind: record
01 00        type: input, flags: 0
01 71        source   "q"
01 30        position "0"
02 7b 7d     data     "{}"
```

Rules that, if broken, make journals replay to the wrong answer:

- **Never drop a record silently.** When the pipe is full, wait; or drop and
  send a `marker` of kind `dropped` ([§3.6](SPEC.md#36-recording)).
- **Never send `environment` records.** The recorder collects them.
- **Failure markers are deterministic.** A `panic` or `error` marker's
  `message` must not contain file paths, line numbers, addresses or object
  identities; put those in `data` ([§4.5](SPEC.md#45-marker--0x05)). Replay
  compares the message.
- **Recording must not fail the service.** If the recorder cannot start or
  reports a fatal error, log it on the service's own log and stop recording
  ([§10.1](SPEC.md#101-starting-the-recorder)).

Optional, later: answer `snapshot_request` so the recorder can segment long
runs ([§10.4](SPEC.md#104-environment-snapshots-and-segments)), and use the
shared-memory ring instead of the pipe when a step costs microseconds
([§10.7](SPEC.md#107-shared-memory-transport)).

## Replaying: the host protocol

When your program's last argument is `kavach-host`, it acts as a host and does
nothing else: no consuming, no serving, no live effects
([§9.1](SPEC.md#91-starting-a-host)). Keep standard output for protocol
messages only; point the language's own standard output at standard error.

One JSON object per line, bytes as padded base64, `seq` and `unix_nanos` as
decimal strings ([§9.2](SPEC.md#92-messages)):

```
driver → host   {"t":"hello","protocol":1,"service":"ledger","start":"genesis"}
host → driver   {"t":"ready","protocol":1,"sdk":"kavach-julia/0.1.0","invariants":[],"environment":{...}}
driver → host   {"t":"step","seq":"0","source":"q","position":"0","data":"e30="}
host → driver   {"t":"clock"}
driver → host   {"t":"clock","unix_nanos":"1759752000000000000"}
host → driver   {"t":"emit","sink":"postgres:balances","data":"...","scope":"remote"}
host → driver   {"t":"done","outcome":"ok"}
driver → host   {"t":"end"}
```

Get `ready.environment` by running `kavach-recorder facts` and adding
`host.runtime`. On `abort`, unwind the step in a way handler code is unlikely
to catch and reply `done` with outcome `"aborted"`
([§9.4](SPEC.md#94-aborts)).

## Done means both suites pass

To run either suite you implement the **conformance handler**
([§9.6](SPEC.md#96-host-conformance)) in your language: a counter driven by
JSON operations, about a hundred lines.

1. **Replay:** `python3 spec/host/run.py --host "<command that starts your
   conformance host>"` passes every transcript in [spec/host](spec/host).
2. **Recording:** with [`spec/recorder/sdk/fake_recorder.py`](spec/recorder/sdk/fake_recorder.py)
   as the recorder command, every case in [spec/recorder/sdk](spec/recorder/sdk)
   writes `"pass": true`. Its [README](spec/recorder/sdk/README.md) says what
   your runner does per case. Your integration must therefore accept a
   recorder command as an option, besides `KAVACH_RECORDER`.

Do not edit the transcripts or cases to make them pass. They are generated by
`gen.py` and are the contract every SDK meets. If one looks wrong, report it.

Then check the full path once by hand: record a step that fails, find the
fixture the recorder reports (`{"t":"fixture",...}`), and run
`kavach replay <fixture> --bin "<your host command>"`. Expect
`still_failing@N`.
