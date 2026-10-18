---
name: kavach-invariants
description: >
  Discover domain invariants from `.kavach/plan/state-map.md`, write a
  reviewable invariant blueprint to `.kavach/plan/invariants.md`, declare them
  via the Kavach SDK, and falsify (mutation-test) each invariant in
  milliseconds with `kavach replay` and `kavach diff` to prove that every
  invariant catches real state corruption.
compatibility: Requires the `kavach` CLI on PATH.
metadata:
  version: "0.1.2"
---

# Discover and Mutation-Test Invariants (`kavach-invariants`)

The hardest part of property-based and deterministic verification is staring at a codebase and asking: *"What invariants should my service have?"* And even after writing an invariant, seeing it pass on a happy-path trace only proves nothing crashed—not that the invariant would have noticed if state broke.

This skill solves both problems:
1. It maps the codebase into **`.kavach/plan/state-map.md`** and **`.kavach/plan/invariants.md`** before touching handler code, pairing every proposed invariant with the exact one-line code mutation that should violate it.
2. It implements the invariants and **falsifies each one in milliseconds** with `kavach replay` and `kavach diff`, recording the verified kill table back into `.kavach/plan/invariants.md`.

## Step 1: Write `.kavach/plan/invariants.md` (Before Touching Code)

1. Read `.kavach/plan/state-map.md` if it exists; if not, inspect the handler and generate `.kavach/plan/state-map.md` first (see [`../kavach-setup/references/state-map-template.md`](../kavach-setup/references/state-map-template.md)).
2. Identify concrete domain properties that **must hold after every step that does not return an error**:
   - **Conservation laws:** Sum of account balances equals total deposits minus withdrawals (`sum(Balances) == Net`); reserved + available inventory equals total stock.
   - **Bounds & non-negativity:** Balances, capacities, reference counts, and TTLs never go negative or exceed configured ceilings.
   - **Referential & index consistency:** Every item in a secondary index or priority queue exists in the primary store with matching attributes; expired keys are never returned as live.
   - **State-machine monotonicity:** Terminal states (`cancelled`, `settled`, `dead_lettered`) never transition back to active states; sequence numbers and epochs never move backward.
   - **Idempotency & deduplication:** Re-delivering an already processed message ID never applies a state mutation twice.
3. Write **`.kavach/plan/invariants.md`** (follow the template in [`references/mutation-sweep.md`](./references/mutation-sweep.md); see [`examples/ledger/.kavach/plan/invariants.md`](../../examples/ledger/.kavach/plan/invariants.md) for a complete example) listing for each proposed invariant:
   - Stable `snake_case` name and category
   - State fields and `file:line` transitions guarded
   - Exact predicate logic
   - **Planned 1-line falsification mutant** (`file:line` change that must trigger `invariant_violated`)

*Anti-patterns to avoid:*
- Never write tautological invariants that just re-compute the exact line of code the handler just ran.
- Never perform external I/O or advance the clock/RNG inside an invariant check.

## Step 2: Declare Invariants on the Handler

Register each invariant from `.kavach/plan/invariants.md` on the handler with a diagnostic failure message that prints the offending values (e.g. `fmt.Sprintf("account %s has balance %d", a, l.Balances[a])`).

Verify that the current unmutated build passes all declared invariants on a baseline journal:

```bash
kavach replay <baseline.kavach> --bin "/tmp/baseline"
```

Expect `ok` (or `fixed` if replaying against a fixed incident fixture).

## Step 3: Falsify Each Invariant (Millisecond Mutation Sweep)

Follow [`references/mutation-sweep.md`](./references/mutation-sweep.md):

For each declared invariant `X` in `.kavach/plan/invariants.md`:
1. Inject the planned **one-line mutant** in the handler that violates the domain property guarded by `X` (for example: skipping a balance debit on transfer, omitting a sufficient-funds check on withdraw, or failing to evict an expired entry).
2. Build the mutant binary to `/tmp/mutant-X` and **immediately revert the working tree** so the repository never stays dirty.
3. Run `kavach replay` and `kavach diff` against `/tmp/mutant-X`:
   ```bash
   kavach diff <journal.kavach> --old "/tmp/baseline" --new "/tmp/mutant-X" --json
   ```
4. **Evaluate the mutant:**
   - **Killed (`invariant_violated(X)@N` or `variant_failed(K)@N` with invariant `X`):** The invariant and journal/variants actively catch the bug.
   - **Survived (`ok` or `fixed`):** Either (a) the invariant predicate is too weak, or (b) the recorded journal and its variants never exercise the state transition. Strengthen the invariant or record an input step that reaches the transition, then re-test until killed.

## Step 4: Record the Verified Kill Table in `.kavach/plan/invariants.md`

Update the **Falsification Results** table in `.kavach/plan/invariants.md` and present it to the user:
- Invariant name & domain property
- 1-line mutant injected (`file:line`)
- Verdict & step (`invariant_violated(X)@N` or `variant_failed(K)@N`)
- Replay / diff verification time
