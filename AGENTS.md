# Using Kavach as an AI coding agent

> **Stub.** Kavach is pre-alpha. The commands below describe the intended
> workflow; until they exist, treat this file as a design note, not as
> instructions you can run.

Kavach gives you an objective check on whether a fix actually fixed a
production incident. Use it instead of reasoning from logs.

## The loop

1. **Find the incident.** `kavach inspect <fixture>` prints the journal: the
   inputs, the clock and random reads, the outputs, and the marker that shows
   how the step failed.
2. **Reproduce it.** `kavach replay <fixture>` must reproduce the failure
   before you change any code. If it does not, stop and report that; do not
   guess at a fix.
3. **Fix the code.** Change the handler, not the fixture.
4. **Verify.** `kavach diff <fixture> --old <old-bin> --new <new-bin>` shows
   the first output where your build differs from the original. Then run the
   fixtures as ordinary tests with `go test`.

## Rules

- Never edit, regenerate or delete a fixture to make a test pass. A fixture is
  a record of what happened in production.
- A fix counts only when the original failure is gone, declared invariants
  still hold, and the mutated variants of the incident journal also pass.
  Passing the single recorded journal is not enough.
- Report results as Kavach states them: `fixed`, `still_failing`,
  `diverged@N`, or `invariant_violated(X)`.

## Coming later

An MCP server exposing `kavach_list_incidents`, `kavach_replay` and
`kavach_diff`, and an `llms.txt`.
