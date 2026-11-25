# Kavach Language SDKs Quick Reference

Every Kavach SDK speaks the same wire protocols (`SPEC.md §9` Host Protocol and `SPEC.md §10` Recorder Protocol + `§10.7` `KVRING02` Shared-Memory Ring). When `kavach replay` or `kavach diff` runs `--bin "<cmd>"`, it appends `kavach-host` to `<cmd>` and communicates over stdin/stdout pipes.

---

## Mode 1: Frictionless Driver-Capture SDKs (Zero-`memrepo`, Recommended)

Wrap your existing database, cache, queue, and HTTP drivers at initialization—zero handler rewrites and zero custom in-memory DB mocks.

### Go (`github.com/kavachlabs/kavach-go`)

Core `github.com/kavachlabs/kavach-go` has **zero third-party dependencies** (and includes `adapters/kavachhttp`). Install only the additional driver adapter modules your service uses:

```bash
go get github.com/kavachlabs/kavach-go@v0.2.1
go get github.com/kavachlabs/kavach-go/adapters/kavachpgx@v0.2.1
go get github.com/kavachlabs/kavach-go/adapters/kavachredis@v0.2.1
go get github.com/kavachlabs/kavach-go/adapters/kavachkgo@v0.2.1
```

| Package | Libraries | Key Exports | Behavior (Record / Replay) |
| --- | --- | --- | --- |
| `github.com/kavachlabs/kavach-go` | Core runtime | `Init`, `kv.Now(ctx)`, `kv.Rand(ctx)`, `kv.Invariant`, `kv.LoadInvariants`, `kv.IsReplay`, `kv.ServeOrReplay`, `kv.ServeHost`, `kv.Close` | Auto-detects `ModeReplay` (`KAVACH_HOST=1` / `kavach-host`), `ModeRecord` (`KAVACH_RING_PATH`), or zero-overhead `ModeOff`; scrubs PII via HMAC-SHA256 |
| `.../adapters/kavachhttp` | `net/http`, `chi`, `gin`, `echo`, `http.Client` | `Wrap`, `WrapWithOptions`, `Middleware`, `WrapClient`, `WrapTransport` | Captures inbound HTTP (`Input`), outbound HTTP (`Gateway`), response (`Output`); offline replay via `httptest` (zero TCP sockets) |
| `.../adapters/kavachpgx` | `pgx/v5`, `database/sql`, `gorm`, `ent` | `Wrap`, `NewPool`, `NewPGXPool`, `WrapPGXConfig`, `OpenSQL`, `WrapConnector`, `WrapStatefulConnector`, `WrapSQLDB`, `ReplaySQLDB` | `SELECT` → `Gateway` (`pg:select`), `INSERT`/`UPDATE`/`DELETE` → `Output` (`pg:exec`); offline replay + `SQLSTATE 40001` fault injection |
| `.../adapters/kavachredis` | `go-redis/v9` | `Wrap`, `NewReplayClient`, `NewHook` | Records `(cmd, key)` replies as `Gateway`; offline startup `PING` → `PONG` + `LOADING` fault injection |
| `.../adapters/kavachkgo` | `twmb/franz-go` (`kgo`) | `Wrap`, `NewClient`, `NewOfflineClient`, `WrapHandler`, `WrapBatchConsumer`, `NewRecordHook` | Captures `kafka:<topic>` `Input` & `Output`; zero broker socket in Replay |

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
		Service: "billing-service",
		PII:     kavach.ScrubStandard,
	})
	defer kv.Close()

	// Skip live TCP dials in Replay mode; wrap real clients in Record/Off mode:
	var (
		db    = kavachpgx.ReplaySQLDB(kv)
		rdb   = kavachredis.NewReplayClient(kv)
		kafka = kavachkgo.NewOfflineClient(kv)
	)
	if !kv.IsReplay() {
		db = kavachpgx.WrapSQLDB(kv, rawSQLDB) // or kavachpgx.Wrap(kv, rawPgxPool) / WrapPGXConfig
		rdb = kavachredis.Wrap(kv, rawRedis)
		kafka = kavachkgo.Wrap(kv, rawKgoClient)
	}

	mux := kavachhttp.Wrap(kv, newRouter(db, rdb, kafka))
	log.Fatal(kv.ServeOrReplay(mux))
}
```

### TypeScript / Node 22 (`@kavachlabs/*`)

Core `@kavachlabs/core` has **zero external runtime dependencies** (Node 22 stdlib only) and uses `AsyncLocalStorage` for automatic step propagation. Always install from the **`@kavachlabs/*`** scope:

```bash
npm install @kavachlabs/core @kavachlabs/http @kavachlabs/pg @kavachlabs/ioredis @kavachlabs/kafkajs
```

| Package | Libraries | Key Exports | Behavior (Record / Replay) |
| --- | --- | --- | --- |
| `@kavachlabs/core` | Node 22 (`AsyncLocalStorage`) | `init`, `kv.now()`, `kv.rand()`, `kv.invariant()`, `kv.loadInvariants()`, `kv.isReplay()`, `kv.serveOrReplay()`, `kv.serveHost()`, `kv.close()` | Lock-free `KVRING02` writer, HMAC-SHA256 PII scrubbing, SPEC §9 `kavach-host` loop, two-tier gateway matcher, `.kavach/invariants.yaml` evaluator |
| `@kavachlabs/http` | `node:http`, Express, Fastify, Hono, Next.js, `fetch` | `wrapHandler`, `createMiddleware`, `wrapFetch` | Captures inbound HTTP (`Input`), outbound `fetch` (`Gateway`), response (`Output`); offline replay with zero TCP socket |
| `@kavachlabs/pg` | `pg` (`node-postgres`), Drizzle (`drizzle(pool)`), Prisma (`@prisma/adapter-pg`) | `wrapPool`, `wrapClient`, `createReplayPool`, `createReplayClient` | `SELECT` → `Gateway` (`pg:select`), `INSERT`/`UPDATE`/`DELETE` → `Output` (`pg:exec`); offline replay + `PG_40001` fault injection |
| `@kavachlabs/ioredis` | `ioredis` | `wrapRedis`, `createReplayRedis` | Timestamp-independent `(cmd, key)` fingerprinting; offline startup `ping()` → `'PONG'` + `LOADING` fault injection |
| `@kavachlabs/kafkajs` | `kafkajs` | `wrapKafka`, `wrapProducer`, `wrapConsumer`, `createReplayKafka` | Captures `kafka:<topic>` `Input` & `Output`; zero broker socket in Replay; retryable error on thrown/uncommitted `eachMessage`/`eachBatch` |

```ts
import { init } from "@kavachlabs/core";
import { wrapHandler } from "@kavachlabs/http";
import { createReplayPool, wrapPool } from "@kavachlabs/pg";
import { createReplayRedis, wrapRedis } from "@kavachlabs/ioredis";
import { createReplayKafka, wrapKafka } from "@kavachlabs/kafkajs";

const kv = init({
  service: "billing-service",
  pii: "standard",
});

// Skip live sockets in Replay mode; wrap real clients in Record/Off mode:
const pool = kv.isReplay() ? createReplayPool(kv) : wrapPool(kv, rawPool);
const redis = kv.isReplay() ? createReplayRedis(kv) : wrapRedis(kv, rawRedis);
const kafka = kv.isReplay() ? createReplayKafka(kv) : wrapKafka(kv, rawKafka);

// HTTP server example:
const app = wrapHandler(kv, createExpressApp({ pool, redis, kafka }));
await kv.serveOrReplay(app);

// Or Kafka consumer example:
// const consumer = kafka.consumer({ groupId: "billing-group" });
// await consumer.connect();
// await consumer.subscribe({ topic: "events_raw" });
// await consumer.run({ eachMessage: async ({ message }) => { ... } });
// await kv.serveOrReplay(consumer);

await kv.close();
```

---

## Mode 2: Explicit `env` / `kavach-host` SDKs (All 13 Languages)

Use these SDKs for languages without v0.2 driver adapters or when building a scoped subsystem harness inside a large repository.

| Language | SDK Path | Host Entrypoint (First line of `main`) | `Env` Methods |
| --- | --- | --- | --- |
| **Go (explicit `env`)** | `sdk/go` (`github.com/kavachlabs/kavach/sdk/go`) | `kavach.MaybeHost(newHandler, opts...)` | `env.Now()`, `env.RandBytes(n)`, `env.Read(gw, req)`, `env.Config(k)`, `env.Emit(sink, data)` |
| **Rust** | `sdk/rust` (`kavach`) | `kavach::maybe_host(\|\| Box::new(MyHandler::new()), HostOptions::new())` | `env.now()`, `env.now_nanos()`, `env.fill_random(&mut buf)`, `env.query(gw, req)`, `env.config(k)`, `env.emit(sink, data, Scope)` |
| **Python** | `sdk/python` (`kavach`) | `kavach.maybe_host(lambda: MyHandler())` | `env.now()`, `env.now_ns()`, `env.random(n)`, `env.query(gw, req)`, `env.config(k)`, `env.emit(sink, data)` |
| **TypeScript (explicit `env`)** | `sdk/typescript` (`@kavachlabs/kavach`) | `await maybeHost(() => handler, { gateways })` | `env.now()`, `env.random(n)`, `await env.query(gw, req)`, `env.config(k)`, `env.emit(sink, bytes)` |
| **C / C++** | `sdk/c` (`libkavach`, `kavach/kavach.h`, `kavach/kavach.hpp`) | `kavach_maybe_host(argc, argv, &handler_vtable, &opts)` / `kavach::maybe_host(...)` | `kavach_env_now_ns`, `kavach_env_random`, `kavach_env_query`, `kavach_env_config`, `kavach_env_emit` |
| **Java** | `sdk/java` (`com.kavachlabs.kavach`) | `Kavach.maybeHost(args, MyHandler::new, opts)` | `env.now()`, `env.nowNanos()`, `env.random(n)`, `env.query(gw, req)`, `env.config(k)`, `env.emit(sink, bytes)` |
| **Ruby** | `sdk/ruby` (`kavach`) | `Kavach.maybe_host(ARGV) { MyHandler.new }` | `env.now`, `env.random(n)`, `env.query(gw, req)`, `env.config(k)`, `env.emit(sink, data)` |
| **PHP** | `sdk/php` (`Kavach\*`) | `Kavach\maybe_host($argv, fn() => new MyHandler())` | `$env->nowNs()`, `$env->random($n)`, `$env->query($gw, $req)`, `$env->config($k)`, `$env->emit($sink, $data)` |
| **Elixir** | `sdk/elixir` (`Kavach`) | `Kavach.maybe_host(System.argv(), MyHandler)` | `Kavach.Env.now(env)`, `Kavach.Env.random(env, n)`, `Kavach.Env.query(env, gw, req)`, `Kavach.Env.config(env, k)`, `Kavach.Env.emit(env, sink, data)` |
| **OCaml** | `sdk/ocaml` (`Kavach`) | `Kavach.maybe_host (module MyHandler)` | `Kavach.Env.now_ns env`, `Kavach.Env.random env n`, `Kavach.Env.query env ~gateway req`, `Kavach.Env.config env k`, `Kavach.Env.emit env ~sink data` |
| **Haskell** | `sdk/haskell` (`Kavach`) | `maybeHost handlerSpec` (enforced via `Kavach` monad with no `MonadIO`) | `now`, `nowNanos`, `random n`, `query gw req`, `config k`, `emit sink data` |
| **Julia** | `sdk/julia` (`Kavach`) | `Kavach.maybe_host(ARGS, () -> MyHandler())` | `Kavach.now(env)`, `now_ns(env)`, `Kavach.random(env, n)`, `query(env, gw, req)`, `config(env, k)`, `emit!(env, sink, data)` |

---

## Host Command (`--bin` / `--old` / `--new`) Examples

- **Go (Mode 1 or Mode 2):** `--bin ./bin/service` (e.g., `go build -o /tmp/old ./cmd/service && kavach replay .kavach/capsules/c1.kavach --bin /tmp/old`)
- **TypeScript / Node 22:** `--bin "node dist/main.js"` (or `--bin "node main.ts"` on Node 22 native TypeScript stripping)
- **Python:** `--bin "python3 -m myservice"`
- **Rust:** `--bin ./target/release/myservice`
- **Java:** `--bin "java -cp target/app.jar com.example.Main"`
- **Ruby:** `--bin "ruby -I sdk/ruby/lib app.rb"`
- **Elixir:** `--bin "elixir -r lib/handler.ex run.exs"`
