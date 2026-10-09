// A host with small custom handlers for edge-case tests:
//   node host-fixture.js <scenario> kavach-host

import { GatewayError, maybeHost, type Env, type Handler } from "../index.js";

const scenario = process.argv[2];
const u = (s: string) => new Uint8Array(Buffer.from(s));

const handlers: Record<string, Handler> = {
  // Swallows every error, including the abort, and keeps reading.
  swallow: {
    handle(env: Env) {
      for (let i = 0; i < 3; i++) {
        try {
          env.nowNanos();
        } catch {
          /* ignored */
        }
      }
      env.emit("after", u("x"));
    },
  },
  // Fires queries without awaiting between them, then joins.
  fanout: {
    async handle(env: Env) {
      const rs = await Promise.allSettled([env.query("g", u("1")), env.query("g", u("2")), env.query("g", u("3"))]);
      env.emit(
        "res",
        u(rs.map((r) => (r.status === "fulfilled" ? Buffer.from(r.value).toString() : `!${(r.reason as GatewayError).error}`)).join(",")),
      );
    },
  },
  // Throws what the input says.
  throws: {
    handle(env: Env, input) {
      const what = Buffer.from(input.data).toString();
      if (what === "string") throw "just a string";
      if (what === "type") throw new TypeError("null is not an object");
      if (what === "number") throw 7;
      env.emit("ok", u("fine"));
    },
  },
  // Prints in every way a handler might.
  prints: {
    handle(env: Env) {
      console.log("log line");
      console.info("info line");
      process.stdout.write("raw write\n");
      env.emit("x", u("x"));
    },
  },
  // Aborted, then rejects with the abort inside a promise nobody awaits.
  floating: {
    handle(env: Env) {
      void env.query("g", u("1"));
      void env.query("g", u("2")); // aborted by the driver; nobody looks at the rejection
    },
  },
};

const handler = handlers[scenario ?? ""];
if (!handler) {
  console.error(`unknown scenario ${scenario}`);
  process.exit(2);
}
await maybeHost(() => handler);
process.exit(2);
