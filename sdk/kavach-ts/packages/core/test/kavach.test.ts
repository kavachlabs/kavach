import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import {
  FrameDecoder,
  RecordType,
  ReplayClockProvider,
  Ring,
  StepTable,
  decodeRecord,
  init,
} from "@kavachlabs/core";

test("Kavach AsyncLocalStorage step propagation across async/await & Promise.all + PII-scrubbed ring capture", async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "kavach-ts-core-"));
  const ringPath = path.join(dir, "app.shm");
  try {
    const kv = init({
      service: "als-service",
      ringPath,
      pii: "standard",
      mode: "record",
    });
    assert.equal(kv.mode(), "record");

    await kv.runStep(
      {
        source: "http:POST /bill",
        data: Buffer.from('{"transaction_id":"tx_1","email":"secret@corp.example"}'),
      },
      async () => {
        const t = kv.now();
        assert.ok(t instanceof Date);
        const id = kv.rand().uuidv4();
        assert.match(id, /^[0-9a-f-]{36}$/);

        const [a, b] = await Promise.all([
          (async () => {
            await new Promise((r) => setTimeout(r, 5));
            return kv.doGateway(
              { driver: "pg", op: "SELECT 1", keyArgs: ["k1"] },
              async () => Buffer.from('{"v":1}'),
            );
          })(),
          (async () => {
            await new Promise((r) => setTimeout(r, 2));
            return kv.doGateway(
              { driver: "redis", op: "GET", keyArgs: ["k2"] },
              async () => Buffer.from('{"v":2}'),
            );
          })(),
        ]);
        assert.equal(Buffer.from(a).toString("utf8"), '{"v":1}');
        assert.equal(Buffer.from(b).toString("utf8"), '{"v":2}');

        kv.recordOutput({
          sink: "kafka:charged",
          data: Buffer.from('{"transaction_id":"tx_1","email":"secret@corp.example"}'),
        });
      },
    );
    await kv.close();

    // Verify ring decoded cleanly and PII was scrubbed
    const reader = Ring.open(ringPath);
    const bytes = reader.pollOnce();
    assert.ok(bytes);
    const dec = new FrameDecoder();
    dec.feed(bytes);
    const res = dec.finishEOF();
    assert.equal(res.cleanClose, true);
    assert.equal(res.discardedTornBytes, 0);

    const inputRec = decodeRecord(res.frames[1].payload);
    assert.equal(inputRec.type, RecordType.Input);
    const inputText = Buffer.from(inputRec.data!).toString("utf8");
    assert.equal(inputText.includes("secret@corp.example"), false);
    assert.match(inputText, /u_[0-9a-f]{16}@redacted\.invalid/);
    reader.close();
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("Kavach mode auto-detection honors KAVACH_HOST=1 and trailing kavach-host argv", () => {
  const k1 = init({ argv: ["node", "app.js", "kavach-host"], env: {} });
  assert.equal(k1.mode(), "replay");

  const k2 = init({ argv: ["node", "app.js"], env: { KAVACH_HOST: "1" } });
  assert.equal(k2.mode(), "replay");

  const k3 = init({ argv: ["node", "app.js"], env: {} });
  assert.equal(k3.mode(), "off");
});

test("Kavach runStepWithTable replays deterministic clock, rand, and reordered gateways", async () => {
  const kv = init({ service: "replay-svc", mode: "replay" });
  const table = new StepTable(1n, [
    { driver: "pg", op: "SELECT 1", keyArgs: ["a"], response: Buffer.from("pg-ok") },
  ]);
  const clockProv = new ReplayClockProvider({
    stepSeq: 1n,
    clocks: [1_700_000_000_000_000_000n],
  });
  const { result, error } = await kv.runStepWithTable(
    { source: "test", data: Buffer.from("{}") },
    table,
    async () => {
      const now = kv.now();
      const uuid = kv.rand().uuidv4();
      const gw = await kv.doGateway({ driver: "pg", op: "SELECT 1", keyArgs: ["a"] }, async () => {
        throw new Error("must never call liveFn in replay");
      });
      return { iso: now.toISOString(), uuid, gw: Buffer.from(gw).toString("utf8") };
    },
    clockProv,
  );
  assert.equal(error, undefined);
  assert.equal(result?.iso, "2023-11-14T22:13:20.000Z");
  assert.equal(result?.gw, "pg-ok");
  assert.match(result!.uuid, /^[0-9a-f-]{36}$/);
});
