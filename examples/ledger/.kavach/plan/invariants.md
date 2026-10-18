# Invariants: Wallet Ledger (`examples/ledger`)

State boundaries mapped in [`state-map.md`](./state-map.md).

## 1. Proposed Invariants

### 1. `balances_non_negative` (Bounds & Non-Negativity)
- **State inspected:** `Ledger.Balances` ([`sdk/ledger.go:114-121`](../../sdk/ledger.go#L114-L121))
- **Transitions guarded:** `withdraw` ([`sdk/ledger.go:67-72`](../../sdk/ledger.go#L67-L72)) and `transfer` ([`sdk/ledger.go:73-80`](../../sdk/ledger.go#L73-L80))
- **Law:** For every account `a` in `Ledger.Balances`, `l.Balances[a] >= 0` after every step.
- **Why it matters:** Catches missing or off-by-one overdraft checks across withdrawals and transfers.
- **Planned falsification mutant:** [`sdk/ledger.go:68`](../../sdk/ledger.go#L68) — omit the `l.Balances[ev.Account] < amount` guard in `withdraw` so overdraft event `evt-005` (`seq 19`) debits `bob` below zero.

### 2. `money_conserved` (Conservation Law)
- **State inspected:** `Ledger.Balances` and `Ledger.Net` ([`sdk/ledger.go:122-131`](../../sdk/ledger.go#L122-L131))
- **Transitions guarded:** `deposit` (`+amount`), `withdraw` (`-amount`), and `transfer` (`-amount` on sender, `+amount` on recipient)
- **Law:** `sum(l.Balances) == l.Net` after every step.
- **Why it matters:** Guarantees internal transfers neither create nor destroy money and that every deposit/withdrawal updates `Balances` and `Net` atomically.
- **Planned falsification mutant:** [`sdk/ledger.go:77`](../../sdk/ledger.go#L77) — skip the sender debit `l.post(env, ev, ev.Account, -amount, at)` during `transfer` so `evt-003` (`seq 9`) credits `bob` without debiting `alice`.

## 2. Falsification Results (Millisecond Mutation Sweep)

Verified against `examples/ledger/testdata/null-amount.kavach`:

| Invariant | 1-Line Mutant Injected (`file:line`) | Verdict | Replay Time |
| --- | --- | --- | --- |
| `balances_non_negative` | [`sdk/ledger.go:68`](../../sdk/ledger.go#L68): skip `l.Balances[ev.Account] < amount` check on `withdraw` | `invariant_violated(balances_non_negative)@19` | `61.1 µs` |
| `money_conserved` | [`sdk/ledger.go:77`](../../sdk/ledger.go#L77): skip sender debit `l.post(..., -amount, ...)` on `transfer` | `invariant_violated(money_conserved)@9` | `61.1 µs` |
