# `kavach-go` — Zero-`memrepo` Deterministic Record & Replay for Go Backends

Wrap your existing Postgres, Redis, Kafka, and HTTP clients at initialization—**zero business-logic rewrites, zero custom in-memory DB mocks**. Capture PII-scrubbed traces over lock-free `KVRING02` shared memory and replay every PR offline across mutated retry and failure schedules.

## 1. Install & initialize

```bash
# Install Kavach CLI
brew install kavachlabs/tap/kavach        # or: curl -fsSL https://www.thekavachlabs.com/install.sh | sh

# Install Go core + thin adapter(s) for your stack
go get github.com/kavachlabs/kavach-go@v0.2.1
go get github.com/kavachlabs/kavach-go/adapters/kavachpgx@v0.2.1
go get github.com/kavachlabs/kavach-go/adapters/kavachredis@v0.2.1
go get github.com/kavachlabs/kavach-go/adapters/kavachkgo@v0.2.1

# Equip your coding agent & scaffold .kavach/
npx skills@latest add kavachlabs/kavach --skill kavach-setup --skill kavach-invariants
kavach init
```

## 2. Wrap your clients in `main.go` (6 lines, zero handler rewrites)

```go
package main

import (
	"github.com/kavachlabs/kavach-go"
	"github.com/kavachlabs/kavach-go/adapters/kavachhttp"
	"github.com/kavachlabs/kavach-go/adapters/kavachkgo"
	"github.com/kavachlabs/kavach-go/adapters/kavachpgx"
	"github.com/kavachlabs/kavach-go/adapters/kavachredis"
)

func main() {
	kv := kavach.Init(kavach.Config{
		Service:  "events-processor",
		RingPath: "/dev/shm/kavach/events.shm", // no-op if unset; replay mode if KAVACH_HOST=1
		PII:      kavach.ScrubStandard,
	})
	defer kv.Close()

	// 1-line wraps at client construction — ZERO changes inside handlers/processors:
	pool  := kavachpgx.Wrap(kv, rawPgxPool)
	rdb   := kavachredis.Wrap(kv, rawRedisClient)
	kafka := kavachkgo.Wrap(kv, rawKafkaClient)
	mux   := kavachhttp.Wrap(kv, appRouter(pool, rdb, kafka))

	// Optional code invariants (or declare in .kavach/invariants.yaml):
	kv.Invariant("single_advance_bill_per_tx", func(s kavach.Step) error {
		return s.AssertAtMostOnceEmit("events_charged_in_advance", "transaction_id")
	})

	// In prod/dev: starts normal server. When invoked by `kavach replay|diff`: runs deterministic host.
	kv.ServeOrReplay(mux)
}
```

## 3. Record locally, inspect topology, gate every PR

```bash
# Record local traffic into .kavach/capsules/ (zero external daemon needed)
kavach dev --cmd "go run ./cmd/events-processor" --out .kavach/capsules/

# Open the interactive handler -> store -> sink -> invariant flow canvas
kavach map --open

# Verify any human or AI PR across 25 mutated retry/failure schedules (offline, zero DBs)
kavach diff .kavach/capsules/*.kavach --old ./bin/main --new ./bin/pr --variants 25
```

## Adapters

Core `github.com/kavachlabs/kavach-go` has **zero third-party dependencies**. Install only the thin adapter(s) your service uses:

| Adapter | Supported Libraries | Record Mode (Prod / Dev) | Replay Mode (`KAVACH_HOST=1`) |
| --- | --- | --- | --- |
| [`adapters/kavachhttp`](adapters/kavachhttp) | `net/http`, `chi`, `gin`, `echo`, `http.RoundTripper` | Captures inbound request (`Input`), clock (`Clock`), outbound HTTP (`Gateway`), response (`Output`) | Drives handler via `httptest` with no TCP socket; replays outbound HTTP offline |
| [`adapters/kavachpgx`](adapters/kavachpgx) | `pgx/v5`, `database/sql`, `gorm` | Executes real SQL; records `SELECT` as `Gateway` and `INSERT`/`UPDATE`/`DELETE` as `Output` | Never dials Postgres; serves recorded rows (or injects `SQLSTATE 40001` faults) |
| [`adapters/kavachredis`](adapters/kavachredis) | `go-redis/v9` | Executes real commands; records `(cmd, key)` replies as `Gateway` | Never dials Redis; serves recorded replies or injects `LOADING` faults on retry |
| [`adapters/kavachkgo`](adapters/kavachkgo) | `twmb/franz-go` (`kgo`) | Captures consumed `kgo.Record` as `Input` and `Produce`/`ProduceSync` as `Output` (`kafka:<topic>`) | Never connects to Kafka; feeds recorded/mutated records and captures emitted outputs |

## Declarative invariants (`.kavach/invariants.yaml`)

Check common billing, queue, and ledger guarantees without writing Go invariant boilerplate:

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
```

Run `kavach doctor` to audit bare `time.Now()`, `math/rand`, and detached `context.Background()` call sites in under 2 seconds.
