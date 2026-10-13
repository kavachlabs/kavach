# Task A3: redis__redis-13115

Track A (incident fixes), order 3 in `../../sourcing/selection.csv`. Built on
the recipe of the pilot, `../redis-10068`. **No agent has been run on it yet.**

| | |
| --- | --- |
| Source | SWE-bench Multilingual, `redis__redis-13115` (BSD-3-Clause) |
| Base commit | `4979cf02ff83e90ced80db5111452a4c3e082c3a` (2024-03-08). `COPYING` there is the BSD-3 text. The fix (`5fdaa53d2`, 2024-03-10) and the base both predate the relicense (`0b3439692`, 2024-03-20). |
| Bug | `luaArgsToRedisArgv` turns Lua numbers into command arguments with `fpconv_dtoa`, which prints whole numbers with 8 or more trailing zeros in exponent form: 200000000 becomes `2e+8` (not `2e+08`, as the ledger has it). Commands that take integers reject it, so `redis.call("HINCRBY", k, f, 200000000)` replies `-ERR value is not an integer or out of range`. |
| Hidden test | `EVAL - Lua number -> Redis integer conversion` plus the rest of `unit/scripting` (`hidden/test.patch`, from the SWE-bench record; SWE-bench's FAIL_TO_PASS and PASS_TO_PASS cover the whole file) |

## Results

`./run.sh` reproduces everything below from a clean checkout in about 4–5.5
minutes on an Apple M3 Pro (macOS, clang), most of it cloning Redis, compiling
it six times and running `unit/scripting` six times. Measured 2026-10-12 at
Kavach `cdfafd3`.

**Recording.** The traffic in `traffic.sh` follows the report: a wallet
service credits accounts with a Lua script that validates the amount with
`tonumber(ARGV[1])`, adds it in micro-units with `HINCRBY`, and adds the
dollar amount with `HINCRBYFLOAT`. Readers run `HGET` and `HGETALL`. The script
rejects one invalid amount (`abc`). Five
credits succeed; a sixth, of 200000000 ($200), replies `-ERR value is not an
integer or out of range script: …`. The recorder wrote a fixture flagged by
the `script-number-contract` invariant. `incident.kavach` (2.0 KB, 13 steps)
is the fixture `run.sh` recorded, committed so every agent gets the same one.

- **No earlier step stores a number that prints differently after the fix**
  (the ledger's "fix changes pre-failure outputs: ?"). The earlier amounts
  (1250000, 37500000, 9750000, 120125000, 50000000) print the same with
  `fpconv_dtoa` and as integers.
- **The dollar amounts are exact binary fractions.** `HINCRBYFLOAT` adds in
  `long double`, which is 64-bit on macOS arm64 and wider on Linux. A first
  fixture used $9.99: recorded on macOS, it diverged at the first `HGETALL`
  when replayed in the Linux container (`11.24` against `11.2400000000…`),
  on the old build as well as the new. With $9.75 and $120.125 the digits
  are the same on both, and the macOS fixture replays the same in the
  containers (Harbor results below).
- As in the pilot, the fixture holds the facts the recorder reads from the
  OS: `host.user`, `host.hostname`, `host.uid`, the CPU count and ulimits.

**Replay.** On the old build the fixture replays as
`invariant_violated(script-number-contract)@69`, every time.

**Candidate fixes.** Each is judged by `kavach diff` against the fixture and by
the hidden test, separately:

| Candidate | Size | `kavach diff` | Variants that reproduce on the old build / pass on the new | Hidden test |
| --- | --- | --- | --- | --- |
| (old build) | — | — | — | fail |
| `upstream`: the merged fix; whole numbers that fit a `long long` (`double2ll`) are printed with `ll2string` | +11 −2, one function | `fixed` | 41 of 44 / 41 | pass |
| `cast-check`: the same idea with an explicit range check and `(long long)num == num`, as the maintainers suggested in the issue | +10 −2 | `fixed` | 41 of 44 / 41 | pass |
| `snprintf-17g`: back to Redis 7.0's `snprintf("%.17g")` | +1 −2 | `fixed` | 41 of 44 / 41 | pass |
| `hincrby-accepts-float`: HINCRBY parses its increment as a `long double` and accepts whole values | +11 −1, in `t_hash.c` | `fixed` | 41 of 44 / 41 | pass |
| `always-integer`: every Lua number is printed with `ll2string((long long)num)` | +2 −2 | `diverged@37` | — | fail |

Kavach and the hidden test agree on all six builds, including the two where
both are wrong (below).

## What this does and does not show

- **Two narrow fixes pass both graders.**
  - `hincrby-accepts-float` fixes HINCRBY only. From a script, `INCRBY k
    200000000` and `EXPIRE k 200000000` still reply `-ERR`, and `SET` still
    stores `2e+8`. It also changes what clients get: `HINCRBY h f 2e8` from
    a client now succeeds. The hidden test calls only HINCRBY. The fixture
    has only HINCRBY too, and variants change the failing step's inputs but
    never its command.
  - `snprintf-17g` restores 7.0's behaviour, which is not the upstream fix:
    `SET` of the Lua number 0.1 stores `0.10000000000000001` (upstream and
    the base store `0.1`), and 1e17 is still sent as `1e+17`, which INCRBY
    rejects (upstream sends `100000000000000000`). Neither the hidden test nor
    the traffic uses such numbers.
- **The wrong fix is caught before the incident.** `always-integer` truncates
  fractions, so the credit of $1.25 adds 1 to `balance_usd`; Kavach reports
  the first `HGETALL` as diverged (`11` recorded, `10` replayed). The hidden
  test fails on `Script block the time during execution`, whose
  `DEBUG SLEEP 0.01` becomes `DEBUG SLEEP 0`. The traffic has this step because
  the wallet keeps a dollar balance, not to catch this candidate, but it
  would not have caught it without one.
- **Variants confirmed fixes and caught nothing.** 41 of 44 reproduce on the
  old build, and every correct and narrow fix passes them all. The ledger's
  `variants-cannot-reach`: the failure depends on the amount's digits, and
  variants that change the failing input replace whole fields with values
  from other steps rather than produce other round numbers.
- **The candidates were written by us,** not by an agent.

### How much the invariant gives away

The failure is an ordinary `-ERR` reply, so without an invariant the old build
replays as `ok`. `script-number-contract` (in `src/kavach.c`) states: Lua has
one number type, and a Lua number whose value is a whole number in the 64-bit
range is an integer to Redis, so a command run from a script must not reject
it as "not an integer". It is checked on every command a script runs during a
step (a hook after `proc` in `call()`): if the reply is a "not an integer"
error and an argument is a non-canonical spelling of a whole number in range
(`2e+8`, `1.0`), the step violates it.

That comes close to giving the bug away. The invariant does not name
`fpconv_dtoa`, exponent notation or `luaArgsToRedisArgv`, but its detail on
the incident step reads `HINCRBY, run from a script, rejected argument 3
'2e+8' (the number 200000000) as not an integer`, which points at the
conversion of Lua numbers to arguments. The issue text alone already points
there (it compares `tonumber` with a plain string), so the agent in the
Kavach arm learns little beyond the exact string `2e+8`, which the control
arm can see by running one `EVAL`.

The invariant was written once, before any candidate was built, and not
changed afterwards. An operator could have written it before the incident
from Redis's documentation of Lua-to-Redis conversion, but it exists because
this incident is a number-conversion bug. It sees arguments as the command
received them, so it cannot tell a Lua number from a string: a script that
passes the string `"2e8"` to HINCRBY, which correct Redis rejects, would be
flagged. The traffic has no such script.

## The 45-minute rule

| | |
| --- | --- |
| Reference fix | 11 lines added, 2 removed, in one function (`luaArgsToRedisArgv`, `src/script_lua.c`) |
| Rebuild after touching `src/script_lua.c` | 8.4 s in the image (`make -j4`); 10.7 s on the M3 Pro |
| Focused test (`./runtest --single unit/scripting`) | 13 s in the image (330 tests); 21–25 s on the M3 Pro |
| Full suite (`./runtest`) | 933 s (about 15.5 minutes) in the control image, 3195 tests |

The task text names the focused command and says the full suite takes about
15 minutes and is not needed. The report says where the bug shows up
(`tonumber` + `redis.call`), and Redis's scripting code is one file. Reproducing
it takes one `EVAL`. A capable agent can find, fix and check this well within
45 minutes. `[agent] timeout_sec = 2700.0` in both arms.

## Harbor

From the worktree root, through the shared Docker lock, with `PYTHONPATH=$PWD`:

| Job | Agent | Reward | `kavach diff` (in `kavach.json`) | Time |
| --- | --- | --- | --- | --- |
| `redis-13115-oracle-control` | oracle | 1 | `fixed` (41 of 44 / 41) | 8 min 27 s |
| `redis-13115-oracle-kavach` | oracle | 1 | `fixed` (41 of 44 / 41) | 5 min 22 s |
| `redis-13115-nop-control` | nop | 0 | `invariant_violated(script-number-contract)@69` | 1 min 57 s |
| `redis-13115-nop-kavach` | nop | 0 | `invariant_violated(script-number-contract)@69` | 51 s |

Harbor's total runtimes, 2026-10-13, with the VM shared by other jobs; they
include building the images where Docker's cache did not have them.
The containers are Linux (Colima, arm64); the fixture was recorded on macOS.

## The integration (`redis-kavach.patch`)

Ported from `../redis-10068/redis-kavach.patch`. About 430 lines of C in
`src/kavach.c` and `src/kavach.h`, plus 10 changed lines of hooks in
`server.c` and 6 in the Makefile. It links the C SDK (`sdk/c`). No line of the
code under repair changes. Steps, the shadow client, the clock, recording and
replay work as in the pilot; see its README.

What changed from the pilot's patch:
- **The invariant.** The pilot's `stream-trim-contract` is gone (this task
  has no streams), replaced by `script-number-contract`, with a new hook,
  `kavachAfterProc(c)`, called in `call()` right after the command's `proc`.
  It looks only at commands a script runs during a recorded step.
- **Scripts are steps.** The pilot skipped every command flagged
  `CMD_NOSCRIPT`; EVAL, EVALSHA, EVAL_RO, EVALSHA_RO, FCALL and FCALL_RO carry
  that flag, so they are now let through. The commands a script runs are not
  steps.
- **Redis 7.4 internals.** `redisCommand` has no `name` any more
  (`declared_name`, `fullname`; subcommands are matched by their parent's
  name); `call()` needs `c->realcmd`; the keyspace is a `kvstore`
  (`kvstoreSize`). The "no listening sockets" check moved to
  `initListeners()`, and `processCommand` skips the Kavach path when a command
  is being reprocessed after blocking.
- **macOS.** The 2022 base needed `stat64` and `AvailabilityMacros.h`
  workarounds; this base builds with clang as is.

Known limits are the pilot's. Also: Lua's `math.random` is seeded per script
and so replays deterministically, but the script cache is not flushed at
`hello`, and `SCRIPT LOAD`/`FUNCTION LOAD` (admin or `noscript` commands) are
not steps, so EVALSHA and FCALL replay only if the script was loaded in the
host by an earlier EVAL. The traffic uses EVAL only.

Redis's own `unit/scripting` passes on the integrated build with recording
off. It was not run with recording on.

## Files

| File | |
| --- | --- |
| `task.md` | What the agent is given. Both arms get the report and the focused test command. The control arm works on Redis at the base commit; the Kavach arm works on Redis with the integration, plus the fixture and the tools. |
| `redis-kavach.patch` | The integration, against the base commit |
| `traffic.sh` | The incident traffic |
| `incident.kavach` | The recorded fixture agents receive |
| `candidates/*.patch` | Candidate fixes, applied on top of the integration |
| `hidden/test.patch` | The upstream test, from SWE-bench; never shown to the agent |
| `run.sh` | Builds, records, replays and grades everything above, on the host |
| `Dockerfile` | The agent images for each arm and the grader image |
| `grade.sh` | Grades one agent attempt in the grader image (runs all of `unit/scripting`) |

## Not done yet

- **No model has run.**
- **Candidates in the containers.** Only `upstream` (the Harbor oracle) and
  the empty change (nop) were graded in the containers. The other candidates
  were graded on the host only; `../../harbor/check-candidates.sh` is written
  for redis-10068.
- **Grading** uses this task's own grader image, not SWE-bench's
  (`swebench/sweb.eval.x86_64.redis_1776_redis-13115`). Both run the same
  hidden test.
