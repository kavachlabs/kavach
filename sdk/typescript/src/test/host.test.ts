import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const fixture = fileURLToPath(new URL("./host-fixture.js", import.meta.url));
const b64 = (s: string) => Buffer.from(s).toString("base64");
const unb64 = (s: string) => Buffer.from(s, "base64").toString();

class Driver {
  private lines: string[] = [];
  private waiting: ((l: string | null) => void)[] = [];
  private ended = false;
  readonly proc: ChildProcess;
  readonly exited: Promise<number | null>;
  stderr = "";

  constructor(scenario: string) {
    this.proc = spawn(process.execPath, [fixture, scenario, "kavach-host"], { stdio: ["pipe", "pipe", "pipe"] });
    this.proc.stderr!.on("data", (d) => (this.stderr += d));
    const rl = createInterface({ input: this.proc.stdout! });
    rl.on("line", (l) => (this.waiting.shift() ?? ((x) => this.lines.push(x!)))(l));
    rl.on("close", () => {
      this.ended = true;
      for (const w of this.waiting.splice(0)) w(null);
    });
    this.exited = new Promise((r) => this.proc.on("exit", (code) => r(code)));
  }

  send(msg: object): void {
    this.proc.stdin!.write(JSON.stringify(msg) + "\n");
  }

  async recv(): Promise<Record<string, any>> {
    const line = this.lines.shift() ?? (this.ended ? null : await new Promise<string | null>((r) => this.waiting.push(r)));
    assert.notEqual(line, null, `host closed its output; stderr: ${this.stderr}`);
    return JSON.parse(line!);
  }

  async start(): Promise<void> {
    this.send({ t: "hello", protocol: 1, service: "x", start: "genesis", mode: "process" });
    const ready = await this.recv();
    assert.equal(ready.t, "ready");
    assert.ok(ready.environment["host.runtime"], "ready.environment has host.runtime");
  }

  step(seq: number, data = ""): void {
    this.send({ t: "step", seq: String(seq), source: "s", position: String(seq), data: b64(data) });
  }

  async finish(): Promise<void> {
    this.send({ t: "end" });
    assert.equal(await this.exited, 0);
  }
}

test("an abort the handler swallows still ends the step as aborted, with no further requests", async () => {
  const d = new Driver("swallow");
  await d.start();
  d.step(0);
  assert.deepEqual(await d.recv(), { t: "clock" });
  d.send({ t: "abort", detail: "nondeterminism" });
  assert.deepEqual(await d.recv(), { t: "done", outcome: "aborted" });
  // The next step is a fresh one.
  d.step(1);
  assert.deepEqual(await d.recv(), { t: "clock" });
  d.send({ t: "clock", unix_nanos: "5" });
  assert.deepEqual(await d.recv(), { t: "clock" });
  d.send({ t: "clock", unix_nanos: "6" });
  assert.deepEqual(await d.recv(), { t: "clock" });
  d.send({ t: "clock", unix_nanos: "7" });
  assert.deepEqual(await d.recv(), { t: "emit", sink: "after", data: b64("x"), scope: "remote" });
  assert.deepEqual(await d.recv(), { t: "done", outcome: "ok" });
  await d.finish();
});

test("Promise.all fan-out sends gateway requests in call order and handles errors", async () => {
  const d = new Driver("fanout");
  await d.start();
  d.step(0);
  for (const [i, answer] of [{ response: b64("one") }, { error: "timeout" }, { response: b64("three") }].entries()) {
    const req = await d.recv();
    assert.deepEqual([req.t, req.gateway, unb64(req.request), req.scope], ["gateway", "g", String(i + 1), "remote"]);
    d.send({ t: "gateway", ...answer });
  }
  const emit = await d.recv();
  assert.equal(unb64(emit.data), "one,!timeout,three");
  assert.deepEqual(await d.recv(), { t: "done", outcome: "ok" });
  await d.finish();
});

test("an abort during a fan-out leaves no unhandled rejection behind", async () => {
  const d = new Driver("floating");
  await d.start();
  d.step(0);
  assert.equal((await d.recv()).t, "gateway");
  d.send({ t: "abort", detail: "x" });
  assert.deepEqual(await d.recv(), { t: "done", outcome: "aborted" });
  await d.finish();
});

test("thrown values map to panic messages", async () => {
  const d = new Driver("throws");
  await d.start();
  for (const [what, message] of [
    ["string", "just a string"],
    ["type", "TypeError: null is not an object"],
    ["number", "7"],
  ] as const) {
    d.step(0, what);
    const done = await d.recv();
    assert.equal(done.t, "done");
    assert.equal(done.outcome, "panic");
    assert.equal(done.message, message);
  }
  d.step(1, "fine");
  assert.equal((await d.recv()).t, "emit");
  assert.deepEqual(await d.recv(), { t: "done", outcome: "ok" });
  await d.finish();
});

test("every way of printing stays off the protocol stream", async () => {
  const d = new Driver("prints");
  await d.start();
  d.step(0);
  assert.deepEqual(await d.recv(), { t: "emit", sink: "x", data: b64("x"), scope: "remote" });
  assert.deepEqual(await d.recv(), { t: "done", outcome: "ok" });
  await d.finish();
  assert.match(d.stderr, /log line/);
  assert.match(d.stderr, /info line/);
  assert.match(d.stderr, /raw write/);
});

test("a malformed message from the driver is a fatal", async () => {
  const d = new Driver("throws");
  d.proc.stdin!.write("this is not json\n");
  const m = await d.recv();
  assert.equal(m.t, "fatal");
  assert.equal(await d.exited, 1);
});

test("an unsupported protocol version is a fatal", async () => {
  const d = new Driver("throws");
  d.send({ t: "hello", protocol: 2, service: "x", start: "genesis", mode: "process" });
  assert.equal((await d.recv()).t, "fatal");
  assert.equal(await d.exited, 1);
});

test("a very large step message round-trips", async () => {
  const d = new Driver("throws");
  await d.start();
  d.step(0, "x".repeat(3_000_000));
  assert.equal((await d.recv()).t, "emit");
  assert.deepEqual(await d.recv(), { t: "done", outcome: "ok" });
  await d.finish();
});
