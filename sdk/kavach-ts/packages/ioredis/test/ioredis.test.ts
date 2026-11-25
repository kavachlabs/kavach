import assert from "node:assert/strict";
import test from "node:test";
import { CODE_REDIS_LOADING, StepTable, init } from "@kavachlabs/core";
import { createReplayRedis, extractRedisKeyArgs, wrapRedis } from "@kavachlabs/ioredis";

test("@kavachlabs/ioredis extracts timestamp-independent (cmd, key) args and supports startup ping offline", async () => {
  assert.deepEqual(extractRedisKeyArgs("zadd", ["subscription_refreshed_v2", 1700000000, "org:sub|bucket1"]), [
    "subscription_refreshed_v2",
  ]);
  assert.deepEqual(extractRedisKeyArgs("hget", ["hash:1", "field_a"]), ["hash:1", "field_a"]);
  assert.deepEqual(extractRedisKeyArgs("mget", ["k1", "k2"]), ["k1", "k2"]);

  const kv = init({ service: "redis-svc", mode: "replay" });
  const rdb = createReplayRedis(kv);

  // Startup healthcheck outside any active step -> 'PONG' offline!
  assert.equal(await rdb.ping(), "PONG");
  assert.equal(await rdb.quit?.(), "OK");
});

test("@kavachlabs/ioredis replays ZADD regardless of score/timestamp and injects LOADING fault", async () => {
  const kv = init({ service: "redis-svc", mode: "replay" });
  const rdb = createReplayRedis(kv);

  const table = new StepTable(1n, [
    {
      driver: "redis",
      gateway: "redis:zadd",
      op: "ZADD",
      normalizedOp: "ZADD",
      keyArgs: ["subscription_refreshed_v2"],
      response: Buffer.from('{"kind":"int","val":1}'),
    },
    {
      driver: "redis",
      gateway: "redis:get",
      op: "GET",
      normalizedOp: "GET",
      keyArgs: ["feature:flag"],
      response: Buffer.from('{"kind":"string","val":"enabled"}'),
    },
  ]);

  const { result, error } = await kv.runStepWithTable(
    { source: "test", data: Buffer.from("{}") },
    table,
    async () => {
      // Different score & member than recording still matches because keyArgs = ["subscription_refreshed_v2"]!
      const added = await rdb.zadd("subscription_refreshed_v2", Date.now() + 9999, "different-member");
      const pipe = rdb.pipeline!();
      pipe.get("feature:flag");
      const pipeRes = await pipe.exec();
      return { added, flag: pipeRes[0]?.[1] };
    },
  );
  assert.equal(error, undefined);
  assert.deepEqual(result, { added: 1, flag: "enabled" });

  // Inject LOADING fault
  const faultTable = new StepTable(2n, [
    {
      driver: "redis",
      gateway: "redis:zadd",
      op: "ZADD",
      keyArgs: ["subscription_refreshed_v2"],
      response: Buffer.from('{"kind":"int","val":1}'),
    },
  ]);
  faultTable.injectFault({ driver: "redis", errString: CODE_REDIS_LOADING });

  const { error: faultErr } = await kv.runStepWithTable(
    { source: "test", data: Buffer.from("{}") },
    faultTable,
    async () => {
      await rdb.zadd("subscription_refreshed_v2", 123, "m");
    },
  );
  assert.ok(faultErr);
  assert.equal(faultErr.name, "ReplyError");
  assert.equal(faultErr.message, "LOADING Redis is loading the dataset in memory");
});

test("@kavachlabs/ioredis wrapRedis records live commands transparently", async () => {
  const kv = init({ service: "redis-rec", mode: "off" });
  const fake = {
    async ping() {
      return "PONG";
    },
    async get(k: string) {
      return `val:${k}`;
    },
    async set() {
      return "OK";
    },
    async del() {
      return 1;
    },
    async exists() {
      return 1;
    },
    async zadd() {
      return 1;
    },
  };
  const wrapped = wrapRedis(kv, fake);
  assert.equal(await wrapped.get("abc"), "val:abc");
});
