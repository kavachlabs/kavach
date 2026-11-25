---
name: kavach-invariants
description: >
  Find the rules a stateful service must always obey (at-most-once side effects
  on retry, no silent output drops, balances never negative, money neither
  created nor destroyed), declare them in `.kavach/invariants.yaml` (zero
  boilerplate) or in code (`kv.Invariant` / `kv.invariant`), and mutation-test
  each invariant in milliseconds so it provably catches corrupted state. Use
  when the user asks what could go wrong in a ledger, payment flow, billing
  pipeline, queue consumer, or state machine, wants invariants or property
  checks, or is about to write a Kavach contract. Reads `.kavach/invariants.yaml`
  and `.kavach/plan/state-map.md` from `kavach-setup` when they exist.
compatibility: Requires the `kavach` CLI on PATH.
metadata:
  version: "0.2.1"
---

# Discover, Declare, and Mutation-Test Invariants (`kavach-invariants`)

The hardest part of deterministic verification is staring at a service and asking: *"What invariants must hold?"* And even after writing one, seeing it pass on a happy-path trace only proves nothing crashed—not that the invariant catches real bugs.

Kavach solves both problems:
1. **Declarative `.kavach/invariants.yaml` (Zero-Boilerplate Default):** Enforce the highest-leverage production guarantees (`at_most_once_emit`, `anti_lobotomy`, `non_negative`, `conservation`) purely in YAML—zero handler code changes, evaluated automatically by `kavach replay`, `kavach diff`, `kavach-go`, and `@kavachlabs/core`.
2. **Code Invariants (When Needed):** Register custom Go (`kv.Invariant`), TypeScript (`kv.invariant`), or Mode-2 handler invariants for internal state-structure checks not visible in emitted payloads.
3. **Millisecond Mutation Sweep:** Falsify every invariant against a 1-line mutant using `kavach diff` and record the verified kill table in `.kavach/plan/invariants.md`.

---

## Declarative `.kavach/invariants.yaml` (Default — Zero Boilerplate)

`kavach init` scaffolds `.kavach/invariants.yaml` automatically. Both `kavach replay` and `kavach diff` auto-detect `.kavach/invariants.yaml` (override with `--invariants FILE`, or disable with `--invariants none`), and both `kavach-go` (`Config.InvariantsPath`) and `@kavachlabs/core` (`config.invariantsPath`) load it automatically when present.

### Supported Declarative Rule Kinds

| `kind` | Required / Key Fields | What It Enforces |
| --- | --- | --- |
| `at_most_once_emit` | `sink`, `key` (JSONPath), optional `on_error_emit_count` (int, e.g. `0`) | No key (e.g. `$.transaction_id`) may be emitted to `sink` more than once across the entire session, **including across retries of a failing step**. When `on_error_emit_count: 0`, emitting to `sink` during a step that returns an error or panics immediately violates the invariant (catches non-atomic emit-before-persist bugs). |
| `anti_lobotomy` | `sinks` (list) or `sink`, optional `min_ratio_vs_baseline` (float, default `1.0`) | Blocks "fix by swallowing errors / returning early / dropping work." Fails if a step or session emits fewer outputs to any listed sink than `baseline * min_ratio_vs_baseline`. |
| `non_negative` | `sink`, `field` (JSONPath; `key` accepted as alias) | Every numeric value extracted from `field` (supports nested paths and wildcards like `$.accounts[*].balance`) on matching outputs must be `>= 0`. |
| `conservation` | `sink`, `field` (JSONPath; `key` accepted as alias) | Sum of all numeric values extracted from `field` across matching outputs in a step must equal `0` (tolerance `1e-9`)—catches unbalanced double-entry transfers or money creation/destruction. |

*Sink matching rule:* Exact match (e.g. `kafka:events_charged_in_advance`, `pg:exec`, `http:response`), bare suffix without prefix (e.g. `events_charged_in_advance` matches `kafka:events_charged_in_advance`), or `*` for all sinks.

### Example `.kavach/invariants.yaml`

```yaml
service: events-processor
invariants:
  - name: atomic_single_emit_on_retry
    kind: at_most_once_emit
    sink: kafka:events_charged_in_advance
    key: $.transaction_id
    on_error_emit_count: 0

  - name: no_silent_billing_drop
    kind: anti_lobotomy
    sinks: [kafka:events_enriched, kafka:events_charged_in_advance]
    min_ratio_vs_baseline: 1.0

  - name: balances_non_negative
    kind: non_negative
    sink: pg:exec
    field: $.balance_cents

  - name: ledger_money_conserved
    kind: conservation
    sink: ledger.entries
    field: $.delta_cents
```

---

## Code Invariants (When Internal State Must Be Inspected)

Add code invariants only when a rule inspects in-memory structures (secondary indices, caches, state-machine monotonicity across fields not emitted on a sink) or custom cross-field predicates:

- **Go (`github.com/kavachlabs/kavach-go`):**
  ```go
  kv.Invariant("single_advance_bill_per_tx", func(s kavach.Step) error {
      return s.AssertAtMostOnceEmit("kafka:events_charged_in_advance", "$.transaction_id")
  })
  ```
- **TypeScript (`@kavachlabs/core`):**
  ```ts
  kv.invariant("single_advance_bill_per_tx", (step) =>
    step.assertAtMostOnceEmit("kafka:events_charged_in_advance", "$.transaction_id")
  );
  ```
- **Mode 2 (`sdk/*`):** Register invariant callbacks directly on the handler with a diagnostic error message showing the offending values.

*Anti-patterns to avoid:*
- Never write tautological invariants that just re-compute the exact line of code the handler just ran.
- Never perform external I/O or advance the clock/RNG inside an invariant check.

---

## Step-by-Step Invariant Discovery & Falsification Workflow

### Step 1: Write `.kavach/plan/invariants.md` (Before Touching Code)

1. Read `.kavach/invariants.yaml` and `.kavach/plan/state-map.md` (if present); otherwise inspect the service's handlers, drivers, and output sinks first (run `kavach doctor` for Mode 1 services or follow [`../kavach-setup/references/state-map-template.md`](../kavach-setup/references/state-map-template.md) for Mode 2).
2. Identify concrete domain properties that must always hold:
   - **Idempotency & retry atomicity (`at_most_once_emit` + `on_error_emit_count: 0`):** Re-delivering a message or retrying after a mid-step Redis/Postgres fault never emits duplicate side effects or leaks side effects from failed attempts.
   - **Liveness / anti-lobotomy (`anti_lobotomy`):** Valid inputs keep producing expected downstream messages, HTTP responses, or DB writes instead of being silently swallowed.
   - **Bounds & non-negativity (`non_negative`):** Balances, quantities, capacities, reference counts, and TTLs never go negative.
   - **Conservation laws (`conservation`):** Debits and credits sum to zero; reserved + available inventory equals total stock.
   - **Structural & monotonicity laws (code invariants):** Secondary indices match primary state; terminal states (`cancelled`, `settled`, `dead_lettered`) never regress; sequence numbers never move backward.
3. Write **`.kavach/plan/invariants.md`** following [`references/mutation-sweep.md`](./references/mutation-sweep.md) (see [`examples/ledger/.kavach/plan/invariants.md`](../../examples/ledger/.kavach/plan/invariants.md) for a complete example), pairing each invariant with its **planned 1-line falsification mutant** (`file:line` change that must trigger `invariant_violated`).

### Step 2: Declare the Invariants & Verify Baseline

1. Add all output-observable rules to **`.kavach/invariants.yaml`** first.
2. Add code invariants (`kv.Invariant` / `kv.invariant` / Mode-2 handler invariants) only for properties requiring internal state inspection.
3. Confirm the unmutated build passes all declared invariants on a baseline capsule/journal:
   ```bash
   kavach replay <capsule.kavach> --bin "/tmp/baseline"
   ```
   Expect `ok` (or `fixed` on a fixed incident fixture).

### Step 3: Falsify Each Invariant (Millisecond Mutation Sweep)

Follow [`references/mutation-sweep.md`](./references/mutation-sweep.md):

For each declared invariant `X` in `.kavach/plan/invariants.md`:
1. Inject the planned **one-line mutant** in the service that violates `X` (e.g., emitting before a failing cache/DB call without rollback, swallowing a retry error and returning early, skipping a debit on transfer, or removing a sufficient-funds check).
2. Build or stage the mutant host command (`/tmp/mutant-X`, or an isolated git worktree for TS/Python) and **immediately revert the working tree** so the repository never stays dirty.
3. Run `kavach diff` (which evaluates `.kavach/invariants.yaml`, code invariants, and mutated gateway-fault/retry variants):
   ```bash
   kavach diff <capsule.kavach> --old "/tmp/baseline" --new "/tmp/mutant-X" --variants 25 --json
   ```
4. **Evaluate the mutant:**
   - **Killed (`invariant_violated(X)@N` or `variant_failed(K)@N` naming `X`):** The invariant and capsule/variants actively catch the bug class.
   - **Survived (`ok` or `fixed`):** Either (a) the invariant rule/JSONPath/sink does not match the actual emitted payload, or (b) the recorded capsule and its variants never exercise the mutated branch. Fix the sink/JSONPath or record a capsule step reaching that branch, then re-test until killed.

### Step 4: Record the Verified Kill Table in `.kavach/plan/invariants.md`

Update the **Falsification Results** table in `.kavach/plan/invariants.md` and present it to the user:
- Invariant name, declaration layer (`.kavach/invariants.yaml` vs code), and domain property
- 1-line mutant injected (`file:line`)
- Verdict & step (`invariant_violated(X)@N` or `variant_failed(K)@N`)
- Replay / diff verification time

### Step 5: Refresh the Interactive Flow Map

For Mode 1 repos, regenerate and open the topology map with all declarative invariants attached:

```bash
kavach map --auto
kavach map --open
```

For Mode 2 custom specs, author `.kavach/plan/map.json` (following [`../kavach-setup/references/map-template.md`](../kavach-setup/references/map-template.md)) and render:

```bash
kavach map .kavach/plan/map.json <fixtures> --bin <host> -o .kavach/plan/map.html
```
