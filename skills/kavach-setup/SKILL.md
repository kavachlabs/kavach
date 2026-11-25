---
name: kavach-setup
description: >
  Add deterministic record and replay to a service with zero business-logic
  rewrites and zero in-memory DB mocks (`memrepo`), so production or local
  traces replay offline across mutated retry and failure schedules. Use when
  setting up Kavach in a codebase, capturing traffic or crashes for replay,
  making intermittent or prod-only bugs reproducible, or turning incidents
  into PR regression gates. Defaults to v0.2 Driver Capture (`kavach init`,
  `kavach doctor`, `github.com/kavachlabs/kavach-go`, `@kavachlabs/*`) for Go
  and TypeScript/Node 22 services, with a Scoped Subsystem Harness fallback
  (`env.*` / `kavach-host`) for Rust, Python, Java, C, C++, Ruby, PHP, Elixir,
  OCaml, Haskell, Julia, or pure in-memory engines.
compatibility: Requires the `kavach` CLI on PATH (and `kavach-recorder` for production sidecar recording).
metadata:
  version: "0.2.1"
---

# Instrument a Service with Kavach (`kavach-setup`)

Onboard a service to Kavach so it captures PII-scrubbed traces over lock-free `KVRING02` shared memory (`SPEC.md §10.7`) and runs offline as a deterministic `kavach-host` (`SPEC.md §9`) for `kavach replay`, `kavach diff`, and `kavach map`.

## Choose the Integration Mode

Always prefer **Mode 1** when onboarding a Go or TypeScript/Node 22 service with standard database, cache, queue, or HTTP drivers:

1. **Mode 1 (Default — Zero-`memrepo` Driver Capture, Go & TypeScript/Node 22):**
   Wrap existing Postgres, Redis, Kafka, and HTTP clients at initialization (`main.go` / `main.ts`) with thin Kavach driver adapters. **Zero business-logic edits. Zero `memrepo` / `memstore` / fake repository mocks.** The exact same binary records in dev/prod and replays offline with zero external databases or brokers.
2. **Mode 2 (Fallback — Scoped Subsystem Harness / Other Languages):**
   Use explicit `env.*` instrumentation and `MaybeHost` / `maybe_host` for **Rust, Python, Java, C, C++, Ruby, PHP, Elixir, OCaml, Haskell, or Julia**, or when isolating a pure in-memory engine or subsystem inside a large repository (such as Redis, Valkey, Kombu, HTTPX, or Axum) without clean driver boundaries.

---

## Mode 1: Frictionless Driver Capture (Default — Go & TypeScript/Node 22)

### Step 1: Initialize & Audit (`kavach init` + `kavach doctor`)

Run both commands at the repository root before editing any code:

```bash
kavach init
kavach doctor
```

- **`kavach init`** auto-detects language and drivers (`pgx/v5`, `database/sql`, `gorm`, `go-redis/v9`, `franz-go`/`kgo`, `net/http`/`chi`/`gin`/`echo` in Go; `pg`/Drizzle/Prisma, `ioredis`, `kafkajs`, `node:http`/Express/Fastify/Hono/Next.js/`fetch` in TypeScript/Node 22) and scaffolds:
  - `.kavach/config.yaml` — service name, mode, PII scrubbing (`standard`), ring path, default `--variants 25`.
  - `.kavach/invariants.yaml` — starter declarative invariants tailored to detected drivers/topics.
  - `.kavach/map.json` + Day-0 interactive `.kavach/map.html` — static topology of handlers, stores, sinks, and invariants.
  - `.github/workflows/kavach.yml` — offline, zero-DB PR merge gate running `kavach diff`.
- **`kavach doctor`** audits non-determinism call sites that could cause replay drift:
  - **Bare clocks:** `time.Now()` / `time.Since()` / `time.Until()` (Go) or `Date.now()` / `new Date()` / `performance.now()` (TS). Route business-logic timestamps through `kv.Now(ctx)` (Go) or `kv.now()` (TS).
  - **Bare rand / UUID:** `math/rand`, `crypto/rand`, `uuid.New` (Go) or `Math.random()`, `crypto.randomUUID()` (TS). Route business-logic randomness through `kv.Rand(ctx)` (Go) or `kv.rand()` (TS).
  - **Detached contexts (Go):** `context.Background()` / `context.TODO()` inside request/message handlers; propagate the handler's `ctx` so driver calls attach cleanly to the active step.

### Step 2: Wrap Existing Drivers at Client Initialization

Install core (zero external runtime dependencies) plus only the adapter(s) the service actually uses, then wrap clients at startup in `main.go` or `main.ts`. See [`references/languages.md`](./references/languages.md) for all adapter constructors and offline startup helpers (`kv.IsReplay()` / `kv.isReplay()`).

> **Hard Rule (Mode 1):** Do **NOT** rewrite business handlers, do **NOT** build `memrepo` / `memstore` / fake DB mocks, and do **NOT** alter domain logic. Keep every Kavach call at client initialization and server/consumer startup.

#### Go (`github.com/kavachlabs/kavach-go`)

```bash
go get github.com/kavachlabs/kavach-go@v0.2.1
# Add only the adapters your service uses (kavachhttp is included in kavach-go):
go get github.com/kavachlabs/kavach-go/adapters/kavachpgx@v0.2.1
go get github.com/kavachlabs/kavach-go/adapters/kavachredis@v0.2.1
go get github.com/kavachlabs/kavach-go/adapters/kavachkgo@v0.2.1
```

```go
package main

import (
	"log"

	"github.com/kavachlabs/kavach-go"
	"github.com/kavachlabs/kavach-go/adapters/kavachhttp"
	"github.com/kavachlabs/kavach-go/adapters/kavachkgo"
	"github.com/kavachlabs/kavach-go/adapters/kavachpgx"
	"github.com/kavachlabs/kavach-go/adapters/kavachredis"
)

func main() {
	kv := kavach.Init(kavach.Config{
		Service: "events-processor",
		PII:     kavach.ScrubStandard, // honors KAVACH_RING_PATH, KAVACH_HOST=1, .kavach/invariants.yaml
	})
	defer kv.Close()

	// 1-line wraps at client construction — ZERO changes inside handlers/processors:
	pool := kavachpgx.Wrap(kv, rawPgxPool)         // or kavachpgx.WrapConnector / WrapPGXConfig / ReplaySQLDB
	rdb := kavachredis.Wrap(kv, rawRedisClient)    // or kavachredis.NewReplayClient(kv) when kv.IsReplay()
	kafka := kavachkgo.Wrap(kv, rawKafkaClient)    // or kavachkgo.NewOfflineClient(kv) when kv.IsReplay()
	mux := kavachhttp.Wrap(kv, appRouter(pool, rdb, kafka))

	// In prod/dev: runs normal server. Under `kavach replay|diff`: runs offline deterministic host.
	log.Fatal(kv.ServeOrReplay(mux))
}
```

#### TypeScript / Node 22 (`@kavachlabs/*`)

> Always use the **`@kavachlabs/*`** npm scope (`@kavachlabs/core`, `@kavachlabs/http`, `@kavachlabs/pg`, `@kavachlabs/ioredis`, `@kavachlabs/kafkajs`).

```bash
npm install @kavachlabs/core @kavachlabs/http @kavachlabs/pg @kavachlabs/ioredis @kavachlabs/kafkajs
```

```ts
import { init } from "@kavachlabs/core";
import { wrapHandler } from "@kavachlabs/http";
import { createReplayPool, wrapPool } from "@kavachlabs/pg";
import { createReplayRedis, wrapRedis } from "@kavachlabs/ioredis";
import { createReplayKafka, wrapKafka } from "@kavachlabs/kafkajs";

const kv = init({
  service: "events-processor",
  pii: "standard", // honors KAVACH_RING_PATH, KAVACH_HOST=1, .kavach/invariants.yaml
});

// 1-line wraps at client construction — AsyncLocalStorage propagates steps automatically:
const pool = kv.isReplay() ? createReplayPool(kv) : wrapPool(kv, rawPool);
const redis = kv.isReplay() ? createReplayRedis(kv) : wrapRedis(kv, rawRedis);
const kafka = kv.isReplay() ? createReplayKafka(kv) : wrapKafka(kv, rawKafka);
const app = wrapHandler(kv, createApp({ pool, redis, kafka }));

// In prod/dev: serves normally. Under `kavach replay|diff`: runs offline deterministic host.
await kv.serveOrReplay(app);
await kv.close();
```

### Step 3: Capture Local Traces, Inspect Map, and Gate PRs

1. **Capture real PII-scrubbed capsules locally** (no external `kavach-recorder` daemon needed — `kavach dev` injects `KAVACH_RING_PATH`, `KAVACH_MODE=record`, and `KAVACH_PII=standard` and writes `.kavach` capsules on exit):
   ```bash
   # Go:
   kavach dev --cmd "go run ./cmd/server" --out .kavach/capsules/ --pii standard
   # TypeScript / Node 22:
   kavach dev --cmd "node dist/main.js" --out .kavach/capsules/ --pii standard
   ```
   Send realistic traffic (HTTP requests, Kafka events, or existing integration tests) against the running service, then stop cleanly with `Ctrl+C`.

2. **Inspect the recorded flow canvas:**
   ```bash
   kavach map --open
   ```
   (Run `kavach map --auto` if you add new handlers or edit `.kavach/invariants.yaml` to refresh `.kavach/map.json` and `.kavach/map.html`.)

3. **Verify deterministic offline replay** (build the binary or point at `node dist/main.js`, and run twice — both must return identical `ok` with zero `nondeterministic@N` or `diverged@N` errors and zero live DB/Redis/Kafka connections):
   ```bash
   kavach replay .kavach/capsules/<capsule>.kavach --bin ./bin/service
   kavach replay .kavach/capsules/<capsule>.kavach --bin ./bin/service
   ```

4. **Gate every PR across mutated retry and gateway-fault schedules:**
   ```bash
   kavach diff .kavach/capsules/*.kavach --old ./bin/main --new ./bin/pr --variants 25
   ```
   `kavach replay` and `kavach diff` auto-load `.kavach/invariants.yaml` (override with `--invariants FILE`, or `--invariants none` to disable). Use `/kavach-invariants` to refine and mutation-test your `.kavach/invariants.yaml` rules.

5. **Production sidecar recording (optional):** Set `KAVACH_RING_PATH=/dev/shm/kavach/<service>.shm` and `KAVACH_PII=standard` alongside the `kavach-recorder` sidecar. When `KAVACH_RING_PATH` and `KAVACH_HOST` are unset, `kavach.Init` / `init` stays in zero-overhead `off` mode.

---

## Mode 2: Scoped Subsystem Harness / Other Languages (Fallback)

Use Mode 2 for **Rust, Python, Java, C, C++, Ruby, PHP, Elixir, OCaml, Haskell, or Julia**, or when isolating a pure in-memory engine / subsystem without clean driver seams (scaffold with `kavach init --mode scoped` if desired).

### 1. Write `.kavach/plan/state-map.md` (Before Touching Code)

Inspect the target handler or subsystem and write **`.kavach/plan/state-map.md`** following [`references/state-map-template.md`](./references/state-map-template.md) (see [`examples/ledger/.kavach/plan/state-map.md`](../../examples/ledger/.kavach/plan/state-map.md) for a complete example):

- **State Root & Mutable Structures:** Primary structs, maps, caches, or secondary indices that hold domain state, with `file:line` locations.
- **Transition Entrypoints (`Input`):** Commands, events, requests, or messages that trigger a state transition.
- **Determinism Boundary (`env` Reads):**
  - Wall clock / timestamps (`env.Now()` / `env.now()`)
  - Random bytes / UUIDs / jitter (`env.Read()` / `env.RandBytes()` / `env.random()`)
  - Synchronous outbound queries to external services or DBs (`env.Query()` / `env.query()`)
  - Dynamic config or feature flags (`env.Config()` / `env.config()`)
- **Captured Side Effects (`env.Emit` / `env.emit`):** Outbound messages, DB writes, or notifications captured and compared during replay rather than executed against live systems.
- **Snapshots & Host Entrypoint:** `Snapshot()` / `Restore()` hooks and where `kavach-host` is intercepted.

Writing `.kavach/plan/state-map.md` first gives the human engineer a concise artifact to review and feeds directly into `/kavach-invariants` (`.kavach/plan/invariants.md`).

### 2. Wire the Language SDK

Consult the **Mode 2** table in [`references/languages.md`](./references/languages.md) for the exact SDK API and host entrypoint for Go (`sdk/go`), Rust, Python, TypeScript (`sdk/typescript`), Java, C, C++, Ruby, PHP, Elixir, OCaml, Haskell, and Julia.

Ensure the binary or script checks for `kavach-host` as its **very first action in `main`** before opening network sockets, worker pools, or database connections.

### 3. Verify Host Mode & Deterministic Replay

1. Build the host command (or prepare the script invocation).
2. Record a short smoke journal (either by driving a few steps through `Recorder` with `kavach-recorder` on `PATH` and flushing a fixture, or using an existing `.kavach` fixture).
3. Replay the journal twice against the host command:
   ```bash
   kavach replay <journal.kavach> --bin "<host-command>"
   ```
4. Confirm:
   - The verdict is `ok` (for a healthy recording) or `still_failing@N` (for a crash recording).
   - Running `kavach replay` twice in a row produces the exact same verdict with zero `nondeterministic@N` or `diverged@N` errors.
