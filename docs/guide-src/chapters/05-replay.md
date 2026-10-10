# Replay and Verification
> replay/, sdk/go/host.go, sdk/go/kavachtest — playing a fixture back, and deciding whether a fix is real

## What replay is for

Chapters 2 to 4 end with a fixture on disk: every input a handler consumed before it failed, every answer the world gave it, every output it asked for, and a marker saying how it failed. This chapter is about the other end. **Replay** runs a build of the handler against a fixture, serving each read from the fixture instead of from the world, and reports a **status** such as `still_failing@32` or `fixed`. **Verification** (`kavach diff`) replays the fixture under two builds, the one that failed in production (the **old** build) and a candidate fix (the **new** build), and then replays dozens of perturbed copies of the incident, called **variants**, to check that the fix is not tailored to the one recorded input.

A few terms used throughout:

- A **step** is one `input` record and the records that follow it up to the next `input`. Replay runs the handler once per step.
- A **read** is a `clock`, `rand`, `gateway` or `config` record: an answer the world gave the handler. An **output** is an `output` record: an effect the handler asked for.
- The **recorded failure** is the first step followed by a `panic`, `error`, `invariant` or `crash` marker.
- A **lenient** step is one whose reads are served by kind rather than in strict order, because the build under test is expected to behave differently there.

Here is the normative text. It defines the strict rules, the lenient exception for the failing step, the statuses and the order in which they are checked.

@spec-only 6

The rest of the chapter follows a replay through the code: how the CLI starts the service as a **host** process and talks to it (§9), how the engine in `replay/replay.go` turns what the host does into a status, how `replay/verify.go` and `replay/variants.go` turn two builds into a verdict, and finally the ledger demo run end to end.

@flow kavach CLI (driver) -> host process (service binary + kavach-host) -> replay.Result (status) -> replay.Verification (verdict) | The pieces of a replay. The CLI implements every rule of §6; the service implements only the protocol.

## Code map

@graph files replay/ sdk/go/host.go sdk/go/entry.go | Figure 5.1: The replay engine's files, with the Go SDK's host side.
@graph calls replay_replay_replayjournal 1 | Figure 5.2: replayJournal, the step loop, and what it calls.
@graph calls replay_verify_verify 1 | Figure 5.3: Verify: replaying old and new builds and the variants.
@graphify explain replay_driver_runhost

## One binary records and replays

Kavach does not ship a replayer for each language. The `kavach` CLI is the only replayer, and the service under test takes part by running its own handler, one step at a time, on the CLI's instructions. The same binary that records in production is the one that replays: started normally it consumes events; started with `kavach-host` as its last argument it becomes a **host** and does nothing but run steps.

@spec-only 9

In Go this is one call at the top of `main`. The ledger demo's first statement is:

@code examples/ledger/sdk/main.go 26-28

`MaybeReplay` returns immediately unless the last argument is `kavach-host`. When it is, it keeps the real standard output for the protocol, points `os.Stdout` at standard error so that a handler that prints cannot corrupt the stream, serves the session, and exits.

@code sdk/go/entry.go func:MaybeReplay

:::trap Common trap
Call `MaybeReplay` before anything that reads flags, opens connections or starts consuming. A host must have no live effects (§9.1). Code that runs before `MaybeReplay` runs during every replay too, and anything it prints to standard output before the swap breaks the protocol: the driver rejects a line that is not a protocol message.
:::

## The host protocol

### Starting a host

@spec 9.1

The driver side lives in `replay/driver.go`. A host command can be a single string such as `"python -m ledger"`, so the first job is to split it into words the way a POSIX shell would, without expanding anything:

@code replay/driver.go func:SplitCommand

The host does not inherit the CLI's environment. `hostEnviron` builds it from the journal's genesis `environment` record: a variable recorded in clear (form `0`) is set to its recorded value; a variable recorded only as a hash (form `1`, a secret) gets the driver's own value, if it has one, and the difference shows up later as drift; an unset variable (form `2`) stays unset. `PATH` and `HOME` are passed from the driver only when the journal does not name them.

@code replay/driver.go func:hostEnviron

`startHost` starts the process with `kavach-host` appended, starts a goroutine that turns its standard output into a channel of lines, sends `hello`, and waits up to `StepTimeout` for `ready`. Note that `mode` is always `"process"`: the driver never asks for a sandbox (§6.3, below). The `ready` message carries the names of the handler's invariants and the replay machine's environment, which the driver keeps for later.

@code replay/driver.go 24-25
@code replay/driver.go func:startHost

### Messages and a session

@spec 9.2
@spec 9.3

### Both halves of one step

The figure shows the first step of the ledger fixture: a deposit for alice. The handler reads the clock, draws 8 random bytes for a transaction id, and emits one ledger entry. Each read is a request the driver answers from the fixture; the emit is sent and not answered.

<figure class="diagram">
<svg viewBox="0 0 660 340" width="640" xmlns="http://www.w3.org/2000/svg" font-family="Helvetica, Arial, sans-serif" font-size="11">
  <defs>
    <marker id="ah" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
      <path d="M0,0 L10,5 L0,10 z" fill="#1f3a5f"/>
    </marker>
  </defs>
  <rect x="150" y="10" width="140" height="28" rx="3" fill="#e8eef6" stroke="#1f3a5f"/>
  <text x="220" y="28" text-anchor="middle" font-weight="bold" fill="#1f3a5f">driver (kavach CLI)</text>
  <rect x="380" y="10" width="140" height="28" rx="3" fill="#e8eef6" stroke="#1f3a5f"/>
  <text x="450" y="28" text-anchor="middle" font-weight="bold" fill="#1f3a5f">host (./ledger)</text>
  <line x1="220" y1="38" x2="220" y2="320" stroke="#888" stroke-dasharray="4 3"/>
  <line x1="450" y1="38" x2="450" y2="320" stroke="#888" stroke-dasharray="4 3"/>

  <line x1="220" y1="65" x2="450" y2="65" stroke="#1f3a5f" marker-end="url(#ah)"/>
  <text x="335" y="60" text-anchor="middle">step seq 1, data {"type":"deposit",…}</text>
  <text x="212" y="69" text-anchor="end" fill="#555">record 1 (input)</text>
  <text x="458" y="69" fill="#555">Handle(env, in)</text>

  <line x1="450" y1="100" x2="220" y2="100" stroke="#1f3a5f" marker-end="url(#ah)"/>
  <text x="335" y="95" text-anchor="middle">clock</text>
  <text x="458" y="104" fill="#555">env.Now()</text>

  <line x1="220" y1="130" x2="450" y2="130" stroke="#1f3a5f" marker-end="url(#ah)"/>
  <text x="335" y="125" text-anchor="middle">clock unix_nanos "…"</text>
  <text x="212" y="134" text-anchor="end" fill="#555">served from record 2</text>

  <line x1="450" y1="165" x2="220" y2="165" stroke="#1f3a5f" marker-end="url(#ah)"/>
  <text x="335" y="160" text-anchor="middle">rand n 8</text>
  <text x="458" y="169" fill="#555">env.Read(txn[:])</text>

  <line x1="220" y1="195" x2="450" y2="195" stroke="#1f3a5f" marker-end="url(#ah)"/>
  <text x="335" y="190" text-anchor="middle">rand data (8 bytes)</text>
  <text x="212" y="199" text-anchor="end" fill="#555">served from record 3</text>

  <line x1="450" y1="230" x2="220" y2="230" stroke="#1f3a5f" marker-end="url(#ah)"/>
  <text x="335" y="225" text-anchor="middle">emit ledger.entries (no answer)</text>
  <text x="212" y="234" text-anchor="end" fill="#555">captured</text>
  <text x="458" y="234" fill="#555">env.Emit(…)</text>

  <line x1="450" y1="265" x2="220" y2="265" stroke="#1f3a5f" marker-end="url(#ah)"/>
  <text x="335" y="260" text-anchor="middle">done outcome ok</text>
  <text x="458" y="269" fill="#555">invariants checked</text>

  <rect x="30" y="285" width="380" height="30" rx="3" fill="#fff4e0" stroke="#c98a1b"/>
  <text x="220" y="304" text-anchor="middle">all reads used? invariants hold? outputs equal record 4?</text>
</svg>
</figure>
<p class="caption">One step of the ledger fixture over the host protocol. The left column says where each answer comes from; the checks at the bottom run in the driver once <code>done</code> arrives.</p>

On the driver side, the host process is wrapped in a type that implements `kavach.Handler`. That is the central design choice of the driver: the replay engine (next section) calls `Handle` on whatever handler it is given, in process or remote, and the `host` type turns that call into protocol messages. `Handle` sends the `step` and loops: each request from the host is answered by `serve`, which simply calls the replay's `Env` (`env.Now()`, `env.Read(...)`, ...) and wraps the result in a message. So the rules of §6 are written once, in `replayEnv`, and the protocol only transports them.

@code replay/driver.go func:Handle
@code replay/driver.go func:serve

`done` ends the step. `finish` maps the outcome back into what an in-process handler would have done: a `panic` outcome becomes a Go panic with the host's message, an `error` outcome an error, and an `invariant` outcome is remembered so that the engine's invariant check (via `host.Invariants`, below) reports it.

@code replay/driver.go func:finish
@code replay/driver.go func:Invariants

The host side is `sdk/go/host.go`. `ServeHost` reads `hello`, creates a fresh handler, restores a snapshot if the journal starts from one, collects the invariant names and the environment, sends `ready`, and then runs steps until `end`.

@code sdk/go/host.go func:ServeHost

`step` runs the handler with a `hostEnv`, recovers a panic with its stack, and builds `done`. Invariants are checked only after a step that returned normally, as §9.2 requires.

@code sdk/go/host.go func:step

`hostEnv` is the mirror image of `replayEnv`: every read becomes a request and blocks for its answer. `ask` sends one request, reads one message, and panics with `abortStep` if the driver answered `abort`.

@code sdk/go/host.go func:ask
@code sdk/go/host.go func:Now
@code sdk/go/host.go func:Query

Two details are worth noticing. Every gateway query and every emit is sent with `"scope": "remote"`: the Go host has no notion of local effects yet, which matters only for sandbox replay. And the Go `driverMsg` has no `mode` field, so the Go host ignores `mode` and would run a `"sandbox"` hello as a process replay; the hosts of the other eleven SDKs answer it with `fatal` ("sandbox mode not supported").

### Aborts

@spec 9.4

When `serve` hits nondeterminism, the replay's `Env` panics with a `nondeterminism` value; `serve` recovers it and returns it. `Handle` then calls `abort`, which sends `abort`, answers any request still in flight with another `abort`, and waits for `done` (or `fatal`, or the deadline). Whatever the host does, the step's status is already decided: `Handle` re-raises the `nondeterminism` panic for the engine to report.

@code replay/driver.go func:abort

On the host side, `ask` sets `aborted` and panics with `abortStep`. Any further read or emit panics again immediately, so a handler that catches the first panic still cannot make progress, and `step` reports `done` with outcome `"aborted"`.

### When the host fails

@spec 9.5

`recv` returns `errHostGone` when the reader goroutine closes the line channel (the host closed its output or exited) and `errTimeout` at the step's deadline. `stepFailure` turns those into two panic values, `hostCrash` and `hostTimeout`, which the engine reports as `still_failing@N` with a detail of `crash: …` or `timeout: …`. A timed-out host is killed. Anything else, such as a malformed line, is a `hostFailure`, which makes the replay fail to run (the CLI exits 3) rather than produce a status.

@code replay/driver.go func:stepFailure
@code replay/driver.go func:crashDetail

Before `ready`, the same errors mean the command is probably not a host at all, and `beforeReady` says so in the error message.

@code replay/driver.go func:beforeReady

## The engine: replay.go

### Statuses and results

A replay produces a `Result`: the status, the `seq` where it applies, the steps it ran and what each produced, the recorded failure, and the environment drift. `Result` holds no timings, so replaying the same fixture against the same build gives a byte-identical result; `TestDeterminism` in `examples/ledger/sdk` checks exactly that, 1,000 times.

@code replay/replay.go 15-35
@code replay/replay.go type:StepResult
@code replay/replay.go type:Result
@code replay/replay.go func:String

### Splitting a journal into steps

`splitSteps` walks the records once. Reads and outputs attach to the current step; an `environment` record written after a step is kept as that step's `envAfter`; the first failure marker of a step becomes its `marker`. A read or an output before the first `input` is an error.

@code replay/replay.go type:step
@code replay/replay.go func:splitSteps

### The step loop

`replayJournal` is the whole engine. Both entry points use it: `Run` (in process, given a Go handler constructor, used by tests and `kavachtest`) and `RunHost` (through a host process, used by the CLI). They differ only in the `open` function that creates the handler.

@code replay/replay.go func:Run
@code replay/replay.go func:replayJournal

Each status is decided by a specific branch of that loop. In the order the code checks them for each step:

| Status | Decided at | Condition |
| --- | --- | --- |
| `nondeterministic@N` | lines 195–198 | the handler panicked with `nondeterminism`, raised by `replayEnv` on a read that does not match; `N` is the `seq` of the record where the expectation failed |
| (no status) | lines 199–201 | a `hostFailure`: the replay could not run; `RunHost` returns an error |
| `still_failing@N` | lines 202–221 | the host crashed or timed out, the handler panicked, or it returned an error; `N` is the step's input `seq` |
| `nondeterministic@N` | lines 223–226 | a strict step returned while recorded reads remained unread |
| `invariant_violated(X)@N` | lines 227–230 | `kavach.CheckInvariants` names the first failing invariant |
| `diverged@N` | lines 231–235 | a step that is not the recorded failure, in a journal that is not a variant, emitted outputs different from the recorded ones |
| `fixed` / `ok` | lines 238–243 | every step passed; `fixed` if the journal recorded a failure |

This is the order of the table in §6: nondeterminism, then failure, then invariants, then divergence. Note that the failing step is never checked for divergence (`st.marker == nil` on line 231): a fixed build is expected to emit something different there, and that difference is what `kavach diff` prints as the "first divergence".

For a host, `kavach.CheckInvariants(h)` calls `host.Invariants`, shown above, which returns one `Invariant` per declared name; the one whose name matches the last `done`'s `invariant` outcome fails. The host already checked them; the driver only replays its answer through the same code path the in-process handler uses.

Outputs are compared in full, in order, sink and bytes:

@code replay/replay.go func:diffOutputs

### Serving reads

`replayEnv` is the `kavach.Env` the handler (or the `host` proxy) reads from during replay. `reset` prepares it for a step, with `lenient` set for the failing step and for every step of a variant.

@code replay/replay.go type:replayEnv
@code replay/replay.go func:reset

In a strict step every read goes through `next`, which insists that the next unread record is of the kind asked for:

@code replay/replay.go func:next

Each read method has a strict branch and a lenient branch. In the lenient branch, clock and random reads are served in recorded order by kind (`nextOfType`), and once they run out are synthesized: the last time served, or bytes derived from the step's `seq` and a counter by SHA-256 (`fillDeterministic`), so that a synthesized replay is still deterministic. Gateway and config reads are matched by content with `takeUnread`.

@code replay/replay.go func:Now
@code replay/replay.go func:Read
@code replay/replay.go func:fillDeterministic
@code replay/replay.go func:Query
@code replay/replay.go func:takeUnread

In the lenient `Read`, a recorded `rand` of a different length from the one asked for is consumed and then ignored: the read is synthesized, and the next random read is served from the record after it. Every synthesized read increments `synthesized`, which the CLI prints as "reads past the recorded failure were synthesized from no record".

:::trap Common trap
A changed gateway request in a strict step is nondeterminism, not a cache miss (§6). If a fix changes what the handler asks an upstream system *before* the failing step, every replay of that fixture will report `nondeterministic@N`. That is deliberate: the recorded response answered a different question. The fix has to keep earlier queries identical, or the fixture cannot judge it.
:::

### Config fallback

In the failing step, a config read the step never made in production has to be answered from somewhere. §6 says: the value last recorded for that key anywhere earlier in the journal, by a `config` record or as a `flag.` or `env.` fact of the latest `environment` record that holds that fact. An `environment` change record (§4.8) holds only the facts that changed, so "the latest environment record" cannot mean the latest record of that type; it means the latest one that mentions the key. `history` implements that reading by remembering, per key, the last `config` record and, per fact, the `seq` of the record that last set it:

@code replay/replay.go type:history
@code replay/replay.go func:addStep
@code replay/replay.go func:lookup
@code replay/replay.go func:Config

`lookup` tries the bare key, then `flag.<key>`, then `env.<key>`, and takes whichever was recorded latest. A fact recorded as a hash or as unset counts as "not set". `history` is filled only as steps pass (`hist.addStep(st)` at the end of each loop iteration), so the failing step sees exactly what was recorded before it.

## Environment drift

@spec 6.2

Drift is computed once, after the replay, from the genesis facts and the `environment` the host sent in `ready`. The Go host collects them with the same `envfacts` package the recorder uses; other SDKs run `kavach-recorder facts`. `sameFact` compares a hashed fact with a plain one by hashing the plain one, and `computeDrift` skips `flag.` keys entirely and, when the host reported nothing but `host.runtime`, everything else as well.

@code replay/drift.go func:sameFact
@code replay/drift.go func:computeDrift

The change records are listed separately, from `splitSteps`'s `envAfter`, as `EnvChange{After: seq}`. Neither changes the status. Replaying the checked-in demo fixture, which was recorded with neutral facts of a Linux host (`examples/ledger/sdk/regen-fixture.sh`), on a Mac shows what drift looks like:

```
$ kavach replay examples/ledger/testdata/null-amount.kavach --bin /tmp/kvb/ledger-old
fixture   examples/ledger/testdata/null-amount.kavach
service   ledger · start genesis · 34 records · 8 steps replayed
recorded  panic at seq 32: runtime error: invalid memory address or nil pointer dereference
result    still_failing@32
          panic: runtime error: invalid memory address or nil pointer dereference
drift     12 facts differ from the recorded environment
          env.HOME  recorded (unset)  replay /Users/koustav
          host.arch  recorded amd64  replay arm64
          host.cpus  recorded 4  replay 11
          host.hostname  recorded ledger-host  replay Koustavs-MacBook-Pro.local
          host.kernel  recorded 6.8.0  replay 24.6.0
          host.os  recorded linux  replay darwin
          host.tz  recorded UTC  replay Asia/Kolkata
          host.uid  recorded 1000  replay 501
          host.ulimit.nofile  recorded 1048576  replay 61440
          host.ulimit.nproc  recorded unlimited  replay 2666
          ... and 2 more (--json lists them all)
wall      26.3 ms (including process start)
```

`env.HOME` is there because the journal does not name `HOME`, so `hostEnviron` passed the driver's own, and §9.1 says that is reported as drift. The status is the same as on Linux: the bug does not depend on the host.

## Verifying a fix

### The rules

@spec 6.1

`VerifyWith` implements the three rules in order. It replays the recorded journal under both builds. If the new build does not pass, or the journal recorded no failure (`ok`), or variants are disabled, the verdict is simply the new build's status. Otherwise it generates the variants, checks each (in parallel, `Parallel` at a time; the result does not depend on it), and decides:

@code replay/verify.go type:VerifyOptions
@code replay/verify.go func:VerifyWith

The three verdicts are on lines 155–169: `variant_failed(K)@N` when some reproducing variant failed (the lowest ID wins, because `checks` is in ID order); `unverified` when fewer than `MinVariants` reproduce; `fixed` otherwise.

`checkVariant` is rule 2. It replays the variant under the old build and counts it only if the old build stops at the variant's `incident` input with exactly the recorded failure string. Only then does it replay the variant under the new build, and `judge` applies rule 3: the new build must pass (a variant has no markers, so passing means `ok`), and every step before the incident must produce the old build's outputs. The comparison is with the old build, not with the journal, because a variant's inputs never happened and have no recorded outputs.

@code replay/verify.go func:checkVariant
@code replay/verify.go func:judge
@code replay/verify.go func:failureOf

The failure string is what makes "fails the same way" precise. It is `panic: <message>`, `error: <message>`, `invariant: <name>`, or just `crash` (a crash marker's message describes the process, not the bug). A step that times out produces `timeout`, which no recorded failure has, so a timeout never reproduces an incident.

### Why at least ten variants

A replay of the recorded journal can only show that the new build handles one input. A fix that checks `ev.ID == "evt-008"` passes it. Variants ask the question a reviewer would ask: does the fix handle the same bug when it arrives slightly differently? But a variant is evidence only if the old build actually hits the bug on it. If changing `amount` from `null` to `5000` makes the old build succeed, that variant tells us nothing about the fix, so it is ignored. Requiring at least *M* reproducing variants (10 in the reference implementation, `DefaultMinVariants`) means a `fixed` verdict rests on ten or more distinct inputs that each trigger the bug, not on one; and when the fixture is such that few mutations keep the bug alive, the verdict is `unverified` rather than an overconfident `fixed`.

### How variants are made

`Variants` finds the failing step `k`, keeps steps `0..k` (everything after the incident is dropped), and generates five classes of candidates. It then takes them round-robin, one of each class in turn, until 64 (`MaxCandidates`) or the classes run out, so that every class is represented even when one class alone could fill the budget.

@code replay/variants.go type:vstep
@code replay/variants.go func:Variants
@code replay/variants.go func:markerFailure

`buildVariant` renumbers every record from 0, keeps the snapshot and the genesis `environment` record, writes only inputs and reads (no outputs, no markers: the variant never happened), and records in the header which mutation it is, the `seq` of the input expected to fail, and the failure string.

@code replay/variants.go func:buildVariant

The mutation classes:

| Class | Function | What it does |
| --- | --- | --- |
| Field mutations | `fieldMutations`, `nearby` | If the failing input is a JSON object: each field set to up to 3 values the same field takes in other inputs, then to nearby values (string + `"-1"`, boolean flipped, integer + 1 and negated, float doubled), then removed; and up to 2 fields seen in other inputs but absent here added. Interleaved across fields. |
| Moved earlier | `moves` | The failing input placed 1, 2, …, `k` slots earlier. Clock and random reads stay with their slots, so time still moves forward; gateway and config reads travel with their input. |
| Dropped | `drops` | Each earlier input removed, nearest first, then all of them at once. |
| Delivered twice | `duplicates` | Each earlier input delivered twice, as an at-least-once source can. |
| Clock shifted | `clockShifts` | Every clock read shifted by +1h30m, +36h, −5h30m, +366 days. |

@code replay/variants.go func:moves
@code replay/variants.go func:drops
@code replay/variants.go func:duplicates
@code replay/variants.go func:clockShifts
@code replay/variants.go func:fieldMutations
@code replay/variants.go func:nearby

:::note Spec and code disagree here
§6.1 says that from 0.2 variants also perturb the failing step's `gateway` records (response fields mutated, success replaced by a recorded error and the reverse) and its `config` reads (boolean flipped, another recorded value, unset). `replay/variants.go` implements none of these: the only reads it changes are clock values. Gateway and config records are carried along unchanged with their input. A fix whose correctness depends on a gateway response or a flag is therefore checked only against the five classes above.
:::

### The CLI's part

`kavach diff` wraps `VerifyWith` with host replays of both builds, `Parallel` set to the number of CPUs, and then saves each reproducing variant the new build failed to disk, so that it can be replayed on its own:

@code cmd/kavach/main.go func:verifyFix
@code cmd/kavach/main.go func:saveFailingVariants

:::trap Common trap
When `diff` says `variant_failed(K)@N`, `N` is a `seq` in the **variant**, not in the original fixture: variants are renumbered from 0 and hold no outputs, so the same input usually has a smaller `seq`. Replay the saved variant to see it. And never special-case the variant: the next run generates the same variants (generation is deterministic), but a fix that handles variant 6 by name still fails the input class it stands for.
:::

## Sandbox replay: specified, not implemented

Everything above is a **process replay**: the handler runs on the replay machine, and the host it runs on is only compared, as drift. §6.3 specifies a **sandbox replay** that recreates the recorded host in a container or VM and replays the recorded environment changes at the steps where they happened, so that a failure caused by the host (shared memory removed when a user logs out, for example) happens again and a fix to the environment can be verified.

@spec-only 6.3

Its levels (`process`, `container`, `vm`), actuators, repetition (*R* runs and the `unstable@N` status) and environment patches are defined in the subsections of §6.3, which this guide does not reproduce. In the code, only the `process` level exists:

- The driver always sends `"mode": "process"` in `hello` (`startHost`), never answers a gateway with `live`, and has no handling of `observed`.
- `replay.Status` has no `unstable`, and there is no notion of a repeated run.
- The CLI has no flag for a level or an environment patch.
- Hosts: all eleven non-Go SDKs answer a sandbox `hello` with `fatal` ("sandbox mode not supported"); the Go host ignores `mode`.

What *is* implemented of the environment model is what §9.1 and §6.2 ask of a process replay: the host starts with the recorded `env.` variables, and drift and change records are reported.

## The ledger, end to end

This is the README's "Try it", run into `/tmp` instead of the repository. The ledger's `-fixtures` flag names the directory for its journal; crash fixtures go in its `fixtures/` subdirectory. The SDK finds `kavach-recorder` on `PATH`.

```
$ go build -o /tmp/kvb/bin/kavach ./cmd/kavach
$ go build -o /tmp/kvb/bin/kavach-recorder ./cmd/kavach-recorder
$ go build -o /tmp/kvb/ledger-old ./examples/ledger/sdk
$ go build -tags ledgerfix -o /tmp/kvb/ledger-new ./examples/ledger/sdk
$ go build -tags ledgerpartialfix -o /tmp/kvb/ledger-partial ./examples/ledger/sdk
$ export PATH=/tmp/kvb/bin:$PATH
```

The three builds differ in two constants. `fixNullAmount` (tag `ledgerfix`) rejects any event without an amount; `partialFix` (tag `ledgerpartialfix`) rejects only deposits without one, which is the event type seen in the incident:

@code examples/ledger/sdk/ledger.go 46-61

### Record the crash

```
$ /tmp/kvb/ledger-old -in examples/ledger/testdata/events.jsonl -fixtures /tmp/kvb/kv
ledger.entries     {"txn":"935bf10595c802b2","event":"evt-001",...}
...
ledger.entries     {"txn":"5ce57483a19aa18d","event":"evt-007",...}
kavach: wrote fixture /tmp/kvb/kv/fixtures/ledger-20261009T123037Z-2bcbeec4-000032.kavach
        (panic: runtime error: invalid memory address or nil pointer dereference)
kavach: handler panicked: runtime error: invalid memory address or nil pointer dereference
...
$ cp /tmp/kvb/kv/fixtures/ledger-*-000032.kavach /tmp/kvb/incident.kavach
```

The segment `ledger-…-000000.kavach` stays next to it; the fixture is cut from it and named after the failing input's `seq`, 32.

### Inspect and reproduce

```
$ kavach inspect /tmp/kvb/incident.kavach        # abridged
service ledger · start genesis · 34 records
     0  environment  75 facts  (--full to show)
     1  input     file:events.jsonl @ 1  {"id":"evt-001","type":"deposit",...}
     2  clock     2026-10-09T12:30:37.70188Z
     3  rand      8 bytes  935bf10595c802b2
     4  output    ledger.entries  {"txn":"935bf10595c802b2","event":"evt-001",...}
   ...
    19  input     file:events.jsonl @ 5  {"id":"evt-005","type":"withdraw",...}
    20  clock     2026-10-09T12:30:37.702331Z
    21  output    ledger.rejections  {"event":"evt-005","reason":"insufficient funds"}
   ...
    32  input     file:events.jsonl @ 8  {"id":"evt-008","type":"deposit","account":"bob","amount":null}
    33  marker    panic  runtime error: invalid memory address or nil pointer dereference  (+1402 bytes detail)

$ kavach replay /tmp/kvb/incident.kavach --bin /tmp/kvb/ledger-old
service   ledger · start genesis · 34 records · 8 steps replayed
recorded  panic at seq 32: runtime error: invalid memory address or nil pointer dereference
result    still_failing@32
          panic: runtime error: invalid memory address or nil pointer dereference
wall      12.1 ms (including process start)
$ echo $?
1
```

No drift is printed this time: the replay ran on the machine that recorded, seconds later, so every fact matched. The failing step has no reads: the handler dereferences `ev.Amount` before it reads the clock, so record 33 follows the input directly.

### Verify the correct fix

```
$ kavach diff /tmp/kvb/incident.kavach --old /tmp/kvb/ledger-old --new /tmp/kvb/ledger-new
old       still_failing@32
new       fixed
first divergence at step seq 32: output 0 of step 32 differs
  old  (none)
  new  ledger.rejections  {"event":"evt-008","reason":"missing amount"}
variants  41 of 44 reproduce the incident on the old build · 41 of those pass on the new build
verdict   fixed
wall      583.4 ms for 87 replays
```

87 replays is 2 of the fixture, 44 variants under the old build, and the 41 that reproduce under the new one (`replays` in `cmd/kavach/main.go`). The first divergence is in the failing step, as expected: the fix emits a rejection where the old build emitted nothing before panicking.

The 44 candidates are 18 field mutations, 7 moves, 8 drops (7 single, 1 "all"), 7 duplicates and 4 clock shifts. The three that do not reproduce are the ones that give `amount` a value (`null → 5000`, `→ 1200`, `→ 750`): the old build succeeds on them, so they are ignored.

### Reject the narrow fix

```
$ kavach diff /tmp/kvb/incident.kavach --old /tmp/kvb/ledger-old --new /tmp/kvb/ledger-partial --keep /tmp/kvb/variants
old       still_failing@32
new       fixed
first divergence at step seq 32: output 0 of step 32 differs
  old  (none)
  new  ledger.rejections  {"event":"evt-008","reason":"missing amount"}
variants  41 of 44 reproduce the incident on the old build · 37 of those pass on the new build
  ✗  6 failing input (seq 32): "type" "deposit" → "transfer"
       still_failing@23: panic: runtime error: invalid memory address or nil pointer dereference
       saved /tmp/kvb/variants/incident.variant-06.kavach
  ✗ 29 failing input (seq 32): "type" "deposit" → "withdraw"
       ...
  ✗ 37 failing input (seq 32): "type" "deposit" → "deposit-1"
       ...
  ✗ 41 failing input (seq 32): "type" removed
       ...
verdict   variant_failed(6)@23
          variant 6 (failing input (seq 32): "type" "deposit" → "transfer"): still_failing@23: ...
wall      554.3 ms for 87 replays
$ echo $?
1
```

The partial fix passes the recorded incident (`new fixed`) and fails every variant that changes the event's type. `@23` is the incident's `seq` inside the variant: renumbered without outputs, the eighth input lands at 23 instead of 32. With `--json`, each check is listed under `variants.checks` (abridged):

```json
{
  "verdict": "variant_failed(6)@23",
  "old": "still_failing@32",
  "new": "fixed",
  "variants": {
    "required": 10, "candidates": 44, "reproducing": 41, "passing": 37,
    "checks": [
      { "id": 5, "mutation": "clock shifted by +1h30m", "incident_seq": 23,
        "reproduces": true, "old": "still_failing@23", "new": "ok", "passed": true },
      { "id": 6, "mutation": "failing input (seq 32): \"type\" \"deposit\" → \"transfer\"",
        "incident_seq": 23, "reproduces": true, "old": "still_failing@23",
        "new": "still_failing@23", "passed": false,
        "detail": "still_failing@23: panic: runtime error: invalid memory address or nil pointer dereference",
        "file": "/tmp/kvb/variants/incident.variant-06.kavach" },
      { "id": 16, "mutation": "failing input (seq 32): \"amount\" null → 5000",
        "incident_seq": 23, "reproduces": false, "old": "ok", "passed": false }
    ]
  }
}
```

### Replay the variant

A saved variant is an ordinary journal with a `variant` key in its header, so `inspect` and `replay` work on it directly. Every step is lenient and none can diverge:

```
$ kavach replay /tmp/kvb/variants/incident.variant-06.kavach --bin /tmp/kvb/ledger-partial
service   ledger · start genesis · 24 records · 8 steps replayed
variant   failing input (seq 32): "type" "deposit" → "transfer"
result    still_failing@23
          panic: runtime error: invalid memory address or nil pointer dereference
```

## Regression tests with kavachtest

Once a fix is verified, the fixture becomes a test. `kavachtest.Run` replays every fixture matching a glob as a Go subtest, **in process**, through `replay.RunFile`; no binary is built and no host is started, so there is no drift either. A fixture passes when the status is `ok` or `fixed`.

@code sdk/go/kavachtest/kavachtest.go func:Run

The ledger's test suite uses it, guarded so that it runs only on the fixed build:

@code examples/ledger/sdk/ledger_test.go 39-46

:::trap Common trap
`kavachtest.Run` checks only the recorded journal, not variants: it is a regression test, not a verification. Verify with `kavach diff` first, then keep the fixture. And never regenerate a fixture to make this test pass; `regen-fixture.sh` exists to rebuild the demo's checked-in fixture after a format change, not to paper over a failing replay.
:::

:::try Try it
```
go test ./replay -run 'Lenient|Verify|ReplayHost' -v
go test -tags ledgerfix -run RegressionFixtures -v ./examples/ledger/sdk
go test -run VerifyCandidateFixes -v ./examples/ledger/sdk
go build -o /tmp/ledger ./examples/ledger/sdk && go run ./cmd/kavach replay examples/ledger/testdata/null-amount.kavach --bin /tmp/ledger
```
:::
