---
description: Build a feature contract-first with Kavach - write and get approval for a behaviour contract, then implement until kavach contract check passes
argument-hint: <spec file or feature description>
---

Build this feature contract-first with Kavach: $ARGUMENTS

Kavach's contract check (SPEC.md §6.4) replays traffic recorded on the
current build against your build. Steps outside the feature must not change.
Feature steps must change. Invariants written from the spec must hold, on the
traffic and on variants of the feature steps. Follow these phases in order,
and don't skip one.

## 1. Contract, before any code

- Read the spec. If it is a description rather than a file, write it to
  `contract-spec.md` in your own words and keep it short.
- Build the current code as the **old** build, as a host command: for a
  compiled service, a binary outside the working tree (`/tmp/old`); for an
  interpreted one, a copy of the checkout. The project's SDK README says how
  it starts as a host.
- Record **traffic** on the old build:
  - realistic inputs for the behaviour that already exists, especially near
    the feature;
  - inputs that reach the new feature, including edge cases the spec names.
  - Write down how you generated it.
- Declare **invariants** in the service's code, through its SDK, written from
  the spec alone. Each is a property of state that the spec promises and that
  must hold after every step. Don't write invariants that describe your
  planned implementation.
- Create the contract:
  `kavach contract init --spec <spec> --traffic <journal> --feature-match '<expr>' --invariant <name> --invariant-source <file>`.
  A step that reads what a feature step wrote is a feature step too; add it
  with `--feature-steps`.
- Check it against the old build:
  `kavach contract check --old <old> --new <old>`. Expect `unreached@N` and
  nothing worse. Anything else means the traffic or the contract is wrong; fix
  that now.

## 2. Approval: stop here

Show the person `kavach contract show`. Name the feature steps and the
invariants, and say what each invariant guarantees. Ask them to run
`kavach contract approve`, then wait. Never approve the contract yourself, and
never pass `--yes`.

If the contract was given to you already approved (`kavach contract show`
says `approved`), skip phases 1 and 2. Don't change it.

## 3. Implement and check

Loop:

1. Change the code. Never change the contract, the traffic or the invariants'
   source files. Those changes make the lock stale, and the person sees it.
2. Build the **new** build.
3. Run `kavach contract check --old <old> --new <new> --json` (or the
   `kavach_contract_check` MCP tool).
4. Act on the verdict:
   - `regression@N`: you changed behaviour outside the feature. Fix it. If
     the step really depends on the feature, stop and say so; don't edit the
     contract.
   - `still_failing@N`, `invariant_violated(X)@N`, `nondeterministic@N`: a bug
     in your build at step N.
   - `variant_failed(K)@N`: your build fails a variant of a feature step.
     Replay the saved variant with `kavach replay <file> --bin <new>` and
     generalize. Don't special-case it.
   - `unreached@N`: feature step N behaves as before. The feature isn't
     reached for that input.
   - `missing_invariant(X)`: the build no longer declares X.
   - `unapproved` or `stale_lock`: the build meets the contract, but the
     contract isn't approved or has changed. Go back to phase 2.
   - `pass`: go to phase 4.
5. Read every changed step in `report.changed` (`recorded` vs `replay`
   outputs) against the spec. A change the spec doesn't call for is a bug,
   even if the check passes.

Stop rules:
- When `report.streak` reaches 3, stop. The same outcome three checks in a
  row means your current theory isn't working.
- Stop after 8 rounds without `pass`.
- When you stop, report what fails and what you tried. Don't start a new
  theory without the person.

Run the project's own focused tests for the area as well. Don't use its full
suite as the inner loop.

## 4. Report

- The final verdict, exactly as printed, and the round number.
- Each changed step: its input, the old and new outputs, and the sentence of
  the spec that calls for the change.
- Anything the contract doesn't cover that a reviewer should check.
