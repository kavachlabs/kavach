# `/kavach implement`: contract-first feature work

Status: proposal. Nothing here is implemented.

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
- **`/kavach implement <spec>`**, an agent command (a Claude Code or OpenCode
  slash command, or a skill) that runs the loop below with those tools. It
  ships with the repository next to AGENTS.md.

The CLI isn't called `implement`, because Kavach doesn't implement anything.

## The contract

A directory, `contract/` by default:

| File | Holds |
| --- | --- |
| `traffic.kavach` | A journal recorded on the base build: realistic traffic for the existing behaviour, and inputs that reach the new feature. |
| `contract.toml` | Which steps are **feature steps** (by `seq`, or by a match on the input's JSON); the invariant names the build must declare; the spec file it was written from. |
| `lock` | After approval: hashes of the journal, `contract.toml`, the spec and the source files that declare the invariants. |
| `rounds.jsonl` | One line per `check`: the build, the result, and whether it matches an earlier round's. |

Invariants stay in the service's code, declared through its SDK as now. The
contract lists their names, so `check` fails if one is missing, and the lock
covers the files that declare them.

### Commands

```
kavach contract init    --spec SPEC.md --traffic traffic.kavach [--feature-steps 40-52] [--feature-match '$.argv[0]=="HSETEX"']
kavach contract approve                     # writes contract/lock
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
| `stale_lock` | The contract changed since it was approved. |

It then replays variants of the feature steps (reordered, dropped,
duplicated, clock-shifted, and fields changed, including array elements) and
checks invariants only, since a variant has no expected outputs.

The build passes when there are no regressions, no violations, no unreached
feature steps and no failing variants. `--json` lists every `changed` step
with both outputs. That list is what a reviewer reads, and it's the report
SPEC §11 asks `diff` for.

## The loop

What `/kavach implement` tells the agent:

1. **Write the contract before any code.** Record traffic on the base build.
   Write invariants from the spec only, as an operator would state the
   feature's contract. Mark the feature steps. Run `contract check` with the
   base build on both sides; it must report only `unreached` steps.
2. **Stop and show the contract.** List the traffic, the feature steps and the
   invariants. Wait for the person to approve, then run `contract approve`.
   In the benchmark, the task supplies the contract already locked.
3. **Implement, then `contract check`.** Fix regressions and violations. Read
   each `changed` step against the spec.
4. **Stop when the rules say so.** Done when `check` passes. If `rounds.jsonl`
   shows the same result three rounds in a row, or after eight rounds, stop
   and report what fails instead of trying another theory. Never edit the
   contract to make a check pass. If the contract is wrong, say so and ask.
5. **Report.** The final `check` result, and every `changed` step with one line
   on why the spec calls for it.

## What it needs from Kavach

In order:

1. **Accepted divergences in replay (SPEC §6).** A replay given a set of
   feature steps compares their outputs but doesn't stop there, and serves
   their reads leniently, as it does the failing step's. After a feature step,
   later steps are compared as usual. Conformance cases in `spec/testdata`.
2. **Array-element variants (SPEC §6.1).** A field variant may replace one
   element of an array with the value at the same index in another input.
   This also helps incidents: redis-10764 and redis-11734 would have had
   variants that reach the arguments the narrow fixes miss.
3. **`kavach contract`** and the MCP tool `kavach_contract_check`.
4. **The command text,** and a "Building a feature" section in AGENTS.md.

Step 1 is the prerequisite. Without it, `check` can't continue past the
first feature step.

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

- **Cascades.** A feature step that changes state can change later outputs on
  existing behaviour, for example a later `GET` of a key the new command
  wrote. These steps would report `regression` though they are intended. The
  options are to report them separately as `downstream@N` (weaker), or to
  require traffic that keeps the feature's state apart (more work for the
  contract author).
- **Feature steps on the base build.** They often fail there, for example
  with an unknown command. Should those failures be recorded as markers, or
  ignored for feature steps?
- **Where the command lives.** In this repository, in each SDK, or as a
  plugin for each agent.
