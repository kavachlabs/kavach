import { init } from "@kavach/core";
import { type IORedisLike, wrapRedis } from "@kavach/ioredis";
import { createReplayKafka } from "@kavach/kafkajs";
import { type PgPoolLike, wrapPool } from "@kavach/pg";
import { type RawBillingEvent, processBillingEvent } from "./processor.ts";

// Records realistic baseline traffic on the unpatched service through KVRING02
// (honoring KAVACH_RING_PATH & KAVACH_PII set by `kavach dev`).
const kv = init({
  service: "l1-billing-ts",
  pii: "standard",
});

// Seed data backing a lightweight local driver stub during `kavach dev` recording
// (in production this is your real `new pg.Pool(...)` and `new Redis(...)`).
const subscriptions: Record<string, { id: string; organization_id: string; plan_code: string; status: string }> = {
  sub_acme_01: { id: "sub_acme_01", organization_id: "org_acme", plan_code: "enterprise_usage", status: "active" },
  sub_beta_02: { id: "sub_beta_02", organization_id: "org_beta", plan_code: "growth_prepaid", status: "active" },
  sub_gamma_03: { id: "sub_gamma_03", organization_id: "org_gamma", plan_code: "scale_prepaid", status: "active" },
};

const charges: Record<string, { id: string; amount_cents: number; pay_in_advance: boolean }> = {
  "sub_acme_01:api_calls": { id: "chg_01", amount_cents: 500, pay_in_advance: true },
  "sub_beta_02:gpu_hours": { id: "chg_02", amount_cents: 1200, pay_in_advance: true },
  "sub_gamma_03:storage_gb": { id: "chg_03", amount_cents: 250, pay_in_advance: true },
};

const localPg: PgPoolLike = {
  async query(textOrCfg, values): Promise<any> {
    const sql = typeof textOrCfg === "string" ? textOrCfg : textOrCfg.text;
    const vals = typeof textOrCfg === "string" ? (values ?? []) : (textOrCfg.values ?? []);
    if (sql.includes("FROM subscriptions")) {
      const row = subscriptions[String(vals[0])];
      return {
        command: "SELECT",
        rowCount: row ? 1 : 0,
        oid: null,
        rows: row ? [row] : [],
        fields: [{ name: "id" }, { name: "organization_id" }, { name: "plan_code" }, { name: "status" }],
      };
    }
    if (sql.includes("FROM charges")) {
      const row = charges[`${String(vals[0])}:${String(vals[1])}`];
      return {
        command: "SELECT",
        rowCount: row ? 1 : 0,
        oid: null,
        rows: row ? [row] : [],
        fields: [{ name: "id" }, { name: "amount_cents" }, { name: "pay_in_advance" }],
      };
    }
    return { command: "SELECT", rowCount: 0, oid: null, rows: [], fields: [] };
  },
  async connect() {
    return this;
  },
  async end() {},
};

const localRedis: IORedisLike = {
  async ping() {
    return "PONG";
  },
  async get() {
    return null;
  },
  async set() {
    return "OK";
  },
  async del() {
    return 1;
  },
  async exists() {
    return 0;
  },
  async zadd() {
    return 1;
  },
};

const pool = wrapPool(kv, localPg);
const redis = wrapRedis(kv, localRedis);
const kafka = createReplayKafka(kv, { offline: true });
const producer = kafka.producer();
const consumer = kafka.consumer({ groupId: "l1-billing-group" });

await consumer.subscribe({ topic: "events_raw" });
await consumer.run({
  eachMessage: async ({ message }) => {
    if (!message.value) return;
    const event = JSON.parse(message.value.toString("utf8")) as RawBillingEvent;
    await processBillingEvent({ kv, pool, redis, producer }, event, false);
  },
});

const sampleEvents: RawBillingEvent[] = [
  {
    transaction_id: "tx_l1_1001",
    subscription_id: "sub_acme_01",
    customer_email: "alice.chen@vertex-ai.example",
    code: "api_calls",
    quantity: 3,
    properties: { region: "us-east-1", tier: "gold" },
  },
  {
    transaction_id: "tx_l1_1002",
    subscription_id: "sub_beta_02",
    customer_email: "bob.patel@vertex-ai.example",
    code: "gpu_hours",
    quantity: 2,
    properties: { region: "eu-west-1", tier: "pro" },
  },
  {
    transaction_id: "tx_l1_1003",
    subscription_id: "sub_gamma_03",
    customer_email: "carol.wu@vertex-ai.example",
    code: "storage_gb",
    quantity: 4,
    properties: { region: "ap-south-1", tier: "scale" },
  },
];

for (let i = 0; i < sampleEvents.length; i++) {
  const ev = sampleEvents[i];
  await consumer.handleMessage("events_raw", 0, {
    key: ev.transaction_id,
    offset: String(i + 1),
    value: JSON.stringify(ev),
  });
}

await kv.close();
