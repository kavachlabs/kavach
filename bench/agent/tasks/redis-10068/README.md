# Pilot task A1: redis__redis-10068

The first task of the agent benchmark (track A, incident fixes; see
`../../sourcing/selection.csv`). It checks that the pipeline works on a real
service before any agent runs: Redis records the incident, Kavach replays it,
and `kavach diff` judges candidate fixes the same way the hidden upstream test
does. **No agent has been run on it yet.**

| | |
| --- | --- |
| Source | SWE-bench Multilingual, `redis__redis-10068` (BSD-3-Clause) |
| Base commit | `e88f6acb94c77c9a5b81f0b2a8bd132b2a5c3d3c` (2022-01-07) |
| Bug | `streamTrim` reads the ID delta from the node's master entry into an `int`. When an entry's millisecond delta exceeds 2³¹ (an old explicit ID such as `10-1` followed by auto-generated IDs), the delta is truncated, and XTRIM MINID deletes entries at or above the threshold. |
| Hidden test | `XTRIM with MINID option, big delta from master record` plus the seven other XTRIM tests (`hidden/test.patch`, from the SWE-bench record) |

## Results

`./run.sh` reproduces everything below from a clean checkout, in about 40–55 s
on an Apple M3 Pro (macOS, Tcl 8.5), most of it compiling Redis five times.
Measured 2026-10-10 at Kavach `4c0e896`.

**Recording.** The traffic in `traffic.sh` follows the report: one entry with
the ID `10-1`, twelve with `*`, readers (`XRANGE`, `XLEN`), and a retention job
that runs `XTRIM orders MINID <6th auto ID>`. Against the live server, that
XTRIM replied `13` and emptied the stream, and the recorder wrote a fixture
flagged by the `stream-trim-contract` invariant. `incident.kavach` (1.7 KB, 21
steps) is the fixture `run.sh` recorded, committed so every agent gets the same
one.

The recorder captures the server's environment (SPEC §4.8), so `run.sh` starts
the server with a minimal environment, relative paths and `TZ=UTC`. The
fixture still holds the facts the recorder reads from the OS: `host.user`,
`host.hostname`, `host.uid`, the CPU count and ulimits. A fixture meant to be
shared needs those recorded in a container, or redacted by the recorder.

**Replay.** On the old build the fixture replays as
`invariant_violated(stream-trim-contract)@73`, every time.

**Candidate fixes.** Each is judged by `kavach diff` against the fixture and by
the hidden test, separately:

| Candidate | `kavach diff` | Variants that reproduce on the old build / pass on the new | Hidden test |
| --- | --- | --- | --- |
| (old build) | — | — | fail |
| `upstream`: the merged fix (`int64_t` for the delta, seq delta, flags and lp-count) | `fixed` | 49 of 64 / 49 | pass |
| `ms-delta-only`: only the millisecond delta made `int64_t` | `fixed` | 49 of 64 / 49 | pass |
| `split-node-on-big-delta`: never store a big delta; start a new node instead | `fixed` | 49 of 64 / 49 | pass |
| `stop-on-negative-delta`: stop trimming when the delta is negative | `invariant_violated` | — | fail |

Kavach and the hidden test agree on all five builds, including the one where
both are wrong (below).

## What this does and does not show

- **A narrow fix passes both graders.** `split-node-on-big-delta`, written by
  the verifier, stops new big deltas from being written but leaves trimming as
  it was. Streams written before the fix still hold big deltas. A stream saved
  by the old build and loaded by this one still loses every entry to
  `XTRIM MINID`: 3 removed, 0 left, where the upstream fix removes 1 and keeps
  2.
  - The hidden test misses it: it builds its stream from scratch.
  - Kavach misses it too. Variants change inputs, the clock and the order of
    steps, but they never start from state saved by another build. That is the
    `filesystem-state` gap in the sourcing ledger.
- **`ms-delta-only` is not narrow.** The other fields the upstream fix widened
  cannot overflow in practice.
- **Variants confirmed fixes and caught nothing here.** The wrong fix is wrong
  on the recorded input itself.
- **The invariant was written by us, for this kind of command.**
  `stream-trim-contract` states the documented contract of stream trimming,
  for XTRIM and XADD, MINID and MAXLEN, exact and approximate. It does not
  mention ID deltas. But it exists because this incident is a trimming bug, and
  an operator would have to write it before the incident to catch it live.
  - It was extended once during the pilot. After the first candidates were
    built, the exact-trim direction (nothing below MINID survives an exact
    trim; at most MAXLEN entries remain) was added, so that a fix that stops
    trimming could not pass.
  - It was then corrected once: it had read the node's master ID, which can
    be a deleted entry, and it wrongly flagged the upstream fix.
  - After review it was corrected again:
    - It no longer fires when the command replied with an error; that was a
      false positive on correct Redis.
    - It reads its key after the step without the expiry check, which had
      read the real clock and could delete a key outside any step.
    - It parses trimming options in any order, as Redis does.
  - The fixture and all results above were recorded after these changes.
- **The candidates were written by us,** not by an agent.

## The integration (`redis-kavach.patch`)

About 570 lines of C in the new `src/kavach.c` and `src/kavach.h`, plus 12
changed lines of hooks in `server.c` and 8 in the Makefile. It links the C SDK
(`sdk/c`). No line of the code under repair changes.

- **Steps.** Each command a network client sends that reaches `call()` is a
  step. Its input is `{"argv":[...]}`, with `"db":N` when the client has
  selected a database other than 0. Its output is the RESP2 reply.
- **Shadow client.** The command runs on a shadow client, as `RM_Call` runs
  module calls, both live and in replay, so recording and replay execute the
  same code. The real client then gets the shadow's reply.
- **Clock.** `ustime()`, and through it `mstime()` and the cached server time,
  reads the Kavach clock during a step.
- **Recording** starts when `KAVACH_REDIS_DIR` is set and the dataset is empty
  at startup. It is closed at exit.
- **Replay.** `redis-server … kavach-host` serves the host protocol, with no
  listener and no persistence, and flushes all data at each `hello`.

**Known limits.** Commands outside the step model run as usual but are not
recorded:
- transactions (`MULTI`/`EXEC`);
- blocking commands;
- `SELECT`, `CLIENT` and other connection commands;
- admin commands, including `CONFIG SET` and `DEBUG`;
- pub/sub;
- RESP3 clients.

Other limits:
- `random()`, the monotonic clock, the LRU clock and the dict hash seed are not
  routed. Replies whose order depends on hashing, such as `KEYS` and `SMEMBERS`,
  can diverge on replay.
- Server config is not served from the journal.
- The shadow client is what slowlog, `MONITOR` and `CLIENT TRACKING` see, not
  the real client.
- While recording, the invariant walks every entry at or above a MINID
  threshold twice per trimming command. That is O(stream length) per
  `XADD … MINID`, which suits a benchmark but not production.

With recording on, Redis's own `unit/type/stream` suite passes and wrote 11
journals. 6 replay as `ok`, and 5 diverge.
- 1 diverges at `INFO`, which reports live process state.
- 4 diverge at stream commands whose results depend on state the integration
  does not record. The suite sets `stream-node-max-entries` with `CONFIG SET`
  and in startup overrides, and uses `MULTI`/`EXEC` and `DEBUG LOADAOF`. I
  attributed these causes by reading the suite, not by tracing each
  divergence.

The A1 traffic uses none of these.

## Files

| File | |
| --- | --- |
| `task.md` | What the agent is given. Both arms get the report. The control arm works on Redis at the base commit; the Kavach arm works on Redis with the integration, plus the fixture and the tools. |
| `redis-kavach.patch` | The integration, against the base commit |
| `traffic.sh` | The incident traffic |
| `incident.kavach` | The recorded fixture agents receive |
| `candidates/*.patch` | Candidate fixes, applied on top of the integration |
| `hidden/test.patch` | The upstream test, from SWE-bench; never shown to the agent |
| `run.sh` | Builds, records, replays and grades everything above, on the host |
| `Dockerfile` | The agent images for each arm and the grader image (see `../../harness`) |
| `grade.sh` | Grades one agent attempt in the grader image |

In the containers, the five builds grade exactly as in the table above, in both
arms.

## Not done yet

- **No model has run.** `../../harness/run.sh` runs any model on this task in
  Docker; it has only been checked against a dead endpoint so far.
- **Grading** uses this task's own grader image, not SWE-bench's
  (`swebench/sweb.eval.x86_64.redis_1776_redis-10068`). Both run the same
  hidden test.
