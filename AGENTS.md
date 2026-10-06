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
   The fix is accepted only when the new verdict is `fixed`.
5. **Keep it as a regression test.** Copy the fixture into the service's
   `testdata/` and run it with `kavachtest.Run` in a Go test.

## Verdicts

| Verdict | Meaning | What to do |
| --- | --- | --- |
| `fixed` | The recorded failure is gone, invariants hold, earlier steps match production. | Done. |
| `still_failing@N` | The step of input `N` panicked or returned an error. | Keep working. |
| `invariant_violated(X)@N` | Declared invariant `X` broke after input `N`. | Your fix corrupted state. |
| `diverged@N` | A step before the failure produced different outputs from production. | Your fix changed behavior it should not have. |
| `nondeterministic@N` | The handler read time or randomness differently from the recording. | Read time and randomness only through `env`, in the same order. |
| `ok` | The fixture recorded no failure and replay matched. | Nothing to fix. |

Exit codes: `0` for `fixed` or `ok`, `1` for any other verdict, `2` for usage
errors, `3` when the fixture or binary cannot be used.

## Rules

- Never edit, regenerate or delete a fixture to make a test pass. A fixture is
  a record of what happened in production.
- Handlers must call `env.Now()`, `env.Read()` and `env.Emit()` instead of
  `time.Now`, `math/rand`/`crypto/rand`, or performing effects directly.
- Report the verdict exactly as Kavach prints it.

## Coming later

Checking mutated variants of the incident journal (a fix that only handles the
exact recorded input will be rejected), an MCP server exposing
`kavach_list_incidents`, `kavach_replay` and `kavach_diff`, and an `llms.txt`.
