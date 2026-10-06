# Using Kavach as an AI coding agent

Kavach gives you an objective check on whether a fix actually fixed a
production incident. Use it instead of reasoning from logs.

A **fixture** (`*.kavach`) is a recording of what a service did before it
failed: every input, every clock and random read, every output, and a marker
saying how it failed. A **replay binary** is the service's own binary; its
`main` calls `kavach.MaybeReplay`.

## The loop

1. **Read the incident.**
   `kavach inspect <fixture>` prints each record; add `--full` for the panic
   stack and untruncated data, or `--json` to parse it.
2. **Reproduce it before changing code.**
   `go build -o /tmp/old ./path/to/service`, then
   `kavach replay <fixture> --bin /tmp/old`. Expect `still_failing@N`, where `N`
   is the seq of the input that failed. If replay does not reproduce the
   failure, stop and report that; do not guess at a fix.
3. **Fix the handler.** Change code, never the fixture.
4. **Verify.**
   `go build -o /tmp/new ./path/to/service`, then
   `kavach diff <fixture> --old /tmp/old --new /tmp/new --json`.
   The fix is accepted only when `verdict` is `fixed`. Besides the recorded
   incident, `diff` replays at least 10 **variants** of it (the failing input
   with fields changed or removed, moved earlier, earlier inputs dropped or
   redelivered, the clock shifted) that make the old build fail the same way.
   A fix that only handles the exact recorded input fails one of them.
   If the verdict is `variant_failed(K)@N`, the variant is saved (its path is
   under `variants.checks[].file` in the JSON); reproduce it with
   `kavach replay <variant> --bin /tmp/new` and generalize the fix. Never
   special-case the variant.
5. **Keep it as a regression test.** Copy the fixture into the service's
   `testdata/` and run it with `kavachtest.Run` in a Go test.

## Verdicts

| Verdict | Meaning | What to do |
| --- | --- | --- |
| `fixed` | The recorded failure is gone, invariants hold, earlier steps match production, and every variant passes. | Done. |
| `variant_failed(K)@N` | The recorded incident passes, but variant `K` still fails at its input `N`. | Your fix is too narrow. Replay the saved variant. |
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
- Handlers must call `env.Now()`, `env.Read()` and `env.Emit()` instead of
  `time.Now`, `math/rand`/`crypto/rand`, or performing effects directly.
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
| `kavach_diff` | `fixture`, `old`, `new`, `variants` (default 10), `keep` | `verdict`, `passed`, the old and new replays of the fixture, `variants` with every check and saved failing variants |

A verdict that is not a pass is a normal tool result; `isError` is set only
when a tool could not run (missing file, binary without `kavach.MaybeReplay`).

## Coming later

An `llms.txt`.
