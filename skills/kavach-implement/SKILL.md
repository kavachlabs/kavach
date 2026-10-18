---
name: kavach-implement
description: >
  Build a new feature contract-first with Kavach (`SPEC.md §6.4`): record
  baseline and feature traffic on the current build, declare invariants from
  the specification, stop for human approval (`kavach contract approve`), and
  implement until `kavach contract check` returns `pass`. Use whenever
  implementing a new feature, command, endpoint, or behaviour change.
compatibility: Requires the `kavach` CLI on PATH (or `kavach mcp` registered).
metadata:
  version: "0.1.2"
---

# Build a Feature Contract-First (`kavach-implement`)

Kavach's contract check (`SPEC.md §6.4`) replays traffic recorded on the current build against your candidate build:
- Steps **outside** the feature must not change (`regression@N`).
- **Feature steps** must change (`unreached@N` if they still behave like the old build).
- **Invariants** written from the specification must hold on the recorded traffic and across perturbed variants of the feature steps (`invariant_violated(X)@N`, `variant_failed(K)@N`).

Follow these four phases in strict order. Do not skip a phase.

---

## Phase 1: Write the Contract Before Any Implementation Code

1. **Read the specification.** If the user gave a prose prompt instead of a file, write a concise specification to `contract-spec.md` first.
2. **Build the current (`--old`) build** as a host command outside the working tree (e.g. `/tmp/old` binary or `git worktree add /tmp/old HEAD`).
3. **Record baseline + feature traffic (`traffic.kavach`) on `--old`:**
   - Include realistic inputs exercising existing behaviour—especially commands/events adjacent to the new feature.
   - Include inputs that reach the new feature, covering every edge case named in the specification.
4. **Declare invariants from the specification alone:**
   - Add invariants to the service handler via its SDK (`env.Invariant` / `invariants()`).
   - Each invariant must express a domain property promised by the specification that holds after every step—never an implementation detail of your planned code.
5. **Initialize the contract:**
   ```bash
   kavach contract init \
     --spec <spec-file> \
     --traffic <traffic.kavach> \
     --feature-match '<jsonpath-expr>' \
     --invariant <name> \
     --invariant-source <file>
   ```
   *Note:* Any later step that reads state written by a feature step is causally a feature step too—include it with `--feature-steps`.
6. **Sanity-check the contract against `--old`:**
   ```bash
   kavach contract check --old "/tmp/old" --new "/tmp/old"
   ```
   Expect `unreached@N` (and `unapproved`) and nothing worse. If you see `baseline_mismatch`, `invariant_violated`, or `regression`, fix the traffic or contract now before asking for approval.

---

## Phase 2: Human Approval Gate (Stop Here)

1. Run `kavach contract show` (or `kavach_contract_show`) and present the contract to the user:
   - List the marked feature steps and explain why each should change.
   - List every declared invariant and state what guarantee it enforces from the spec.
2. Ask the user to review and run:
   ```bash
   kavach contract approve
   ```
3. **Wait for the user.** Never run `kavach contract approve` yourself, never pass `--yes`, and never edit `.kavach` lock files by hand.
   *(If `kavach contract show` already reports `approved` when you start, skip Phases 1 and 2 and never modify the contract files.)*

---

## Phase 3: Implement and Verify Loop

Repeat until `verdict` is `pass`:

1. **Edit the implementation code.** Never modify `kavach.contract.json`, `traffic.kavach`, or the locked `--invariant-source` files—doing so invalidates the cryptographic lock (`stale_lock`).
2. **Build `--new`.**
3. **Run the contract check:**
   ```bash
   kavach contract check --old "/tmp/old" --new "/tmp/new" --json
   ```
   (Or call the `kavach_contract_check` MCP tool.)
4. **Act on the verdict** (see [`references/contract-verdicts.md`](./references/contract-verdicts.md)):
   - `regression@N`: You altered outputs on a non-feature step `N`. Fix your implementation so existing behaviour is preserved. (If step `N` genuinely depends on the feature, stop and explain why to the user; do not edit the contract yourself.)
   - `unreached@N`: Feature step `N` still produces `--old`'s outputs. Wire the feature for that input path.
   - `invariant_violated(X)@N` or `still_failing@N`: Bug at step `N`.
   - `variant_failed(K)@N`: Your implementation fails on a perturbed variant of a feature step. Replay the saved variant (`kavach replay <variant-file> --bin "/tmp/new"`) and generalize your code.
   - `missing_invariant(X)`: Restore the declared invariant `X`.
   - `unapproved` or `stale_lock`: Stop and return to Phase 2 for human approval.
   - `pass`: Audit every entry in `report.changed` (`recorded` vs `replay` outputs) against the specification to confirm no unintended output change slipped through on a feature step.

### Stop Rules

- **Streak limit (`report.streak >= 3`):** The same non-pass verdict three checks in a row means your current approach is stuck. Stop and report to the user.
- **Round limit (`report.round >= 8`):** Stop after 8 rounds without `pass` and report what fails and what you tried.

---

## Phase 4: Report

Present:
- The exact final verdict (`pass`) and round number (`report.round`).
- Every changed feature step (`seq`, input summary, old output vs. new output, and the spec requirement it satisfies).
- Any aspect of the feature not covered by the contract that a human reviewer should inspect.
