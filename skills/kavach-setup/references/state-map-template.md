# `.kavach/plan/state-map.md` Template

Before modifying source files to wire a handler or build a `kavach-host` harness, write `.kavach/plan/state-map.md` so the human engineer can review the state boundaries and determinism seams.

```markdown
# State Map: <Service or Subsystem Name>

## 1. Integration Mode
- **Mode:** Direct Handler Instrumentation | Scoped Subsystem Harness (`cmd/kavach-harness`)
- **Language & SDK:** <e.g. Go (`github.com/kavachlabs/kavach/sdk/go`)>
- **Host Command (`--bin`):** `<e.g. ./ledger>`

## 2. State Root & Mutable Structures
| Structure / Field | Location (`file:line`) | Role & Consistency Requirement |
| --- | --- | --- |
| `<Struct.Field>` | `<path/to/file:line>` | <Primary state / secondary index / cache> |

## 3. Transition Entrypoints (`Input`)
| Command / Event | Location (`file:line`) | State Mutated | Failure / Rejection Path |
| --- | --- | --- | --- |
| `<event_type>` | `<path/to/file:line>` | `<fields updated>` | `<how invalid inputs are rejected>` |

## 4. Determinism Boundary (`env` Reads)
Every non-deterministic read inside a step must route through `env` so `kavach replay` and `kavach diff` reproduce the exact execution in microseconds:

| Source of Non-Determinism | Current Call (`file:line`) | Kavach `env` Replacement |
| --- | --- | --- |
| Wall clock / TTL check | `<e.g. time.Now() at file:line>` | `env.Now()` |
| Random ID / nonce / jitter | `<e.g. crypto/rand.Read at file:line>` | `env.Read(buf)` / `env.RandBytes(n)` |
| Outbound query / lookup | `<e.g. db.Get(...) at file:line>` | `env.Query(gateway, req)` |
| Dynamic config / flag | `<e.g. os.Getenv(...) at file:line>` | `env.Config(key)` |

## 5. Captured Side Effects (`env.Emit`)
| Sink Name | Location (`file:line`) | Payload Schema |
| --- | --- | --- |
| `<sink.name>` | `<path/to/file:line>` | `<JSON / binary effect payload>` |

## 6. Snapshots & Host Entrypoint
- **Snapshot / Restore:** `<file:line or N/A>`
- **Host Entrypoint (`kavach-host`):** `<file:line where MaybeReplay / maybe_host is called>`
```
