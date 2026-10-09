# @kavachlabs/kavach (TypeScript / Node)

Kavach SDK for Node >= 22. ESM, zero runtime dependencies. It records what a
handler did (flight recorder, SPEC §3.6/§10) and lets `kavach` replay it
(host protocol, SPEC §9).

## Install and build

```bash
cd sdk/typescript
npm install
npm run build
npm test
```

## API

```ts
import { Recorder, maybeHost, Panic, HandlerError, GatewayError } from "@kavachlabs/kavach";

const handler: Handler = {
  async handle(env, input) {
    const at = env.now();                           // sync, recorded
    const id = env.random(8);                       // sync, recorded
    const flag = env.config("flag.beta");           // sync, Uint8Array | undefined
    const rate = await env.query("fx-rates", req);  // async, rejects with GatewayError
    env.emit("postgres:balances", bytes);           // delivered after the step succeeds
  },
  snapshot() { return bytes; },                     // optional
  restore(data) {},                                 // optional
  invariants() { return [{ name: "x", check() { /* throw on failure */ } }]; }, // optional
};

await maybeHost(() => handler, { gateways });       // first thing in main

const rec = await Recorder.start({ service: "ledger", handler, gateways, deliver });
const result = await rec.step({ source, position, data });  // never throws for handler failures
await rec.flush({ durable: true });
await rec.close();
```

Handlers must use `env` instead of `Date.now`, `Math.random`/`crypto`, `fetch`
and `process.env` for anything that can change what they do. `env` is invalid
after its step ends.

Recorder options: `recorderCommand` (argv; else `KAVACH_RECORDER`; else
`kavach-recorder` on PATH), `required`, `gateways` (`name -> {call, scope}`),
`config`, `flags`, `deliver`, `log`, `snapshot` (snapshot start), journal
settings (`dir`, `compression`, ...), and `clock`/`random` overrides.

If the recorder cannot start, dies, or reports a fatal error, the SDK logs
loudly, stops recording and keeps running steps. `required: true` makes
`Recorder.start` reject instead.

## Failure mapping (SPEC §4.5)

| Handler does | Marker kind | Message |
| --- | --- | --- |
| throws an `Error` | `panic` | `` `${err.name}: ${err.message}` ``, stack as data |
| throws a non-Error `x` | `panic` | `String(x)` |
| throws `new Panic(msg)` | `panic` | exactly `msg`, stack as data |
| throws `new HandlerError(msg)` | `error` | exactly `msg` |
| an invariant fails after an ok step | `invariant` | the invariant's name, failure as data |

The same function is used when recording and when hosting a replay.

## Async and ordering rules

- `now`, `nowNanos`, `random`, `config`, `emit` are synchronous.
- `query` returns a promise, but its journal position is reserved when `query()`
  is called, not when it resolves. `Promise.all([env.query(a), env.query(b)])`
  therefore records `a` then `b` however the responses arrive. Order of reads
  and queries must not depend on timing.
- In host mode `query()` does its request/answer exchange inside the call and
  returns an already settled promise (except a `live` local query in a sandbox:
  await it before making other reads).
- Steps are serialized. The `input` frame is written before the handler runs.
- On a driver `abort` the SDK throws `KavachAbort` into the handler. If the
  handler swallows it, the host still reports `done` with outcome `aborted`.
- In host mode, `console.log`/`info`/`debug` and `process.stdout.write` go to
  stderr; the protocol uses the original fd 1 with synchronous I/O.

## Conformance

The contract lives in the spec directory, `KAVACH_SPEC_DIR` (default: the main
checkout's `spec/`). Needs `python3`.

```bash
python3 $KAVACH_SPEC_DIR/host/run.py --host "node $PWD/dist/conformance/host.js"
node dist/conformance/recorder-case.js $KAVACH_SPEC_DIR/recorder/sdk/reads.json
```

`npm test` runs both suites, plus unit tests.

## Example

`examples/ledger/` is the ledger demo with its `"amount": null` bug:

```bash
node dist/examples/ledger/main.js --in events.jsonl            # buggy
node dist/examples/ledger/main.js --in events.jsonl --fixed    # fixed
```

Behaviour is chosen with flags, not environment variables (those are served
from the journal on replay).
