# `@kavach/*` — Zero-`memrepo` Deterministic Record & Replay for TypeScript / Node 22

Wrap your existing Postgres, Redis, Kafka, and HTTP clients at initialization—**zero business-logic rewrites, zero custom in-memory DB mocks**. Capture PII-scrubbed traces over lock-free `KVRING02` shared memory (with automatic `AsyncLocalStorage` step propagation) and deterministically replay every PR offline across mutated retry and failure schedules.

## 1. Install & initialize

```bash
npm install @kavach/core @kavach/http @kavach/pg @kavach/ioredis @kavach/kafkajs
npx skills@latest add kavachlabs/kavach --skill kavach-setup
kavach init
```

## 2. Wrap your clients in `main.ts` (6 lines, zero handler rewrites)

```ts
import { init } from "@kavach/core";
import { wrapHandler } from "@kavach/http";
import { wrapPool } from "@kavach/pg";
import { wrapRedis } from "@kavach/ioredis";
import { wrapKafka } from "@kavach/kafkajs";

const kv = init({
  service: "events-processor",
  ringPath: process.env.KAVACH_RING_PATH, // no-op if unset; replay mode if KAVACH_HOST=1
  pii: "standard",
});

// 1-line wraps at client construction — ZERO changes inside handlers/processors:
const pool = wrapPool(kv, rawPool);
const redis = wrapRedis(kv, rawRedis);
const kafka = wrapKafka(kv, rawKafka);
const app = wrapHandler(kv, expressApp);

// Optional code invariants (or declare in .kavach/invariants.yaml):
kv.invariant("single_advance_bill_per_tx", (step) =>
  step.assertAtMostOnceEmit("kafka:events_charged_in_advance", "$.transaction_id")
);

// In prod/dev: starts normal server. When invoked by `kavach replay|diff`: runs deterministic host.
await kv.serveOrReplay(app);
```

## 3. Record locally, inspect topology, gate every PR

```bash
# Record local traffic into .kavach/capsules/ (Node 22 runs main.ts natively or dist/main.js)
kavach dev --cmd "node dist/main.js" --out .kavach/capsules/

# Open the interactive handler -> store -> sink -> invariant flow canvas
kavach map --open

# Verify any human or AI PR across 25 mutated retry/failure schedules (offline, zero DBs)
kavach diff .kavach/capsules/*.kavach --old "node base/dist/main.js" --new "node dist/main.js" --variants 25
```

## Packages

Core `@kavach/core` has **zero external runtime dependencies** (Node 22 stdlib only). Install only the thin adapter(s) your service uses:

| Package | Exports | Supported Libraries | Record / Replay Behavior |
| --- | --- | --- | --- |
| [`@kavach/core`](packages/core) | `init`, `kv.now()`, `kv.rand()`, `kv.invariant()`, `kv.serveOrReplay()`, `kv.close()` | Node 22 (`AsyncLocalStorage`) | Lock-free `KVRING02` writer, HMAC-SHA256 PII scrubbing, SPEC §9 `kavach-host` loop, two-tier gateway matcher |
| [`@kavach/http`](packages/http) | `wrapHandler`, `createMiddleware`, `wrapFetch` | `node:http`, Express, Fastify, Hono, Next.js, `fetch` | Captures inbound HTTP (`Input`), outbound `fetch` (`Gateway`), response (`Output`); offline replay with zero TCP socket |
| [`@kavach/pg`](packages/pg) | `wrapPool`, `wrapClient`, `createReplayPool` | `pg` (`node-postgres`), Drizzle (`drizzle(pool)`), Prisma (`@prisma/adapter-pg`) | `SELECT` -> `Gateway` (`pg:select`), `INSERT`/`UPDATE`/`DELETE` -> `Output` (`pg:exec`); replays offline & injects `PG_40001` |
| [`@kavach/ioredis`](packages/ioredis) | `wrapRedis`, `createReplayRedis` | `ioredis` | Timestamp-independent `(cmd, key)` fingerprinting; offline startup `ping()` -> `'PONG'` & `LOADING` fault injection |
| [`@kavach/kafkajs`](packages/kafkajs) | `wrapKafka`, `wrapProducer`, `wrapConsumer`, `createReplayKafka` | `kafkajs` | Captures `kafka:<topic>` `Input` & `Output`; zero broker socket in Replay; signals retryable error on failed `eachMessage` |

## Declarative invariants (`.kavach/invariants.yaml`)

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

Run `kavach doctor` to audit bare `Date.now()`, `new Date()`, `Math.random()`, and `crypto.randomUUID()` call sites in milliseconds, or see [`examples/l1-billing`](examples/l1-billing) for a complete runnable end-to-end example.
