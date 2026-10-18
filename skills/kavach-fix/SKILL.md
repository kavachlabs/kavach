---
name: kavach-fix
description: >
  Reproduce a production incident from a `.kavach` flight-recorder fixture and
  fix the handler until `kavach diff` (or the `kavach_diff` MCP tool) returns
  `verdict: "fixed"` across all incident perturbations and neighbour regression
  variants. Use whenever fixing a bug captured in a `.kavach` file or verifying
  that a patch is not a narrow single-trace workaround.
compatibility: Requires the `kavach` CLI on PATH (or `kavach mcp` registered).
metadata:
  version: "0.1.2"
---

# Fix an Incident with Kavach (`kavach-fix`)

Use `kavach replay` and `kavach diff` (or the `kavach_list_incidents`, `kavach_replay`, and `kavach_diff` MCP tools) as an objective oracle instead of guessing from logs or stopping after a single unit test turns green.

## Hard Rules

1. **Never edit, regenerate, or delete a `.kavach` fixture** to make a check pass. A fixture is an immutable record of what happened in production.
2. **Always reproduce the failure on the unpatched build first** (`still_failing@N` or `invariant_violated(X)@N`) before editing any code.
3. **Never write a narrow patch** that checks only the recorded event ID, timestamp, or single command branch. `kavach diff` perturbs fields, order, delivery, and clock across 10+ incident variants and up to 48 neighbour regression variants; a narrow patch will be rejected with `variant_failed(K)@N` or `regressed(K)@N`.
4. **Keep the determinism boundary clean:** handlers must read time, random bytes, gateways, and config—and emit effects—only through the SDK's `env`, never via language-native clocks, RNGs, or direct I/O.
5. **Report the final verdict exactly as Kavach prints it.**

## Workflow

### Step 1: Inspect the incident

Find the `.kavach` fixture (via `kavach_list_incidents` or `find . -name "*.kavach"`) and read its causal sequence and failure marker:

```bash
kavach inspect <fixture> --full
```

Note:
- Which input `seq` failed (`N`).
- How it failed (`panic`, `error`, or `invariant`).
- What inputs, gateway responses, config reads, and clock/random values preceded step `N`.

### Step 2: Reproduce on the unpatched (`--old`) build

Create a host command for the code **as it exists before your fix**:
- **Compiled service (Go, Rust, C, C++, Java, Haskell, OCaml):** build a binary outside the working tree, e.g. `go build -o /tmp/old ./cmd/service` or `cargo build --bin service && cp target/debug/service /tmp/old`.
- **Interpreted service (Python, TypeScript/Node, Ruby, PHP, Elixir, Julia):** create an isolated worktree (`git worktree add /tmp/old HEAD`) and point `--old` at the worktree's host command.

Run `kavach replay`:

```bash
kavach replay <fixture> --bin "/tmp/old"
```

Confirm that the output reports `still_failing@N` (or `invariant_violated(X)@N`) matching the recorded failure. **If `kavach replay` does not reproduce the failure on `--old`, stop immediately and report it to the user.**

### Step 3: Fix the handler

Edit the service handler to fix the root cause of the bug:
- Audit sibling branches, command types, and callers that share the same invariant or data shape.
- Route any new clock, random, gateway, or config reads strictly through `env`.

### Step 4: Verify with `kavach diff` across variants and regressions

Build or point `--new` at your patched code and run `kavach diff`:

```bash
kavach diff <fixture> --old "/tmp/old" --new "/tmp/new" --json
```

(Or invoke the `kavach_diff` MCP tool with `fixture`, `old`, and `new`.)

Read [`references/verdicts.md`](./references/verdicts.md) to interpret the verdict:
- **`fixed`**: The recorded failure is gone, all declared invariants hold, every step before `N` matches production outputs, at least 10 reproducing incident variants pass, and up to 48 neighbour regression variants match `--old`. Proceed to Step 5.
- **`variant_failed(K)@M`**: Your fix passed the recorded incident, but failed variant `K` at step `M`. Look up `variants.checks[].file` in the JSON output, replay that saved `.kavach` variant against `--new`:
  ```bash
  kavach replay <saved-variant.kavach> --bin "/tmp/new"
  ```
  Generalize your fix so it handles the broader class of inputs. **Never special-case the variant.**
- **`regressed(K)@M`**: Your fix is too broad and altered behaviour on a valid neighbour input `K` that `--old` handled without failing. Replay the saved regression variant (`regressions.checks[].file`) on both builds with `--json` and compare `steps[].outputs`:
  ```bash
  kavach replay <saved-regression.kavach> --bin "/tmp/old" --json
  kavach replay <saved-regression.kavach> --bin "/tmp/new" --json
  ```
  Narrow your change if `--old` was right according to the specification, or report to the user if `--old` was also buggy on that input.
- **`diverged@M`** or **`nondeterministic@M`**: Your change altered outputs on an earlier step `M < N` or read `env` in a different order. Restore causal compatibility on steps `0..N-1`.

### Step 5: Keep the fixture as a permanent regression test

Copy the `.kavach` fixture into the service's `testdata/` directory and wire `kavach replay <fixture> --bin "<host-cmd>"` (or `kavachtest.Run(t, "testdata/...", handler)` in Go) into the test suite so the bug can never return.
