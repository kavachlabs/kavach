# Task A2: redis__redis-11734

Track A (incident fixes), order 2 in `../../sourcing/selection.csv`. Built
the same way as the pilot, `../redis-10068`. **No agent has been run on it
yet.**

| | |
| --- | --- |
| Source | SWE-bench Multilingual, `redis__redis-11734` (BSD-3-Clause) |
| Base commit | `45d3310694406fb0f338f7c639cda9754edb88f8` (2023-08-21) |
| Bug | `bitposCommand` looks the key up before it parses `start`, `end` and the unit, and replies at once when the key is missing: `0` when looking for a clear bit, `-1` for a set bit. So `BITPOS missing 1 0 -1 BITS` or `BITPOS missing 0 a b` succeed, while the same command on an existing key replies `ERR syntax error` or `ERR value is not an integer`. |
| Hidden test | FAIL_TO_PASS `BITPOS will illegal arguments` and `BITPOS against non-integer value`, plus the 47 other tests of `unit/bitops` (`hidden/test.patch`, from the SWE-bench record) |
| Reference fix | `candidates/upstream.patch`: `src/bitops.c` only, 31 lines added and 19 removed in two functions |

**The report is about BITCOUNT; the bug left at the base commit is in
BITPOS.** The issue was filed against 7.0.7. Before the base commit,
upstream #12394 already moved BITCOUNT's argument parsing ahead of its key
lookup, so `BITCOUNT missing 0` replies `ERR syntax error` at the base. The
upstream PR fixed both commands; SWE-bench took the commit just before it
was merged, so only BITPOS fails. The hidden test's FAIL_TO_PASS is BITPOS
only. An agent must notice that the reported case already works and find the
same pattern in BITPOS. The task text adds one sentence to the issue for
both arms: it was filed against 7.0.7 and "this checkout is later". That
says the report may be stale, not where the bug is.

## Results

`./run.sh <work-dir>` reproduces everything below from a clean checkout, in
about 75 s on an Apple M3 Pro (macOS, Tcl 8.5), most of it compiling Redis
six times. The work directory's path must be at most 40 characters: the test
suite puts Unix sockets under it, and macOS caps their paths at 104 bytes.
Measured 2026-10-12 at Kavach `cdfafd3`.

**Recording.** `traffic.sh` is a small analytics service: one bitmap per
day, a bit per user ID. A dashboard counts yesterday's and today's bitmaps
and finds the first active user (`BITCOUNT`, `BITPOS`, with and without
ranges and units). Today's bitmap does not exist yet, so those replies are
`0` and `-1`. Then a new release of a reporting job sends the unit as
`BITS`. On yesterday's bitmap BITCOUNT and BITPOS reply `ERR syntax error`;
on today's, BITCOUNT also errors (the base already fixed it) but BITPOS
replies `-1`. The recorder flags that step with the `bit-range-syntax`
invariant. `incident.kavach` (1.7 KB, 25 steps) is the fixture `run.sh`
recorded, committed so every agent gets the same one. As in the pilot, it
holds the recording host's `host.user`, `host.hostname`, CPU count and
ulimits.

**Replay.** On the old build the fixture replays as
`invariant_violated(bit-range-syntax)@97`, every time.

**Candidate fixes.** Each is judged by `kavach diff` against the fixture and by
the hidden test, separately:

| Candidate | `kavach diff` | Variants that reproduce on the old build / pass on the new | Hidden test |
| --- | --- | --- | --- |
| (old build) | — | — | fail |
| `upstream`: parse every argument, then look the key up and check its type, then answer for a missing key | `fixed` | 61 of 64 / 61 | pass |
| `validate-first`: check start, unit and end before the existing lookup; nothing else changes (14 lines added) | `fixed` | 61 of 64 / 61 | pass |
| `unit-only` (narrow): reject an unknown unit before the lookup; non-integer `start`/`end` on a missing key still succeed | `fixed` | 61 of 64 / 61 | fail |
| `type-before-args` (wrong): answer for a missing key after parsing, but keep the type check before it, so a list key answers WRONGTYPE where the arguments are invalid | `fixed` | 61 of 64 / 61 | fail |
| `missing-as-empty` (wrong): drop the early reply and treat a missing key as an empty string; `BITPOS missing 0` now replies `-1` instead of `0` | `fixed` | 61 of 64 / 61 | fail |

**Kavach does not separate the right fixes from the wrong ones here.** All
five fix the recorded failure, and on the recorded traffic they reply
identically. The three that fail the hidden test differ only on inputs the
traffic does not contain:
- non-integer `start` or `end` on a missing key (`unit-only`);
- invalid arguments on a key of another type (`type-before-args`);
- `BITPOS <missing> 0` (`missing-as-empty`). The dashboard only looks for
  set bits.

The variants do not reach them either. Of the 64, 38 drop or repeat an
earlier step, 19 move the failing step earlier, 4 shift the clock, and 3
replace the failing input's whole `argv` with another recorded `argv` (those
3 no longer reproduce). That is the ledger's
`variants-cannot-reach`: no variant changes one argument of a command, such
as a `0` to `a` or a `1` to `0`. Each of the three would be caught by a
variant that did, because `bit-range-syntax` or the output comparison would
then fire on the old build and not on the new.

The traffic was written before the candidates and was not changed to catch
them. Adding `BITPOS <today> 0` to the dashboard would catch
`missing-as-empty` through an output divergence. That is plausible traffic,
but adding it after seeing the result would be tuning the fixture to the
candidates.

## The invariant and how much it gives away

`bit-range-syntax` states the documented syntax of BITCOUNT and BITPOS:
`start` and `end` are integers, `bit` is 0 or 1, the unit is BYTE or BIT, and
the argument count fits. A command outside it must reply with an error. The
invariant decides from `argv` alone, before the command runs, and checks
the reply afterwards. It says nothing about missing keys.

The failure is a well-formed integer reply, so it can't be observed without
an invariant (the ledger's `invariant-leaks-bug`). It comes close to giving
the bug away: the fixture shows the violating step, `BITPOS active:2023-08-21
1 0 -1 BITS` replying `:-1`, with the detail `BITPOS: the unit is not BYTE or
BIT`, and the same command on an existing key replying `ERR syntax error` two
steps earlier. The agent still has to find that the missing key is the
difference and fix the other argument checks too. The hidden test checks
non-integer arguments and wrong-type keys, which the fixture never shows.

## The integration (`redis-kavach.patch`)

Ported from `../redis-10068/redis-kavach.patch` (Redis of January 2022) to
the base commit (August 2023, 7.2 development):
- **Hooks in `server.c`** are the same 12 lines, placed by hand.
  - 7.2 checks for listeners in `initListeners()` (`listen_fds == 0`).
  - `processCommand` calls `call(c, flags)` with `CMD_CALL_REPROCESSING`;
    the hook replaces that call.
  - `InitServerLast()` moved; the host hook follows it.
- **Makefile**: the same KAVACH_SDK/KAVACH_LIB lines, and `kavach.o` added
  to the new object list.
- **`kavach.c`**:
  - `struct redisCommand` has no `name` in 7.2. Messages use `fullname`, and
    the list of connection and blocking commands matches the top-level
    command's `declared_name`, so `CLIENT LIST` matches `client`. `WAITAOF`
    (new in 7.2) is added to that list.
  - The shadow client's `realcmd` is set, which 7.2's `call()` reads.
  - The stream-trimming invariant is replaced by `bit-range-syntax`.
  - Nothing else changed: the step model, shadow client, clock routing,
    recording and host mode are the pilot's, and so are its known limits
    (see `../redis-10068/README.md`).

No line of `bitops.c` changes. The integration is 440 lines of diff. The
Redis test suite was not run with recording on.

## 45-minute assessment

- **Rebuild.** After a one-file change to `src/bitops.c`, `make -j4` in the
  agent image takes 17 s (link-time optimisation relinks the whole server).
- **Focused test.** `./runtest --single unit/bitops` takes 1.4 s in the
  image (46 tests) and about 2 s on the host. The full suite (89 units, 16
  clients) ran 88 units in about 6 minutes on the 11-core host, then hung in
  the last one; in the 6-CPU Docker VM it will be slower. The task text says
  it takes 10 minutes or more and is not needed.
- **Fix size.** The upstream change is 31 added and 19 removed lines in one
  file; `validate-first` shows that 14 added lines are enough.
- **Verdict:** feasible in 45 minutes. The command is about 130 lines of C,
  and the fix is to reorder or add argument checks. The main risk is the stale
  report: an agent that reproduces only `BITCOUNT missing 0`, sees an error,
  and stops. A capable agent checks BITPOS, the sibling command with the same
  syntax.

## Harbor

`../../harbor/tasks/redis-11734-{control,kavach}`, laid out as for the pilot.
The agent timeout is 2700 s (45 minutes). Run from the repository root with
`PYTHONPATH=$PWD`, `harbor run -p bench/agent/harbor/tasks/redis-11734-<arm>
-o "$PWD/bench/agent/results/harbor" --agent <oracle|nop> -y`, on 2026-10-12:

| Agent | Arm | Reward | `kavach diff` (in `kavach.json`) | Job time |
| --- | --- | --- | --- | --- |
| oracle (upstream fix) | control | 1 | `fixed` (61 of 64 / 61) | 127 s, including the image build |
| oracle (upstream fix) | kavach | 1 | `fixed` (61 of 64 / 61) | 50 s |
| nop | control | 0 | `invariant_violated(bit-range-syntax)@97` | 57 s |
| nop | kavach | 0 | `invariant_violated(bit-range-syntax)@97` | 39 s |

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
| `Dockerfile` | The agent images for each arm and the grader image |
| `grade.sh` | Grades one agent attempt in the grader image |
