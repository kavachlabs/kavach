# State Map: Wallet Ledger (`examples/ledger`)

## 1. Integration Mode
- **Mode:** Direct Handler Instrumentation (Production `kavach-recorder` + `kavach-host` Replay)
- **Language & SDK:** Go (`github.com/kavachlabs/kavach/sdk/go`)
- **Host Command (`--bin`):** `./ledger`

## 2. State Root & Mutable Structures
| Structure / Field | Location (`file:line`) | Role & Consistency Requirement |
| --- | --- | --- |
| `Ledger.Balances` (`map[string]int64`) | [`sdk/ledger.go:40`](../../sdk/ledger.go#L40) | Primary per-account balance map; every account balance must remain `>= 0`. |
| `Ledger.Net` (`int64`) | [`sdk/ledger.go:41`](../../sdk/ledger.go#L41) | Running net total of `deposits - withdrawals`; must always equal `sum(Balances)`. |

## 3. Transition Entrypoints (`Input`)
| Command / Event | Location (`file:line`) | State Mutated | Failure / Rejection Path |
| --- | --- | --- | --- |
| `deposit` | [`sdk/ledger.go:64-66`](../../sdk/ledger.go#L64-L66) | `l.Net += amount`, `l.Balances[account] += amount` | Rejects if `ev.Amount == nil` or `*ev.Amount <= 0` (`ledger.rejections`) |
| `withdraw` | [`sdk/ledger.go:67-72`](../../sdk/ledger.go#L67-L72) | `l.Net -= amount`, `l.Balances[account] -= amount` | Rejects with `"insufficient funds"` if `l.Balances[account] < amount` |
| `transfer` | [`sdk/ledger.go:73-80`](../../sdk/ledger.go#L73-L80) | `l.Balances[account] -= amount`, `l.Balances[to] += amount` (`l.Net` unchanged) | Rejects with `"insufficient funds"` if `l.Balances[account] < amount` |

## 4. Determinism Boundary (`env` Reads)
| Source of Non-Determinism | Location (`file:line`) | Kavach `env` Call |
| --- | --- | --- |
| Step timestamp (`at`) | [`sdk/ledger.go:61`](../../sdk/ledger.go#L61) | `env.Now()` |
| 8-byte transaction ID (`txn`) | [`sdk/ledger.go:88`](../../sdk/ledger.go#L88) | `env.Read(txn[:])` |

## 5. Captured Side Effects (`env.Emit`)
| Sink Name | Location (`file:line`) | Payload Schema |
| --- | --- | --- |
| `ledger.entries` | [`sdk/ledger.go:98`](../../sdk/ledger.go#L98) | `Entry{Txn, Event, Account, Delta, Balance, At}` |
| `ledger.rejections` | [`sdk/ledger.go:107`](../../sdk/ledger.go#L107) | `Rejection{Event, Reason}` |

## 6. Snapshots & Host Entrypoint
- **Snapshot / Restore:** [`sdk/ledger.go:135-136`](../../sdk/ledger.go#L135-L136) (`json.Marshal` / `json.Unmarshal` on `*Ledger`)
- **Host Entrypoint (`kavach-host`):** [`sdk/main.go:28`](../../sdk/main.go#L28) (`kavach.MaybeReplay(func() kavach.Handler { return NewLedger() })`)
