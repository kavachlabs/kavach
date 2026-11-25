import assert from "node:assert/strict";
import readline from "node:readline";
import { PassThrough } from "node:stream";
import test from "node:test";
import { init } from "@kavach/core";

test("SPEC §9 kavach-host stdio protocol loop supports hello/ready/step/gateway/emit/reset/end with async handlers", async () => {
  const driverToHost = new PassThrough();
  const hostToDriver = new PassThrough();

  const kv = init({
    service: "host-test",
    mode: "replay",
    stdin: driverToHost,
    stdout: hostToDriver,
  });
  kv.addInvariantRule({
    name: "single_emit",
    kind: "at_most_once_emit",
    sink: "kafka:charged",
    key: "$.tx",
    on_error_emit_count: 0,
  });

  let resets = 0;
  const hostDone = kv.serveHost(async (input) => {
    const body = JSON.parse(Buffer.from(input.data).toString("utf8")) as { tx: string };
    const gwResp = await kv.replayGateway({
      driver: "pg",
      gateway: "pg:select",
      op: "SELECT 1",
      keyArgs: [body.tx],
    });
    const ok = JSON.parse(Buffer.from(gwResp).toString("utf8")) as { ok: boolean };
    if (ok.ok) {
      kv.recordOutput({
        sink: "kafka:charged",
        data: Buffer.from(JSON.stringify({ tx: body.tx })),
      });
    }
  });

  const rl = readline.createInterface({ input: hostToDriver });
  const lines: string[] = [];
  const waiters: Array<(line: string) => void> = [];
  rl.on("line", (line) => {
    const w = waiters.shift();
    if (w) w(line);
    else lines.push(line);
  });
  const nextMsg = async (): Promise<Record<string, unknown>> => {
    const line =
      lines.length > 0
        ? lines.shift()!
        : await new Promise<string>((resolve) => waiters.push(resolve));
    return JSON.parse(line) as Record<string, unknown>;
  };
  const send = (msg: Record<string, unknown>) => {
    driverToHost.write(JSON.stringify(msg) + "\n");
  };

  // 1. hello -> ready
  send({ t: "hello", protocol: 1, service: "host-test", start: "genesis", mode: "process" });
  const ready1 = await nextMsg();
  assert.equal(ready1.t, "ready");
  assert.equal(ready1.protocol, 1);
  assert.deepEqual(ready1.invariants, ["single_emit"]);
  assert.ok((ready1.environment as Record<string, unknown>)?.["host.runtime"]);

  // 2. step -> gateway -> emit -> done(ok)
  send({
    t: "step",
    seq: "1",
    source: "kafka:raw",
    position: "0:0",
    data: Buffer.from('{"tx":"t1"}').toString("base64"),
  });
  const gwReq = await nextMsg();
  assert.equal(gwReq.t, "gateway");
  assert.equal(gwReq.gateway, "pg:select");
  send({ t: "gateway", response: Buffer.from('{"ok":true}').toString("base64") });

  const emitMsg = await nextMsg();
  assert.equal(emitMsg.t, "emit");
  assert.equal(emitMsg.sink, "kafka:charged");
  assert.equal(Buffer.from(String(emitMsg.data), "base64").toString("utf8"), '{"tx":"t1"}');

  const done1 = await nextMsg();
  assert.equal(done1.t, "done");
  assert.equal(done1.outcome, "ok");

  // 3. reset -> ready (clears seen keys so tx t1 passes again in new run!)
  send({ t: "reset", protocol: 1, service: "host-test", start: "genesis", mode: "process" });
  const ready2 = await nextMsg();
  assert.equal(ready2.t, "ready");
  resets++;
  assert.equal(resets, 1);

  // 4. step with gateway error -> done(error)
  send({
    t: "step",
    seq: "1",
    source: "kafka:raw",
    position: "0:0",
    data: Buffer.from('{"tx":"t1"}').toString("base64"),
  });
  const gwReq2 = await nextMsg();
  assert.equal(gwReq2.t, "gateway");
  send({ t: "gateway", error: "LOADING Redis is loading the dataset in memory" });

  const done2 = await nextMsg();
  assert.equal(done2.t, "done");
  assert.equal(done2.outcome, "error");
  assert.match(String(done2.message), /LOADING/);

  // 5. end
  send({ t: "end" });
  await hostDone;
  rl.close();
});
