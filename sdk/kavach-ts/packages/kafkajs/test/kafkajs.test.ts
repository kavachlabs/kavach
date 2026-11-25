import assert from "node:assert/strict";
import test from "node:test";
import { StepTable, init } from "@kavachlabs/core";
import { ErrRecordNotCommitted, createReplayKafka } from "@kavachlabs/kafkajs";

test("@kavachlabs/kafkajs captures kafka:<topic> Input/Output (raw JSON payload) and replays offline with zero broker", async () => {
  const kv = init({ service: "kafka-svc", mode: "replay" });
  const kafka = createReplayKafka(kv);
  const producer = kafka.producer();
  const consumer = kafka.consumer({ groupId: "billing" });

  await producer.connect();
  await consumer.connect();
  await consumer.subscribe({ topic: "events_raw" });
  await consumer.run({
    eachMessage: async ({ message }) => {
      const ev = JSON.parse(message.value!.toString("utf8")) as { transaction_id: string };
      await producer.send({
        topic: "events_charged_in_advance",
        messages: [{ key: ev.transaction_id, value: JSON.stringify({ transaction_id: ev.transaction_id, ok: true }) }],
      });
    },
  });

  const table = new StepTable(1n);
  const { error } = await kv.runStepWithTable(
    {
      source: "kafka:events_raw",
      position: "0:7",
      data: Buffer.from('{"transaction_id":"tx_42"}'),
    },
    table,
    (sc) => consumer.runStep(sc.input()),
  );
  assert.equal(error, undefined);

  const outs = table.emittedOutputs();
  assert.equal(outs.length, 1);
  assert.equal(outs[0].sink, "kafka:events_charged_in_advance");
  // Raw JSON payload preserved so $.transaction_id matches directly!
  assert.deepEqual(JSON.parse(Buffer.from(outs[0].data).toString("utf8")), {
    transaction_id: "tx_42",
    ok: true,
  });
});

test("@kavachlabs/kafkajs signals retryable error on thrown eachMessage and uncommitted eachBatch", async () => {
  const kv = init({ service: "kafka-retry", mode: "replay" });
  const kafka = createReplayKafka(kv);

  const c1 = kafka.consumer();
  await c1.run({
    eachMessage: async () => {
      throw new Error("LOADING Redis is loading the dataset in memory");
    },
  });
  const r1 = await kv.runStepWithTable(
    { source: "kafka:events_raw", position: "0:1", data: Buffer.from("{}") },
    new StepTable(1n),
    (sc) => c1.runStep(sc.input()),
  );
  assert.match(r1.error?.message ?? "", /LOADING/);

  const c2 = kafka.consumer();
  await c2.run({
    autoCommit: false,
    eachBatch: async () => {
      // Deliberately do NOT call resolveOffset -> uncommitted!
    },
  });
  const r2 = await kv.runStepWithTable(
    { source: "kafka:events_raw", position: "0:2", data: Buffer.from("{}") },
    new StepTable(2n),
    (sc) => c2.runStep(sc.input()),
  );
  assert.equal(r2.error, ErrRecordNotCommitted);
});
