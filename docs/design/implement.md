# `/kavach-implement`: contract-first feature work

Status: implemented. The pieces are `replay.Options.Features` and
`replay.FeatureVariants` (SPEC.md §6.4), array-element variants (§6.1), the
`contract` package, `kavach contract`, two MCP tools, and
[commands/kavach-implement.md](../../commands/kavach-implement.md). The
benchmark arms at the end aren't built yet.

## Why

[AGENTS.md](../../AGENTS.md) gives an agent a loop for incidents: reproduce,
fix, `diff` until `fixed`. There is no loop for building a feature. The agent
benchmark's feature tasks (track B) put the procedure in prose, in each
task's "Kavach arm only" section, and the first tasks show where that falls
short:

- **The agent improvises the procedure.** In the redis-10068 pilot, MiMo v2.6
  Flash with Kavach once built a wrong theory and looped until it gave up with
  an empty patch. Agents in the earlier pilot runs spent 25 minutes or more in
  Redis's full test suite.
- **`diff` can't say a divergence was intended.** Replay stops at the first
  divergence, so on a feature task a correct and a wrong implementation both
  report `diverged@N` at the first step that reaches the feature. The tasks
  work around this with invariants alone (SPEC §11, "Reporting
  compensations", is the related open question).
- **Variants don't edit inside an argument list.** Field variants change
  top-level JSON fields only. Redis inputs are `{"argv":[...]}`, so no variant
  moves a key to another argument position. Kavach passed narrow fixes in
  redis-10764 and redis-11734 that the hidden tests reject.
- **The same author writes the checks and the code.** In five of the ten
  tasks, Kavach caught a wrong implementation the hidden tests passed, but the
  traffic, the invariants and the wrong fix all came from one agent. Those
  catches aren't independent.

## What it is

Two parts:

- **`kavach contract`**, CLI subcommands and matching MCP tools that do the
  mechanical work: build a contract, lock it, and check a build against it.
  Kavach calls no model.
- **`/kavach-implement <spec>`**, an agent command (a Claude Code or OpenCode
  slash command, or a skill) that runs the loop below with those tools. It
  ships with the repository next to AGENTS.md.

The CLI isn't called `implement`, because Kavach doesn't implement anything.

## The contract

A directory, `contract/` by default:

| File | Holds |
| --- | --- |
| `traffic.kavach` | A journal recorded on the base build: realistic traffic for the existing behaviour, and inputs that reach the new feature. |
| `contract.json` | Which steps are **feature steps** (by `seq`, or by a match on the input's JSON); the invariant names the build must declare; the spec file it was written from. |
| `lock.json` | After approval: hashes of the journal, `contract.json`, the spec and the source files that declare the invariants. |
| `rounds.jsonl` | One line per `check`: the build, the result, and whether it matches an earlier round's. |

Invariants stay in the service's code, declared through its SDK as now. The
contract lists their names, so `check` fails if one is missing, and the lock
covers the files that declare them.

### Commands

```
kavach contract init    --spec SPEC.md --traffic traffic.kavach [--feature-steps 40-52] [--feature-match '$.argv[0]=="HSETEX"']
kavach contract show
kavach contract approve                     # writes contract/lock.json; asks at a terminal, else needs --yes
kavach contract check   --old <host> --new <host> [--json]
```

`check` replays the journal under both builds and sorts every step:

| Result | Meaning |
| --- | --- |
| `regression@N` | A step that isn't a feature step produced different outputs from the base build. |
| `changed@N` | A feature step produced different outputs. Expected; the outputs of both builds are shown. |
| `unreached@N` | A feature step produced the base build's outputs. The feature may not be wired in. |
| `invariant_violated(X)@N` | As in replay. |
| `nondeterministic@N` | As in replay, outside feature steps. |
| `variant_failed(K)@N` | A variant of a feature step failed or broke an invariant. |
| `missing_invariant(X)` | The build doesn't declare a named invariant. |
| `baseline_mismatch(V)` | `--old` doesn't replay the traffic as recorded. |
| `unapproved`, `stale_lock` | The build meets the contract, but no one approved it, or it changed since. |

It then replays variants of the feature steps (reordered, dropped,
duplicated, clock-shifted, and fields changed, including array elements) and
checks invariants only, since a variant has no expected outputs.

The build passes when there are no regressions, no violations, no unreached
feature steps and no failing variants. `--json` lists every `changed` step
with both outputs. That list is what a reviewer reads, and it's the report
SPEC §11 asks `diff` for.

## The loop

What `/kavach-implement` tells the agent:

1. **Write the contract before any code.** Record traffic on the base build.
   Write invariants from the spec only, as an operator would state the
   feature's contract. Mark the feature steps. Run `contract check` with the
   base build on both sides; it must report only `unreached` steps.
2. **Stop and show the contract.** List the traffic, the feature steps and the
   invariants. Wait for the person to run `contract approve`; the agent never
   does.
   In the benchmark, the task supplies the contract already locked.
3. **Implement, then `contract check`.** Fix regressions and violations. Read
   each `changed` step against the spec.
4. **Stop when the rules say so.** Done when `check` passes. If `rounds.jsonl`
   shows the same result three rounds in a row, or after eight rounds, stop
   and report what fails instead of trying another theory. Never edit the
   contract to make a check pass. If the contract is wrong, say so and ask.
5. **Report.** The final `check` result, and every `changed` step with one line
   on why the spec calls for it.

## What it needed from Kavach

In order, all now built:

1. **Accepted divergences in replay (SPEC §6).** A replay given a set of
   feature steps compares their outputs but doesn't stop there, and serves
   their reads leniently, as it does the failing step's. After a feature step,
   later steps are compared as usual.
2. **Array-element variants (SPEC §6.1).** A field variant may replace one
   element of an array with the value at the same index in another input.
   This also applies to incidents. Whether it catches the narrow fixes in
   redis-10764 and redis-11734 depends on the arguments other inputs in those
   fixtures carry. Rerunning their candidates will tell.
3. **`kavach contract`** and the MCP tool `kavach_contract_check`.
4. **The command text,** and a "Building a feature" section in AGENTS.md.

Step 1 is the prerequisite. Without it, `check` can't continue past the
first feature step.

## First results

Measured 2026-10-13 on an Apple M3 Pro, with the task branches' `run.sh`
builds.

**go-genai-streamed-function-args** (feature task, #23). The contract is the
task's committed `traffic.kavach`, `task.md` as the spec, feature steps
`31-55` and the invariant `streamed-call-args`.

| Build | `kavach diff` | `kavach contract check` | Hidden tests |
| --- | --- | --- | --- |
| base commit | `ok` | `unreached@31`; 64 of 64 variants pass | fail |
| `solution` (DeepSWE reference) | `diverged@31` | meets the contract: 13 of 13 feature steps changed, 64 of 64 variants pass | pass |
| `client-scoped-state` (wrong) | `invariant_violated(streamed-call-args)@31` | `invariant_violated(streamed-call-args)@31` | pass |

- **The reference now passes.** `diff` can't tell a correct implementation
  from a wrong one here, because it stops at the first intended change. The
  contract separates them.
- **The cascade rule fired on real traffic.** With only the `partialArgs`
  steps marked, the reference gave `regression@45`: a `chat_send` whose
  request replays the history that a feature step built. Marking it, and step
  49, as feature steps (`31-55`) was right. The check named the step and both
  outputs.
- **Each check took 0.2 s**, including 64 variants.
- **The catch still isn't independent.** One agent wrote the traffic, the
  invariant and the wrong implementation (see the task's README).

**Array-element variants on the incident tasks.** Rerunning the candidates:

| Task | Reproducing variants, before → now | Narrow or wrong fixes that still pass `diff` |
| --- | --- | --- |
| redis-10764 | 37 of 59 → 41 of 64 | `skip-leading-args` |
| redis-11734 | 61 of 64 → 53 of 64 | `unit-only`, `type-before-args`, `missing-as-empty` |

The new variants are generated. In redis-11734, for example, `"argv"[4]
"-1" → "BIT"` puts a non-integer end on the failing input. But none of them
reaches these fixes' gaps. Each gap needs two changes at once (a valid unit
and a bad number on a missing key in 11734; a write to a key at another
argument position in 10764), and a variant changes one element. Combining
mutations is the next step for that, at a cost in variant count.

## What it doesn't fix

- **Integration cost.** It works only where a service already records through
  an SDK. The benchmark's integrations ran from about 340 lines (Redis
  blocking commands) to 531 (the updo driver).
- **Weak contracts.** Thin traffic or vague invariants pass weak code. The
  approval step is the only defence, and it depends on the reviewer.
- **Exact outputs of the new feature.** Invariants state properties. Whether a
  `changed` output is right is a judgement the agent and the reviewer make.
- **Agents that skip the stop.** Nothing prevents an agent from running
  `approve` itself. The lock shows when the contract changed, not who changed
  it.

## Effect on the benchmark

The kavach arm would measure Kavach plus a disciplined workflow, so a win
couldn't be credited to Kavach alone. Two arms would separate them:

- **workflow:** the same contract-first command, with the project's own tests
  in place of `kavach contract check`;
- **kavach-implement:** no contract supplied; the agent builds and locks it
  itself, with an automatic approval. This is closer to real use, and it shows
  whether agents write contracts that catch their own mistakes.

## Open questions

- **Cascades.** *Decided: strict.* A feature step that changes state can
  change later outputs on existing behaviour, such as a later `GET` of a key
  the new command wrote. Such a step must be marked as a feature step, or it
  reports `regression`. Reporting it as `downstream@N` would let unintended
  changes pass as consequences.
- **Feature steps on the base build.** They often fail there, for example
  with an unknown command. Should those failures be recorded as markers, or
  ignored for feature steps?
- **Where the command lives.** In this repository, in each SDK, or as a
  plugin for each agent.
