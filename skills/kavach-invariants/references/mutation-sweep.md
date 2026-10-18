# Millisecond Invariant Mutation Sweep Reference

Unlike VM-level mutation testing—which requires rebuilding container images and running 30-minute simulations per mutant—Kavach replays a journal in microseconds (`61.1 µs`) and runs 56 variants in `5.9 ms`. You can falsify an entire catalog of invariants locally in under a second.

## Designing Good Mutants

| Invariant Category | Good Realistic Mutant | Bad Mutant (Avoid) |
| --- | --- | --- |
| **Conservation (`conservation`)** | Credit recipient on `transfer` without debiting sender | Panic unconditionally on line 1 (tests crash recovery, not the invariant) |
| **Non-negativity (`non_negative`)** | Remove `balance < amount` guard on `withdraw` | Set `balance = -999999` in `init()` |
| **Capacity bound (`room_capacity`)** | Check capacity on `reserve` but skip check on `extend` | Change constant `MAX_CAP` inside the invariant itself |
| **Deduplication (`idempotent_charge`)** | Only deduplicate when the duplicate is immediately adjacent (`prevID == id`) | Delete the dedup map entirely so it fails to compile |
| **Expiration / TTL (`no_expired_reads`)** | Use `<` instead of `<=` on expiry boundary timestamp | Return a syntax error |

## Diagnosing a Surviving Mutant

When `/tmp/mutant-X` survives `kavach diff`:

1. **Check whether any step in the journal reaches the mutated branch.** If not, record a journal step that exercises that command/event.
2. **Check whether the mutant only fails under a specific ordering or boundary value.** Notice whether `kavach diff`'s variant generator (which shifts clocks, reorders steps, redelivers events, and mutates fields) killed the mutant via `variant_failed(K)@N` even when step-by-step replay of the unperturbed journal passed.
3. **Check the invariant's scope.** If the invariant only inspects the last-touched key instead of scanning the handler's state map, broaden the invariant check so cross-key corruption cannot hide.
