# Task A4: valkey-io__valkey-1499

Track A (incident fixes), order 4 in `../../sourcing/selection.csv`. Built on
the recipe of the pilot, `../redis-10068/`. **No agent has been run on it
yet.**

| | |
| --- | --- |
| Source | SWE-bench Multilingual, `valkey-io__valkey-1499` (BSD-3-Clause) |
| Base commit | `93b701d8d48eaf87077e707dd77eaaed0e66900f` (2025-01-03) |
| Bug | In no-touch mode (`CLIENT NO-TOUCH ON`), `lookupKey` exempts TOUCH from the no-touch rule by checking `server.current_client->cmd`. Inside a script that is the calling client's command, `EVAL`, not `TOUCH`, so `redis.call('TOUCH', k)` in a script leaves the key's last access time unchanged and `OBJECT IDLETIME` keeps growing. |
| Reference fix | 1 line in `src/db.c`: check `server.executing_client->cmd` instead |
| Hidden test | `unit/introspection-2` (`hidden/test.patch`, from the SWE-bench record): FAIL_TO_PASS `TOUCH alters the last access time of a key in no-touch mode`, plus 45 PASS_TO_PASS tests in the same file |

## Results

`./run.sh` reproduces everything below from a clean checkout in about 4½
minutes on an Apple M3 Pro (macOS, Tcl 8.5), most of it compiling Valkey five
times and running the hidden test file five times. Measured 2026-10-12 at
Kavach `cdfafd3`. On macOS, give it a short work directory (see `run.sh`).

**Recording.** The traffic in `traffic.sh` is a session cache. An application
writes six `session:*` keys and a counter and reads some of them. Three
seconds later a cache keeper connects in no-touch mode, so that its own reads
do not keep keys alive. It reads idle times with `OBJECT IDLETIME`, peeks at a
value with a read-only script, `TOUCH`es one key directly, and then keeps two
keys alive with a script that `TOUCH`es them. Against the live server, that
script left both keys 3 s idle, and the recorder wrote a fixture flagged by the
`access-time-contract` invariant. `incident.kavach` (1.7 KB, 19 steps) is the
fixture `run.sh` recorded, committed so every agent gets the same one. As in
the pilot, it holds the recording host's user, hostname, CPU count and limits.

**Replay.** On the old build the fixture replays as
`invariant_violated(access-time-contract)@75`, every time.

**Candidate fixes.** Each is judged by `kavach diff` against the fixture and by
the hidden test, separately:

| Candidate | `kavach diff` | Variants that reproduce on the old build / pass on the new | Hidden test |
| --- | --- | --- | --- |
| (old build) | — | — | fail |
| `upstream`: check `executing_client->cmd` | `fixed` | 47 of 64 / 47 | pass |
| `touch-sets-access-time`: TOUCH looks keys up without touching, then sets their access time itself | `fixed` | 47 of 64 / 47 | pass |
| `eval-only`: exempt the case where the caller's command is `EVAL` and the executing one is `TOUCH` | `fixed` | 47 of 64 / 47 | pass |
| `scripts-always-touch`: drop the no-touch rule while a script runs | `invariant_violated(access-time-contract)@57` | — | **pass** |

Both graders accept `eval-only`, which is too narrow. Kavach rejects
`scripts-always-touch`, which the hidden test accepts and which is wrong.

## What this does and does not show

- **The hidden test misses a wrong fix; Kavach catches it.**
  `scripts-always-touch` makes `TOUCH` in a script work, but every other read a
  script makes in no-touch mode now updates access times too, which is what
  no-touch mode exists to prevent. The hidden test never reads a key from a
  script in no-touch mode. The recorded traffic does (the keeper's "peek"
  script at seq 57).
  - The invariant's second rule flags it at that step. Without the rule,
    Kavach would still reject it: the keeper's next `OBJECT IDLETIME` (seq 63)
    replies `0` where production replied `3`, a divergence.
- **A narrow fix passes both graders.** `eval-only` handles `EVAL` but not
  `EVALSHA`, `FCALL` or their `_RO` forms.
  - The hidden test only uses `EVAL`.
  - Kavach misses it too. The fixture only uses `EVAL`, and variants mutate
    the failing input's JSON fields to values from other inputs, so they never
    turn `EVAL` into `FCALL`. This is the ledger's `variants-cannot-reach` gap.
- **Variants confirmed the fixes and caught nothing here.** 47 of 64 reproduce
  the incident on the old build. Of the 17 that don't, 5 set `no_touch` to
  false, remove it, or replace or remove `argv`; 12 move the keeper's script
  before the application's 3-second pause, where the keys it touches are not
  yet a second old.
- **The invariant comes close to the bug, but not closer than the report.**
  `access-time-contract` states the documented contract of `TOUCH` and
  `CLIENT NO-TOUCH`: TOUCH updates the last access time of each key it names,
  and a client in no-touch mode does not update the access time of the keys it
  reads, except through TOUCH. It does not mention scripts or which client is
  checked. On the fixture it reports
  `TOUCH did not update the last access time of key session:3` at the `EVAL`
  step. The report already says that TOUCH in a script does nothing in
  no-touch mode, so the agent learns where (the access-time path in
  `lookupKey`) no sooner than from the report. An operator would write this
  invariant before the incident only if access times mattered to them, as they
  do to a cache keeper relying on LRU eviction; that is the
  `invariant-leaks-bug` flag in the ledger.
- **The candidates and the invariant were written by us,** not by an agent.
  The invariant was written before any candidate was built and not changed
  afterwards.

## The integration (`valkey-kavach.patch`)

A port of `../redis-10068/redis-kavach.patch` to Valkey at the base commit:
about 500 lines of C in the new `src/kavach.c` and `src/kavach.h`, plus 13
changed lines of hooks in `server.c`, 2 in `evict.c` and 8 in the Makefile. It
links the C SDK (`sdk/c`). No line of the code under repair changes.

What the port changed:

- **Valkey names.** `client->flag` bitfields instead of `flags` bits,
  `serverCommand`, `serverGitSHA1()`, `valkey-server`, the recorder's directory
  in `KAVACH_VALKEY_DIR`, and the service name `valkey`.
- **No-touch mode is part of the input.** The shadow client runs every step,
  so a connection's own state never reaches it. The pilot carries the selected
  database as `"db":N`; this port also carries `"no_touch":true` when the
  client is in no-touch mode. `CLIENT NO-TOUCH` itself is still not a step.
- **Scripts are steps.** The pilot skipped every command flagged `noscript`,
  which includes `EVAL`, `EVALSHA` and `FCALL`. They are recorded now, and the
  commands a script calls run inside its step.
- **The LRU clock is routed.** During a step, `LRU_CLOCK()` is derived from the
  step's cached time, which `call()` reads from the Kavach clock. It reads no
  clock of its own, so it adds no reads to the journal, and builds that touch
  keys more or less often still read the clock the same number of times.
  Otherwise the LRU clock is what `serverCron` caches, which a replay host never
  runs, and `OBJECT IDLETIME` would not replay.
- **A call hook.** `call()` calls `kavachObserveCall()` just before running a
  command, so the invariant sees the commands a script runs. It notes the
  keys each names (`getKeysFromCommand`) and their access times.
- **A new invariant.** The pilot's `stream-trim-contract` is gone;
  `access-time-contract` (above) replaces it. It does not apply under an LFU
  eviction policy, or while a child process runs.

The pilot's other limits still hold: transactions, blocking commands,
connection and admin commands, pub/sub and RESP3 clients are not recorded;
`random()`, the monotonic clock and the hash seed are not routed; server
config is not served from the journal; the shadow client is what slowlog,
`MONITOR` and `CLIENT TRACKING` see. The invariant keeps a note per key a step
names, which is cheap, but scans the notes linearly.

Unlike the pilot, Valkey's own test suite was not run with recording on.

## The 45-minute budget

Agents get 45 minutes (`timeout_sec = 2700` in both arms).

| | Host (M3 Pro) | Agent image |
| --- | --- | --- |
| Incremental rebuild after touching `src/db.c` (`make -j4`, relinks with LTO) | 14 s | 14 s (control), 9 s (kavach) |
| Focused test, `./runtest --single unit/introspection-2` | 9 s | 7 s (both arms) |
| `kavach diff` of the fixture | 0.6 s | |

The reference fix is one line. The report gives the reproduction, the place to
look is the only no-touch check in `lookupKey`, and `task.md` names the
focused test and says the full suite isn't needed. A capable agent should
finish well within 45 minutes. The full suite's time was not measured; the
task text says "tens of minutes".

## Harbor

`../../harbor/tasks/valkey-1499-control` and `-kavach`, as for the pilot:
separate verifier container, the collect hook, and `solution/` with the
upstream fix. Run through `heavy.sh` from the repository root with
`PYTHONPATH=$PWD`:

| Agent | Arm | Reward | Kavach | Time |
| --- | --- | --- | --- | --- |
HARBOR_ROWS

## Files

| File | |
| --- | --- |
| `task.md` | What the agent is given. Both arms get the report and the focused test. The control arm works on Valkey at the base commit; the Kavach arm works on Valkey with the integration, plus the fixture and the tools. |
| `valkey-kavach.patch` | The integration, against the base commit |
| `traffic.sh` | The incident traffic |
| `incident.kavach` | The recorded fixture agents receive |
| `candidates/*.patch` | Candidate fixes, applied on top of the integration |
| `hidden/test.patch` | The upstream test, from SWE-bench; never shown to the agent |
| `run.sh` | Builds, records, replays and grades everything above, on the host |
| `Dockerfile` | The agent images for each arm and the grader image |
| `grade.sh` | Grades one attempt in the grader image |

## Not done yet

- **No model has run.**
- **Grading** uses this task's own grader image, not SWE-bench's
  (`swebench/sweb.eval.x86_64.valkey-io_1776_valkey-1499`). Both run the same
  hidden test file.
- `../../harbor/check-candidates.sh` is written for the pilot; the candidates
  above were graded on the host by `run.sh`, not through Harbor.
