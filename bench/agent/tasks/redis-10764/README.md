# Task A5: redis__redis-10764

Track A (incident fixes), 5th in `../../sourcing/README.md`'s "Best
candidates". Built like the pilot, `../redis-10068`. **No agent has been run
on it yet.**

| | |
| --- | --- |
| Source | SWE-bench Multilingual, `redis__redis-10764` (BSD-3-Clause) |
| Base commit | `843a4cdc075a5b251e1b154f8013a9e0abe1038b` (2022-05-22) |
| Bug | `blockingGenericZpopCommand` blocks on `argv[1..argc-2]`, which for BZPOPMIN/BZPOPMAX are the keys but for BZMPOP also include the timeout, numkeys, `MIN`/`MAX` and `COUNT`. A client that creates a sorted set with one of those names wakes the waiting BZMPOP, which pops that set and replies with its name. |
| Reference fix | 1 line in `src/t_zset.c`: block on `keys, numkeys` |
| Hidden test | `BZMPOP should not blocks on non key arguments - #10762` plus the 19 other BZMPOP tests (`hidden/test.patch`, from the SWE-bench record; run as `./runtest --single unit/type/zset --only BZMPOP`) |

## Results

`./run.sh` reproduces everything below from a clean checkout in about
2 minutes 45 seconds on an Apple M3 Pro (macOS, Tcl 8.5): it clones Redis, builds it five
times and runs the hidden test five times. Measured 2026-10-13 at Kavach
`cdfafd3`.

**Recording.** `traffic.sh` is a game backend: a job queue, the sorted set
`jobs`, drained by workers with `BZMPOP 0 1 jobs MIN COUNT 2`, and one
leaderboard per match, named by the match number (`2`, `3`, `1`). Workers wait
while the queue is empty, and a new job wakes one of them; that path works.
Then match 1 records its first score, `ZADD 1 10 player:raj`, while a worker
waits. `1` is the numkeys argument of the worker's BZMPOP, so the worker wakes,
pops match 1's whole leaderboard and replies `1`; `ZCARD 1` is then 0. The
recorder flagged the ZADD's step with the `pop-key-contract` invariant.
`incident.kavach` (1.8 KB, 18 steps, 2 clients waiting at once) is the fixture
`run.sh` recorded, committed so every agent gets the same one. As in the
pilot, the server runs with a minimal environment, relative paths and
`TZ=UTC`, and the fixture still holds the host facts the recorder reads from
the OS.

**Replay.** On the old build the fixture replays as
`invariant_violated(pop-key-contract)@70`, every time.

**Candidate fixes.** Each is judged by `kavach diff` against the fixture and by
the hidden test, separately:

| Candidate | `kavach diff` | Variants that reproduce on the old build / pass on the new | Hidden test |
| --- | --- | --- | --- |
| (old build) | — | — | fail |
| `upstream`: block on `keys, numkeys` | `fixed` | 38 of 64 / 38 | pass |
| `key-specs`: block on the keys `getKeysFromCommand` finds (what the issue suggests) | `fixed` | 38 of 64 / 38 | pass |
| `skip-leading-args`: block from the first key to the argument before the last | `variant_failed(30)@54` | 38 of 64 / 36 | fail |
| `count-only`: the right count from the wrong place (`argv+1, numkeys`) | `nondeterministic@35` | — | fail |

Kavach and the hidden test agree on all four candidates.

## What this does and does not show

- **Cross-position array-element variants catch `skip-leading-args`.**
  `skip-leading-args` stops BZMPOP from blocking on its timeout and numkeys,
  the arguments the report and the incident are about, but still blocks on
  `MIN`/`MAX`, `COUNT` and the count when COUNT is given. The hidden test
  writes `min`, `max`, `count` and `10` and catches it.
  - Before array-element variants (`cdfafd3`, 37 of 59 reproducing), and with
    same-index array-element variants alone (41 of 64 reproducing),
    `skip-leading-args` passed `kavach diff`: the recorded trigger is `"1"`
    (`argv[1]` of ZADD, `argv[2]` of BZMPOP), and same-index variants only
    replaced `argv[1]` of ZADD with `argv[1]` of other commands (`"jobs"`,
    `"0"`, `"2"`).
  - With cross-position array-element variants (§6.1), `"1"` in the failing
    ZADD is also replaced by other arguments of the earlier array where `"1"`
    appeared at another index (`BZMPOP 0 1 jobs MIN COUNT 2`), producing
    `"argv"[1] "1" → "MIN"` (variant 30) and `"argv"[1] "1" → "COUNT"`
    (variant 60). Both reproduce `pop-key-contract` on the old build and fail
    on `skip-leading-args`, while passing on `upstream` and `key-specs`.
- **The ledger's two claims, checked.**
  - *"Field variants on the ZADD's key reach the trigger"*: true once array
    elements are varied across argument positions of earlier commands that
    share the element (above).
  - *"No identified capability gap"*: true of Kavach, false of the pilot's
    integration. Its `redis-kavach.patch` ran blocking commands outside the
    step model, so this incident could not be recorded at all. Kavach's model
    held: a step's outputs may go to sinks other than the step's own client,
    so the reply of a command that waited is an output of the step that
    served it. The integration grew by about 340 lines (below).
- **The invariant comes close to the bug, but no closer than the report.**
  `pop-key-contract` says that a pop which names the key it popped from
  (BLPOP, BRPOP, BLMPOP, LMPOP, BZPOPMIN, BZPOPMAX, BZMPOP, ZMPOP) replies with
  one of the command's keys, by the command's key specs. It is documented
  behaviour and an operator could write it before the incident, for every
  such command. It names the symptom (a reply with a foreign key name), which
  is what the report already says; it does not point at `blockForKeys` or at
  which arguments were taken as keys.
- **`count-only` shows as nondeterminism, not divergence.** Blocking on the
  timeout alone, workers never wake for `jobs`, so the step that served one
  in production reads the clock fewer times (Redis updates its cached time
  while serving). Kavach stops at the first difference, the reads, before it
  compares outputs. Either way the build is rejected.
- **The candidates were written by us,** not by an agent.

## The integration (`redis-kavach.patch`)

The pilot's integration, ported to the base commit, plus blocking commands.
`src/kavach.c` is 889 lines (the pilot's is 552), plus the pilot's hooks in
`server.c` and the Makefile and one new hook at the end of `unblockClient()`
in `blocked.c`.

Porting changes:
- The Makefile's object list had gained `syscheck.o`; the hunk was reapplied.
- `struct redisCommand` has `fullname` instead of `name`, and `call()` reads
  `c->realcmd`, which the shadow client must set (without it the first
  command crashed).
- `kavachWantsCall` names subcommands by their parent (CLIENT LIST is
  `client`).

Blocking commands, new:
- BLPOP, BRPOP, BRPOPLPUSH, BLMOVE, BLMPOP, BZPOPMIN, BZPOPMAX and BZMPOP are
  steps. XREAD BLOCK, WAIT and module blocking still run outside the step
  model.
- A command that has to wait leaves its shadow client blocked, as Redis leaves
  a client, and its step replies nothing. Each waiting command has its own
  shadow; the input's position (the client id) says which.
- The step whose write serves it calls `handleClientsBlockedOnKeys()` inside
  the step and emits the served reply to the sink `reply:<client id>`. Live,
  the reply then goes to the real client.
- The real client waits on a reserved key (`"\0kavach-parked"`) with the
  command's timeout, so Redis stops reading its input, times it out and cleans
  up after it as usual. It counts as the one blocked client in INFO.
- A wait that ends without data (timeout, or the client went away) is a step
  of its own from that client, `{"unblock":true}`, which replies the null
  array. The shadow never times out by itself, so replay does not depend on
  Redis's cron.
- In replay, a variant can send a command from a client whose earlier command
  still waits, which a real client cannot. The earlier command is then
  dropped, as if the client had gone away.
- The new invariant `pop-key-contract` (above); the pilot's
  `stream-trim-contract` is kept.

Known limits, beyond the pilot's:
- A waiting client served by something that is not a step (a write inside
  MULTI/EXEC or a script, an admin command) gets its reply, but the journal
  misses it; the server logs a warning.
- `CLIENT UNBLOCK … ERROR` is recorded as a timeout.
- A client that writes the reserved key would be served from it.

With recording on, Redis's own `unit/type/zset` and `unit/type/list` suites
pass. They wrote 5 journals: 2 replay as `ok`, 3 diverge. One diverges at
`INFO`; the other two after a RESP3 client (`HELLO 3`, not recorded) wrote
the keys the next recorded command reads. I attributed these by reading the
journal and the suite. The zset journal replays about 117,000 records,
including the blocking tests, before it diverges at a BZPOPMIN after the
RESP3 tests.

## Time (45-minute rule)

Measured in the control image on the Colima VM (6 CPUs):

| | |
| --- | --- |
| Incremental `make` after touching `src/t_zset.c` | 3.0 s |
| `./runtest --single unit/type/zset --only BZMPOP` (24 tests) | 3.0 s |
| `./runtest --single unit/type/zset` (308 tests) | 12.3 s |
| Reference fix | 1 line, 1 file |

The report names the command and shows the wrong reply; the code is one
function in `t_zset.c` whose first lines take `keys, numkeys`; the focused
test takes seconds. A capable agent can find, fix and check this well inside
45 minutes. `task.md` names the focused test command and says the full suite
is not needed.

One trap: a wrong fix can leave a test waiting for a reply that never comes
(`count-only` does), and Redis's test runner then stalls for its default 20
minutes. `task.md` tells the agent about `--timeout 60`, and `run.sh` and
`grade.sh` use it.

## Harbor checks

From the repository root, through the shared Docker lock (times are Harbor's
own, image builds included):

| Run | Arm | Reward | Time |
| --- | --- | --- | --- |
| oracle (`upstream`) | kavach | 1 (hidden pass, `kavach diff` `fixed`, 37 of 59 / 37) | 4m 16s |
| nop (no change) | kavach | 0 (hidden fail, `invariant_violated(pop-key-contract)@70`) | 52s |
| oracle (`upstream`) | control | 1 (hidden pass, `fixed`) | 1m 50s |
| nop (no change) | control | 0 (hidden fail, `invariant_violated(pop-key-contract)@70`) | 1m 13s |

The commands were `harbor run -p bench/agent/harbor/tasks/redis-10764-<arm>
-o "$PWD/bench/agent/results/harbor" --agent oracle|nop`, with
`PYTHONPATH=$PWD`. Both arms give the agent 2700 s.

## Files

| File | |
| --- | --- |
| `task.md` | What the agent is given. Both arms get the report and the test command; the Kavach arm also gets the integration, the fixture and the tools. |
| `redis-kavach.patch` | The integration, against the base commit |
| `traffic.sh` | The incident traffic |
| `incident.kavach` | The recorded fixture agents receive |
| `candidates/*.patch` | Candidate fixes, applied on top of the integration |
| `hidden/test.patch` | The upstream test, from SWE-bench; never shown to the agent |
| `run.sh` | Builds, records, replays and grades everything above, on the host |
| `Dockerfile` | The agent images for each arm and the grader image |
| `grade.sh` | Grades one agent attempt in the grader image |

`../../harbor/check-candidates.sh` is written for the pilot only, so the
candidates were graded through Harbor only as the oracle (`upstream`) and nop
(empty) solutions.
