import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import {
  CODE_PG_40001,
  FrameDecoder,
  RecordType,
  Ring,
  StepTable,
  decodeRecord,
  init,
} from "@kavachlabs/core";
import { type PgDatabaseError, type PgPoolLike, createReplayPool, wrapPool } from "@kavachlabs/pg";

test("@kavachlabs/pg records SELECT as Gateway (pg:select) and INSERT/UPDATE as Output (pg:exec), and replays 100% offline", async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "kavach-ts-pg-"));
  const ringPath = path.join(dir, "pg.shm");
  try {
    const kvRec = init({ service: "pg-svc", ringPath, mode: "record" });
    const fakeRawPool: PgPoolLike = {
      async query(textOrCfg, values): Promise<any> {
        const sql = typeof textOrCfg === "string" ? textOrCfg : textOrCfg.text;
        const vals = typeof textOrCfg === "string" ? (values ?? []) : (textOrCfg.values ?? []);
        if (sql.startsWith("SELECT")) {
          return {
            command: "SELECT",
            rowCount: 1,
            oid: null,
            rows: [{ id: vals[0], pay_in_advance: true, amount_cents: 500, created_at: new Date("2026-01-01T00:00:00.000Z") }],
            fields: [{ name: "id" }, { name: "pay_in_advance" }, { name: "amount_cents" }, { name: "created_at" }],
          };
        }
        return { command: "INSERT", rowCount: 1, oid: null, rows: [], fields: [] };
      },
      async connect() {
        return this;
      },
      async end() {},
    };

    const pool = wrapPool(kvRec, fakeRawPool);
    await kvRec.runStep({ source: "test", data: Buffer.from("{}") }, async () => {
      const sel = await pool.query<{ id: string; pay_in_advance: boolean; amount_cents: number; created_at: Date }>(
        "SELECT id, pay_in_advance, amount_cents, created_at FROM subscriptions WHERE id = $1",
        ["sub_1"],
      );
      assert.equal(sel.rows[0].pay_in_advance, true);
      await pool.query("INSERT INTO audit_log (sub_id) VALUES ($1)", ["sub_1"]);
    });
    await kvRec.close();

    // Decode recorded Gateway and Output records from ring
    const reader = Ring.open(ringPath);
    const bytes = reader.pollOnce();
    assert.ok(bytes);
    const dec = new FrameDecoder();
    dec.feed(bytes);
    const drained = dec.finishEOF();
    const gwRec = decodeRecord(drained.frames[2].payload);
    const outRec = decodeRecord(drained.frames[3].payload);
    assert.equal(gwRec.type, RecordType.Gateway);
    assert.equal(gwRec.gateway, "pg:select");
    assert.equal(outRec.type, RecordType.Output);
    assert.equal(outRec.sink, "pg:exec");
    reader.close();

    // Replay completely offline with createReplayPool(kv) (zero socket!)
    const kvRep = init({ service: "pg-svc", mode: "replay" });
    const replayPool = createReplayPool(kvRep);
    const table = new StepTable(1n, [
      {
        driver: "pg",
        gateway: gwRec.gateway,
        request: gwRec.request,
        response: gwRec.response,
      },
    ]);

    const { error } = await kvRep.runStepWithTable(
      { source: "test", data: Buffer.from("{}") },
      table,
      async () => {
        // Test object row mode
        const res = await replayPool.query<{ id: string; pay_in_advance: boolean; amount_cents: number; created_at: Date }>({
          text: "SELECT id, pay_in_advance, amount_cents, created_at FROM subscriptions WHERE id = $1",
          values: ["sub_1"],
        });
        assert.equal(res.rows.length, 1);
        assert.equal(res.rows[0].id, "sub_1");
        assert.equal(res.rows[0].pay_in_advance, true);
        assert.equal(res.rows[0].amount_cents, 500);
        assert.ok(res.rows[0].created_at instanceof Date);

        // Test Drizzle/Prisma connect() + tx + INSERT Output
        const client = await replayPool.connect();
        await client.query("BEGIN");
        await client.query("INSERT INTO audit_log (sub_id) VALUES ($1)", ["sub_1"]);
        await client.query("COMMIT");
        client.release?.();
      },
    );
    assert.equal(error, undefined);
    assert.equal(table.emittedOutputs().length, 1);
    assert.equal(table.emittedOutputs()[0].sink, "pg:exec");
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("@kavachlabs/pg reconstructs Postgres PG_40001 error with err.code = '40001'", async () => {
  const kv = init({ service: "pg-fault", mode: "replay" });
  const pool = createReplayPool(kv);
  const table = new StepTable(1n, [
    { driver: "pg", op: "SELECT 1", keyArgs: [], response: Buffer.from('{"columns":[],"rows":[]}') },
  ]);
  table.injectFault({ driver: "pg", errString: CODE_PG_40001 });

  const { error } = await kv.runStepWithTable({ source: "test", data: Buffer.from("{}") }, table, async () => {
    await pool.query("SELECT 1");
  });
  assert.ok(error);
  const pgErr = error as PgDatabaseError;
  assert.equal(pgErr.code, "40001");
  assert.equal(pgErr.severity, "ERROR");
});
