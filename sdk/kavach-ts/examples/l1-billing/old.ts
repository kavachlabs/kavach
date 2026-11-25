import { init } from "@kavachlabs/core";
import { createReplayRedis } from "@kavachlabs/ioredis";
import { createReplayKafka } from "@kavachlabs/kafkajs";
import { createReplayPool } from "@kavachlabs/pg";
import { type RawBillingEvent, processBillingEvent } from "./processor.ts";

const kv = init({
  service: "l1-billing-ts",
  pii: "standard",
});

// Zero memrepo, zero code invariants — only thin 1-line Kavach driver wrappers.
const pool = createReplayPool(kv);
const redis = createReplayRedis(kv);
const kafka = createReplayKafka(kv);
const producer = kafka.producer();
const consumer = kafka.consumer({ groupId: "l1-billing-group" });

// Startup healthchecks succeed offline in Replay mode (zero TCP sockets).
const pong = await redis.ping();
if (pong !== "PONG") {
  throw new Error(`unexpected redis ping: ${pong}`);
}
await producer.connect();
await consumer.connect();
await consumer.subscribe({ topic: "events_raw" });

await consumer.run({
  eachMessage: async ({ message }) => {
    if (!message.value) return;
    const event = JSON.parse(message.value.toString("utf8")) as RawBillingEvent;
    await processBillingEvent({ kv, pool, redis, producer }, event, false);
  },
});

await kv.serveOrReplay(consumer);
await kv.close();
