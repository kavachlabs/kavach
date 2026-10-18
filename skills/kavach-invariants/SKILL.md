---
name: kavach-invariants
description: >
  Discover domain invariants for a service handler, declare them via the Kavach
  SDK, and falsify (mutation-test) each invariant in milliseconds with
  `kavach replay` and `kavach diff` to prove that every invariant catches real
  state corruption. Use when strengthening a service's verification coverage or
  preparing invariants for a feature contract.
compatibility: Requires the `kavach` CLI on PATH.
metadata:
  version: "0.1.0"
---

# Discover and Mutation-Test Invariants (`kavach-invariants`)

An invariant that passes every run only proves nothing bad happened on that trace—not that the invariant would have noticed if state broke. This skill discovers domain invariants and **falsifies** each one by injecting a realistic mutant and confirming `kavach replay` or `kavach diff` catches it in milliseconds.

## Step 1: Discover Concrete Domain Invariants

Read the handler, domain types, and specification to identify properties that **must hold after every step that does not return an error**:

1. **Conservation laws:** Sum of account balances equals total deposits minus withdrawals; reserved + available inventory equals total stock.
2. **Bounds & non-negativity:** Balances, capacities, reference counts, and TTLs never go negative or exceed configured ceilings.
3. **Referential & index consistency:** Every item in a secondary index or priority queue exists in the primary store with matching attributes; expired keys are never returned as live.
4. **State-machine monotonicity:** Terminal states (`cancelled`, `settled`, `dead_lettered`) never transition back to active states; sequence numbers and epochs never move backward.
5. **Idempotency & deduplication:** Re-delivering an already processed message ID never applies a state mutation twice.

*Anti-patterns to avoid:*
- Never write tautological invariants that just re-compute the exact line of code the handler just ran.
- Never perform external I/O or advance the clock/RNG inside an invariant check.

## Step 2: Declare Invariants on the Handler

Register each invariant with a stable `snake_case` name and a diagnostic failure message that prints the offending values (e.g. `fmt.Sprintf("account %s balance=%d < 0", acct, bal)`).

Verify that the current unmutated build passes all declared invariants on a baseline journal:

```bash
kavach replay <baseline.kavach> --bin "/tmp/baseline"
```

Expect `ok` (or `fixed` if replaying an incident fixture).

## Step 3: Falsify Each Invariant (Millisecond Mutation Sweep)

Follow [`references/mutation-sweep.md`](./references/mutation-sweep.md):

For each declared invariant `X`:
1. Create a realistic **one-line mutant** in the handler that violates the domain property guarded by `X` (for example: skipping a balance debit on transfer, omitting a capacity check on group extension, or failing to evict an expired entry).
2. Build the mutant binary to `/tmp/mutant-X` and **immediately revert the working tree** so the repository never stays dirty.
3. Run `kavach replay` and `kavach diff` against `/tmp/mutant-X`:
   ```bash
   kavach diff <journal.kavach> --old "/tmp/baseline" --new "/tmp/mutant-X" --json
   ```
4. **Evaluate the mutant:**
   - **Killed (`invariant_violated(X)@N` or `variant_failed(K)@N` with invariant `X`):** The invariant and journal/variants actively catch the bug.
   - **Survived (`ok` or `fixed`):** Either (a) the invariant predicate is too weak, or (b) the recorded journal and its variants never exercise the state transition. Strengthen the invariant or record an input step that reaches the transition, then re-test until killed.

## Step 4: Report the Falsification Table

Present a summary table listing every invariant:
- Invariant name & domain property
- Mutant injected
- Verdict & step (`invariant_violated(X)@N` or `variant_failed(K)@N`)
- Replay time
