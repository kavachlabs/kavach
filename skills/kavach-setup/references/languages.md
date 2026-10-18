# Kavach Language SDKs Quick Reference

Every SDK speaks the same wire protocols (`SPEC.md §9` Host Protocol and `SPEC.md §10` Recorder Protocol + `§10.7` Shared-Memory Ring). When `kavach replay` or `kavach diff` runs `--bin "<cmd>"`, it appends `kavach-host` to `<cmd>` and communicates over stdin/stdout pipes.

| Language | SDK Path | Host Entrypoint (First line of `main`) | `Env` Methods |
| --- | --- | --- | --- |
| **Go** | `sdk/go` (`github.com/kavachlabs/kavach/sdk/go`) | `kavach.MaybeHost(newHandler, opts...)` | `env.Now()`, `env.RandBytes(n)`, `env.Read(gw, req)`, `env.Config(k)`, `env.Emit(sink, data)` |
| **Rust** | `sdk/rust` (`kavach`) | `kavach::maybe_host(\|\| Box::new(MyHandler::new()), HostOptions::new())` | `env.now()`, `env.now_nanos()`, `env.fill_random(&mut buf)`, `env.query(gw, req)`, `env.config(k)`, `env.emit(sink, data, Scope)` |
| **Python** | `sdk/python` (`kavach`) | `kavach.maybe_host( lambda: MyHandler() )` | `env.now()`, `env.now_ns()`, `env.random(n)`, `env.query(gw, req)`, `env.config(k)`, `env.emit(sink, data)` |
| **TypeScript / Node** | `sdk/typescript` (`@kavachlabs/kavach`) | `await maybeHost(() => handler, { gateways })` | `env.now()`, `env.random(n)`, `await env.query(gw, req)`, `env.config(k)`, `env.emit(sink, bytes)` |
| **C / C++** | `sdk/c` (`libkavach`, `kavach/kavach.h`, `kavach/kavach.hpp`) | `kavach_maybe_host(argc, argv, &handler_vtable, &opts)` / `kavach::maybe_host(...)` | `kavach_env_now_ns`, `kavach_env_random`, `kavach_env_query`, `kavach_env_config`, `kavach_env_emit` |
| **Java** | `sdk/java` (`com.kavachlabs.kavach`) | `Kavach.maybeHost(args, MyHandler::new, opts)` | `env.now()`, `env.nowNanos()`, `env.random(n)`, `env.query(gw, req)`, `env.config(k)`, `env.emit(sink, bytes)` |
| **Ruby** | `sdk/ruby` (`kavach`) | `Kavach.maybe_host(ARGV) { MyHandler.new }` | `env.now`, `env.random(n)`, `env.query(gw, req)`, `env.config(k)`, `env.emit(sink, data)` |
| **PHP** | `sdk/php` (`Kavach\*`) | `Kavach\maybe_host($argv, fn() => new MyHandler())` | `$env->nowNs()`, `$env->random($n)`, `$env->query($gw, $req)`, `$env->config($k)`, `$env->emit($sink, $data)` |
| **Elixir** | `sdk/elixir` (`Kavach`) | `Kavach.maybe_host(System.argv(), MyHandler)` | `Kavach.Env.now(env)`, `Kavach.Env.random(env, n)`, `Kavach.Env.query(env, gw, req)`, `Kavach.Env.config(env, k)`, `Kavach.Env.emit(env, sink, data)` |
| **OCaml** | `sdk/ocaml` (`Kavach`) | `Kavach.maybe_host (module MyHandler)` | `Kavach.Env.now_ns env`, `Kavach.Env.random env n`, `Kavach.Env.query env ~gateway req`, `Kavach.Env.config env k`, `Kavach.Env.emit env ~sink data` |
| **Haskell** | `sdk/haskell` (`Kavach`) | `maybeHost handlerSpec` (enforced via `Kavach` monad with no `MonadIO`) | `now`, `nowNanos`, `random n`, `query gw req`, `config k`, `emit sink data` |
| **Julia** | `sdk/julia` (`Kavach`) | `Kavach.maybe_host(ARGS, () -> MyHandler())` | `Kavach.now(env)`, `now_ns(env)`, `Kavach.random(env, n)`, `query(env, gw, req)`, `config(env, k)`, `emit!(env, sink, data)` |

## Host Command Examples

- **Go:** `--bin ./ledger` (or `go build -o /tmp/old ./cmd/ledger && kavach replay fixture.kavach --bin /tmp/old`)
- **Python:** `--bin "python3 -m myservice"`
- **TypeScript / Node:** `--bin "node dist/main.js"`
- **Rust:** `--bin ./target/release/myservice`
- **Ruby:** `--bin "ruby -I sdk/ruby/lib app.rb"`
- **Elixir:** `--bin "elixir -r lib/handler.ex run.exs"`
