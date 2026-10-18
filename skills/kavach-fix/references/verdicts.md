# `kavach replay` and `kavach diff` Verdicts Reference

## Verdict Table

| Verdict | Exit Code | Meaning | Required Agent Action |
| --- | --- | --- | --- |
| `fixed` | `0` | Recorded failure is resolved, all invariants hold after every step, steps before the failure match production outputs, all reproducing incident variants pass (minimum 10), and all neighbour regression variants match `--old`. | Done. Report the verdict and variant counts. |
| `ok` | `0` | Fixture recorded no failure and replay matched every output and invariant. | Nothing to fix. |
| `variant_failed(K)@N` | `1` | Recorded incident passes on `--new`, but reproducing incident variant `K` fails at step `N`. | Your fix is too narrow. Inspect `variants.checks[].file`, run `kavach replay <variant-file> --bin <new>`, and generalize the fix. |
| `regressed(K)@N` | `1` | Recorded incident and incident variants pass, but neighbour regression variant `K` produces different outputs from `--old` (or fails) at step `N`. | Your fix broke valid traffic. Run `kavach replay <reg-file> --bin <old> --json` and `--bin <new> --json`, compare `steps[].outputs`, and tighten the fix. |
| `still_failing@N` | `1` | Step `N` panicked or returned a handler error. | Keep working on the root cause. |
| `invariant_violated(X)@N` | `1` | Declared invariant `X` failed after step `N`. | Your fix corrupted handler state. Inspect the invariant failure detail. |
| `diverged@N` | `1` | A step `N` *before* the recorded failure produced different outputs (`emit` sink or bytes) than production recorded. | Your change altered behaviour on healthy historical steps. Preserve exact outputs prior to the failing step. |
| `nondeterministic@N` | `1` | At step `N`, the handler read the clock, RNG, gateway, or config in a different order or shape than recorded. | Ensure all non-deterministic reads go through `env` in the same deterministic sequence on steps before the failure. |
| `unverified` | `1` | Recorded incident passes on `--new`, but fewer than 10 generated variants reproduced the failure on `--old`. | Report `unverified` honestly to the user; do not claim `fixed`. |

## Variant Perturbation Axes (`SPEC.md §6.2`)

When `kavach diff` runs, it systematically mutates the journal across:
1. **Field mutations & removals:** Replacing string/number/boolean/null fields in the failing JSON payload with sibling values from earlier steps, boundary constants (`0`, `-1`, `""`, `null`), or removing keys.
2. **Cross-position & pairwise mutations:** Combining command/event types from earlier steps with the failing payload's edge-case values.
3. **Causal reordering:** Moving the failing step earlier in the journal or replaying it cold (without preceding setup steps).
4. **Delivery anomalies:** Dropping earlier steps or delivering an earlier step twice (testing idempotency).
5. **Virtual clock shifts:** Shifting `env.Now()` across hour, day, negative offset, and leap-year (`+366d`) boundaries.
