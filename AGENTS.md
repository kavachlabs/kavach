# Using Kavach as an AI coding agent

Kavach gives you an objective check on whether a fix actually fixed a
production incident. Use it instead of reasoning from logs.

A **fixture** (`*.kavach`) is a recording of what a service did before it
failed: every input, every clock and random read, every output, and a marker
saying how it failed. A **host command** starts the service's own build in a
mode where the `kavach` CLI drives it over a pipe, in any language: `./ledger`
for a compiled service, `python -m ledger`, `node dist/main.js`. Each SDK's
README under `sdk/` says how its service starts as a host. `--bin`, `--old` and `--new` all take a host command; a
string is split into words like a shell would, and `kavach-host` is appended.

No SDK for your language? [INTEGRATING.md](INTEGRATING.md) shows how to speak
the protocols directly.

## The loop

1. **Read the incident.**
   `kavach inspect <fixture>` prints each record; add `--full` for the panic
   stack and untruncated data, or `--json` to parse it.
2. **Reproduce it before changing code.**
   Make a host command for the code as it is now: for a compiled service, build
   it (`go build -o /tmp/old ./path/to/service`, `cargo build`, ...); for an
   interpreted one, copy the checkout (`git worktree add /tmp/old HEAD`) and
   point the command at the copy. Then
   `kavach replay <fixture> --bin /tmp/old`. Expect `still_failing@N`, where `N`
   is the seq of the input that failed. If replay does not reproduce the
   failure, stop and report that; do not guess at a fix.
3. **Fix the handler.** Change code, never the fixture.
4. **Verify.**
   Make a host command for the changed code the same way (`/tmp/new`, or the
   working tree itself), then
   `kavach diff <fixture> --old /tmp/old --new /tmp/new --json`.
   The fix is accepted only when `verdict` is `fixed`. Besides the recorded
   incident, `diff` replays at least 10 **variants** of it (the failing input
   with fields changed or removed, moved earlier, earlier inputs dropped or
   redelivered, the clock shifted) that make the old build fail the same way.
   A fix that only handles the exact recorded input fails one of them.
   `diff` also replays up to 48 **regression variants**: the same mutations
   applied to earlier inputs that resemble the failing one (they share, for
   example, the command name) and that the old build handled without failing.
   The new build must produce the old build's outputs on them.
   If the verdict is `variant_failed(K)@N`, the variant is saved (its path is
   under `variants.checks[].file` in the JSON); reproduce it with
   `kavach replay <variant> --bin /tmp/new` and generalize the fix. Never
   special-case the variant. If it is `regressed(K)@N`, the saved variant
   (under `regressions.checks[].file`) is an input the old build handled
   without failing. A variant records no outputs, so plain `replay` prints `ok`
   on both builds: run `kavach replay <variant> --bin <build> --json` on each
   and compare `steps[].outputs`. If the specification calls for the old
   build's output, narrow your change; if it calls for yours, the old build was
   wrong there too, so report that instead of changing code.
5. **Keep it as a regression test.** Copy the fixture into the service's
   `testdata/` and have its test suite run
   `kavach replay <fixture> --bin <host command>`, which exits 0 on `fixed`.
   With the Go SDK, `kavachtest.Run` does the same inside `go test`.

## Building a feature

For a feature rather than an incident, the check is a **contract**: traffic
recorded on the build before the feature, the steps of it that reach the
feature, and invariants written from the specification (SPEC.md §6.4). The
`/kavach-implement` command ([commands/kavach-implement.md](commands/kavach-implement.md))
walks through this loop.

1. **Write the contract before any code.** Record traffic on the current build:
   realistic inputs for what exists, and inputs that reach the new feature.
   Declare invariants in the service's code from the specification alone, as
   an operator would state the feature's contract. Then
   `kavach contract init --spec SPEC.md --traffic traffic.kavach --feature-match '$.argv[0]=="NEWCMD"' --invariant NAME --invariant-source FILE`.
   A step that reads what a feature step wrote is a feature step too
   (`--feature-steps`).
2. **Check the contract on the current build.**
   `kavach contract check --old /tmp/old --new /tmp/old` must report
   `unreached@N` and nothing worse.
3. **Stop and ask a person to approve it.** Show `kavach contract show`. Never
   run `kavach contract approve` yourself.
4. **Implement, then check.**
   `kavach contract check --old /tmp/old --new /tmp/new --json`. Fix every
   `regression`, `still_failing`, `invariant_violated` and `variant_failed`;
   read every changed step against the specification.
5. **Stop when the rules say so.** Done at `pass`. When `streak` reaches 3 (the
   same outcome three checks in a row), or after 8 rounds, stop and report what
   fails. Never edit the contract to make a check pass; if it is wrong, say so.
6. **Report** the verdict, and each changed step with why the specification
   calls for it.

| Verdict | Meaning | What to do |
| --- | --- | --- |
| `pass` | Approved contract; no regressions; every feature step changed; invariants hold on the traffic and on every feature variant. | Report the changed steps. |
| `regression@N` | Step `N` is not a feature step, and its outputs changed. | Fix it, or say why the contract should mark it. |
| `unreached@N` | Feature step `N` produced the old build's outputs. | The feature is not wired in for that input. |
| `variant_failed(K)@N` | A variant of a feature step broke an invariant or failed. | Replay the saved variant; generalize. |
| `missing_invariant(X)` | The build does not declare invariant `X`. | Declare it as the specification states it. |
| `baseline_mismatch(V)` | `--old` does not replay the traffic. | Wrong old build, or the traffic is not from it. |
| `unapproved`, `stale_lock` | The build meets the contract, but no one approved it, or it changed since. | Ask the person. |

## Verdicts

| Verdict | Meaning | What to do |
| --- | --- | --- |
| `fixed` | The recorded failure is gone, invariants hold, earlier steps match production, and every variant passes. | Done. |
| `variant_failed(K)@N` | The recorded incident passes, but variant `K` still fails at its input `N`. | Your fix is too narrow. Replay the saved variant. |
| `regressed(K)@N` | The recorded incident and its variants pass, but on regression variant `K` the new build's outputs at input `N` differ from the old build's (or it failed). | Your fix changed behaviour on an input like the failing one that the old build handled without failing. Replay the saved variant (`regressions.checks[].file`) on both builds with `--json`, and check which output the specification calls for before changing code. |
| `unverified` | The recorded incident passes, but fewer than 10 variants reproduce it on the old build. | Report it; do not claim the fix is verified. |
| `still_failing@N` | The step of input `N` panicked or returned an error. | Keep working. |
| `invariant_violated(X)@N` | Declared invariant `X` broke after input `N`. | Your fix corrupted state. |
| `diverged@N` | A step before the failure produced different outputs from production. | Your fix changed behavior it should not have. |
| `nondeterministic@N` | The handler read time or randomness differently from the recording. | Read time and randomness only through `env`, in the same order. |
| `ok` | The fixture recorded no failure and replay matched. | Nothing to fix. |

`still_failing`, `invariant_violated`, `diverged` and `nondeterministic` from
`diff` refer to the recorded fixture; on a replayed variant they refer to the
variant.

Exit codes: `0` for `fixed` or `ok`, `1` for any other verdict, `2` for usage
errors, `3` when the fixture or binary cannot be used.

## Rules

- Never edit, regenerate or delete a fixture to make a test pass. A fixture is
  a record of what happened in production.
- Handlers must read the clock, random bytes, gateways and config, and emit
  effects, only through the env their SDK gives them (in Go, `env.Now()`,
  `env.Read()`, `env.Emit()`), never through the language's own clock or
  random generator, or by performing effects directly.
- Report the verdict exactly as Kavach prints it.

## Over MCP

`kavach mcp` serves the same loop as a Model Context Protocol server on
stdin/stdout. Register it with your client, e.g. for Claude Code:

```bash
claude mcp add kavach -- kavach mcp
```

(This repository's `.mcp.json` does the same with `go run ./cmd/kavach mcp`.)

| Tool | Arguments | Returns |
| --- | --- | --- |
| `kavach_list_incidents` | `dir` (default: working directory) | every `*.kavach` under it, with the recorded failure's kind, seq and message |
| `kavach_replay` | `fixture`, `bin` (default `$KAVACH_BIN`), `include_steps` | `verdict`, `passed`, `detail`, `recorded_failure` |
| `kavach_diff` | `fixture`, `old`, `new`, `variants` (default 10), `regressions` (default 48, 0 skips), `keep` | `verdict`, `passed`, the old and new replays of the fixture, `variants` with every check and saved failing variants, and `regressions` with the same for regression variants |
| `kavach_contract_show` | `dir` (default `./contract`) | the contract, every input with the feature steps marked, and the lock |
| `kavach_contract_check` | `dir`, `new`, `old`, `no_variants` | `verdict`, `passed`, and `report`: changed and unreached steps, variant failures, `round` and `streak` |

There is no tool to approve a contract: a person does that.

A verdict that is not a pass is a normal tool result; `isError` is set only
when a tool could not run (missing file, a host command that does not start as a host).
