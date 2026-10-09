import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { GatewayError, HandlerError, Panic, Recorder, type Env, type Handler, type RecorderOptions } from "../index.js";

const testRecorder = fileURLToPath(new URL("./test-recorder.js", import.meta.url));
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const u = (s: string) => new Uint8Array(Buffer.from(s));

function setup(mode = "normal") {
  const dir = mkdtempSync(join(tmpdir(), "kavach-ts-"));
  const out = join(dir, "frames.jsonl");
  const logs: string[] = [];
  const frames = (): Record<string, any>[] => {
    try {
      return readFileSync(out, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l));
    } catch {
      return [];
    }
  };
  const options = (handler: Handler, extra: Partial<RecorderOptions> = {}): RecorderOptions => ({
    service: "t",
    handler,
    recorderCommand: [process.execPath, testRecorder, out, mode],
    log: (m) => logs.push(m),
    clock: (() => {
      let n = 1000n;
      return () => n++;
    })(),
    ...extra,
  });
  return { dir, out, logs, frames, options, cleanup: () => rmSync(dir, { recursive: true, force: true }) };
}

const shape = (frames: Record<string, any>[]) =>
  frames.map((f) => (f.frame === "record" ? f.type + (f.gateway ? `:${f.request}` : "") : f.frame));

test("Promise.all fan-out records queries in call order, not resolution order", async () => {
  const t = setup();
  const delays: Record<string, number> = { A: 40, B: 0, C: 10 };
  const gateways = {
    g: {
      async call(req: Uint8Array) {
        const k = Buffer.from(req).toString();
        await sleep(delays[k]!);
        if (k === "C") throw new Error("timeout");
        return u("resp-" + k);
      },
    },
  };
  const handler: Handler = {
    async handle(env: Env) {
      const a = env.query("g", u("A"));
      env.nowNanos(); // a read between two queries
      const b = env.query("g", u("B"));
      const c = env.query("g", u("C")).catch((e) => {
        assert.ok(e instanceof GatewayError);
        return u("failed");
      });
      const all = await Promise.all([a, b, c]);
      env.emit("out", all[2]!);
    },
  };
  const rec = await Recorder.start(t.options(handler, { gateways }));
  const r = await rec.step({ source: "s", position: "0", data: u("") });
  assert.equal(r.outcome, "ok");
  await rec.close();
  const fs = t.frames();
  assert.deepEqual(shape(fs), ["open", "facts", "input", "gateway:A", "clock", "gateway:B", "gateway:C", "output", "step_end", "close"]);
  const gw = fs.filter((f) => f.type === "gateway");
  assert.deepEqual(gw.map((g) => [g.response, g.error, g.critical]), [
    ["resp-A", "", true],
    ["resp-B", "", true],
    ["", "timeout", true],
  ]);
  t.cleanup();
});

test("the input frame is written before the handler runs", async () => {
  const t = setup();
  let seen: string[] = [];
  const handler: Handler = {
    async handle() {
      await sleep(250);
      seen = shape(t.frames());
    },
  };
  const rec = await Recorder.start(t.options(handler));
  await rec.step({ source: "s", position: "0", data: u("x") });
  await rec.close();
  assert.deepEqual(seen, ["open", "facts", "input"]);
  t.cleanup();
});

test("failures are recorded as markers; outputs are delivered only after success", async () => {
  const t = setup();
  const delivered: string[][] = [];
  const handler: Handler = {
    handle(env, input) {
      const mode = Buffer.from(input.data).toString();
      env.emit("sink", u(mode));
      if (mode === "panic") throw new Panic("boom");
      if (mode === "error") throw new HandlerError("nope");
      if (mode === "type") throw new TypeError("bad");
      if (mode === "str") throw "plain";
    },
  };
  const rec = await Recorder.start(
    t.options(handler, { deliver: (outs) => void delivered.push(outs.map((o) => Buffer.from(o.data).toString())) }),
  );
  const results = [];
  for (const m of ["ok", "panic", "error", "type", "str"]) {
    results.push(await rec.step({ source: "s", position: m, data: u(m) }));
  }
  await rec.close();
  assert.deepEqual(
    results.map((r) => [r.outcome, r.message]),
    [
      ["ok", undefined],
      ["panic", "boom"],
      ["error", "nope"],
      ["panic", "TypeError: bad"],
      ["panic", "plain"],
    ],
  );
  assert.deepEqual(delivered, [["ok"]]);
  const markers = t.frames().filter((f) => f.type === "marker");
  assert.deepEqual(markers.map((m) => [m.kind, m.message]), [
    ["panic", "boom"],
    ["error", "nope"],
    ["panic", "TypeError: bad"],
    ["panic", "plain"],
  ]);
  assert.match(markers[2]!.data, /TypeError: bad/);
  // The marker precedes step_end and follows the step's outputs.
  const s = shape(t.frames());
  assert.deepEqual(s.slice(s.indexOf("input"), s.indexOf("step_end") + 1), ["input", "output", "step_end"]);
  t.cleanup();
});

test("invariants run after an ok step; the failure is recorded and nothing is delivered", async () => {
  const t = setup();
  let broken = false;
  const delivered: number[] = [];
  const handler: Handler = {
    handle(env, input) {
      broken = Buffer.from(input.data).toString() === "break";
      env.emit("s", u("x"));
    },
    invariants: () => [
      {
        name: "intact",
        check() {
          if (broken) throw new Error("it is broken");
        },
      },
    ],
  };
  const rec = await Recorder.start(t.options(handler, { deliver: (o) => void delivered.push(o.length) }));
  const r = await rec.step({ source: "s", position: "0", data: u("break") });
  assert.equal(r.outcome, "invariant");
  assert.equal(r.message, "intact");
  assert.deepEqual(delivered, []);
  await rec.close();
  const m = t.frames().find((f) => f.type === "marker")!;
  assert.deepEqual([m.kind, m.message], ["invariant", "intact"]);
  assert.match(m.data, /it is broken/);
  t.cleanup();
});

test("a recorder that cannot start is logged loudly and does not fail steps", async () => {
  const logs: string[] = [];
  const handler: Handler = { handle: (env) => env.emit("s", u("x")) };
  const delivered: number[] = [];
  const rec = await Recorder.start({
    service: "t",
    handler,
    recorderCommand: ["/nonexistent/kavach-recorder"],
    log: (m) => logs.push(m),
    deliver: (o) => void delivered.push(o.length),
  });
  assert.equal(rec.isRecording, false);
  assert.match(logs.join("\n"), /WITHOUT RECORDING/);
  const r = await rec.step({ source: "s", position: "0", data: u("") });
  assert.equal(r.outcome, "ok");
  assert.deepEqual(delivered, [1]);
  assert.equal(await rec.flush({ durable: true }), false);
  await rec.close();
});

test("required: true makes startup fail", async () => {
  await assert.rejects(
    Recorder.start({
      service: "t",
      handler: { handle() {} },
      recorderCommand: ["/nonexistent/kavach-recorder"],
      required: true,
      log: () => {},
    }),
    /recording is required/,
  );
});

test("a recorder that dies mid-run stops recording and never fails a step", async () => {
  const t = setup("die");
  const handler: Handler = { handle: (env) => void env.nowNanos() };
  const rec = await Recorder.start(t.options(handler));
  for (let i = 0; i < 5; i++) {
    const r = await rec.step({ source: "s", position: String(i), data: u("") });
    assert.equal(r.outcome, "ok");
    await sleep(30);
  }
  assert.equal(rec.isRecording, false);
  assert.match(t.logs.join("\n"), /RECORDING HAS STOPPED/);
  await rec.close();
  t.cleanup();
});

test("a fatal recorder error stops recording", async () => {
  const t = setup("fatal");
  const rec = await Recorder.start(t.options({ handle() {} }));
  await sleep(200);
  assert.equal(rec.isRecording, false);
  const r = await rec.step({ source: "s", position: "0", data: u("") });
  assert.equal(r.outcome, "ok");
  assert.match(t.logs.join("\n"), /disk full/);
  await rec.close();
  t.cleanup();
});

test("a durable flush that is never answered times out", async () => {
  const t = setup("silent");
  const rec = await Recorder.start(t.options({ handle() {} }, { flushTimeoutMs: 100 }));
  assert.equal(await rec.flush({ durable: true }), false);
  assert.equal(rec.isRecording, true);
  await rec.close().catch(() => {});
  t.cleanup();
});

test("env is dead after its step; rand validates n", async () => {
  const t = setup();
  let leaked: Env | undefined;
  const handler: Handler = {
    handle(env) {
      leaked = env;
      assert.throws(() => env.random(0), RangeError);
      assert.equal(env.random(4).length, 4);
    },
  };
  const rec = await Recorder.start(t.options(handler));
  const r = await rec.step({ source: "s", position: "0", data: u("") });
  assert.equal(r.outcome, "ok");
  assert.throws(() => leaked!.nowNanos(), /after its step/);
  await rec.close();
  t.cleanup();
});

test("config falls back to environment variables and records the source", async () => {
  const t = setup();
  process.env.KAVACH_TS_TEST_VAR = "v1";
  const handler: Handler = {
    handle(env) {
      assert.equal(Buffer.from(env.config("KAVACH_TS_TEST_VAR")!).toString(), "v1");
      assert.equal(env.config("KAVACH_TS_TEST_UNSET"), undefined);
    },
  };
  const rec = await Recorder.start(t.options(handler));
  await rec.step({ source: "s", position: "0", data: u("") });
  await rec.close();
  const cs = t.frames().filter((f) => f.type === "config");
  assert.deepEqual(cs.map((c) => [c.key, c.present, c.value, c.source]), [
    ["KAVACH_TS_TEST_VAR", true, "v1", "env"],
    ["KAVACH_TS_TEST_UNSET", false, "", "env"],
  ]);
  delete process.env.KAVACH_TS_TEST_VAR;
  t.cleanup();
});
