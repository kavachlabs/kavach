import assert from "node:assert/strict";
import test from "node:test";
import {
  CODE_PG_40001,
  CODE_REDIS_LOADING,
  FAULT_REDIS_LOADING,
  FaultError,
  StepTable,
  UnmatchedGatewayError,
  canonicalizeGatewayCall,
  isFault,
  normalizeSQL,
} from "@kavach/core";

test("normalizeSQL collapses whitespace, comments, literals, and positional placeholders", () => {
  const sql = `
    SELECT id, amount -- comment
    FROM charges /* block */
    WHERE subscription_id = $1 AND status = 'active' AND amount > 100;
  `;
  assert.equal(
    normalizeSQL(sql),
    "SELECT id, amount FROM charges WHERE subscription_id = ? AND status = ? AND amount > ?",
  );
});

test("StepTable two-tier matching: StrictSeq, KeyedTemplate fallback, separate Output stream, and UnmatchedGatewayError", () => {
  const gw1 = canonicalizeGatewayCall({
    driver: "pg",
    op: "SELECT id FROM subscriptions WHERE id = $1",
    keyArgs: ["sub_1"],
    response: Buffer.from('{"id":"sub_1"}'),
  });
  const gw2 = canonicalizeGatewayCall({
    driver: "redis",
    op: "ZADD",
    keyArgs: ["subscription_refreshed_v2"],
    response: Buffer.from('{"kind":"int","val":1}'),
  });

  const table = new StepTable(1n, [gw1, gw2], [
    { sink: "kafka:events_enriched", data: Buffer.from('{"transaction_id":"tx_1"}') },
  ]);

  // Reordered: call Redis FIRST, then PG SECOND (triggers KeyedTemplate fallback!)
  const r1 = table.matchGateway({
    driver: "redis",
    op: "ZADD",
    keyArgs: ["subscription_refreshed_v2"],
  });
  assert.equal(r1.tier, "keyed_template");
  assert.equal(r1.error, undefined);
  assert.equal(table.reorderedCount(), 1);
  assert.equal(table.telemetry()[0]?.kind, "reordered_gateway");

  // PG matches via StrictSeq now that slot 0 is still next unconsumed
  const r2 = table.matchGateway({
    driver: "pg",
    op: "SELECT   id  FROM subscriptions WHERE id = $1",
    keyArgs: ["sub_1"],
  });
  assert.equal(r2.tier, "strict_seq");
  assert.equal(Buffer.from(r2.response!).toString("utf8"), '{"id":"sub_1"}');

  // Emitting output after gateways matches recorded output stream cleanly
  table.recordOutput({ sink: "kafka:events_enriched", data: Buffer.from('{"transaction_id":"tx_1"}') });
  assert.equal(table.compareOutputs(), null);

  // Brand-new unexpected read hard-fails with UnmatchedGatewayError
  const r3 = table.matchGateway({ driver: "pg", op: "SELECT * FROM secrets", keyArgs: [] });
  assert.equal(r3.tier, "unmatched_gateway");
  assert.ok(r3.error instanceof UnmatchedGatewayError);
});

test("StepTable fault injection (REDIS_LOADING, PG_40001) with once flag", () => {
  const gw = canonicalizeGatewayCall({
    driver: "redis",
    op: "ZADD",
    keyArgs: ["refresh_key"],
    response: Buffer.from("1"),
  });
  const table = new StepTable(1n, [gw, gw]);
  table.injectFault({ driver: "redis", errString: CODE_REDIS_LOADING, once: true });

  const fail = table.matchGateway({ driver: "redis", op: "ZADD", keyArgs: ["refresh_key"] });
  assert.equal(fail.tier, "fault_injected");
  assert.ok(fail.error instanceof FaultError);
  assert.equal(fail.error.message, FAULT_REDIS_LOADING);
  assert.equal(isFault(fail.error, CODE_REDIS_LOADING), true);
  assert.equal(isFault(fail.error, CODE_PG_40001), false);

  // Second call succeeds because once: true
  const ok = table.matchGateway({ driver: "redis", op: "ZADD", keyArgs: ["refresh_key"] });
  assert.equal(ok.tier, "strict_seq");
  assert.equal(ok.error, undefined);
});
