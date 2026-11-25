import type { Kavach } from "@kavachlabs/core";
import type { IORedisLike } from "@kavachlabs/ioredis";
import type { KafkaProducerLike } from "@kavachlabs/kafkajs";
import type { PgPoolLike } from "@kavachlabs/pg";

export interface RawBillingEvent {
  transaction_id: string;
  subscription_id: string;
  customer_email: string;
  code: string;
  quantity: number;
  properties?: Record<string, unknown>;
}

export interface BillingDeps {
  kv: Kavach;
  pool: PgPoolLike;
  redis: IORedisLike;
  producer: KafkaProducerLike;
}

interface SubscriptionRow {
  id: string;
  organization_id: string;
  plan_code: string;
  status: string;
}

interface ChargeRow {
  id: string;
  amount_cents: number;
  pay_in_advance: boolean;
}

/**
 * Processes one inbound Kafka usage event:
 *   1. Enriches against Postgres `subscriptions`
 *   2. Checks Postgres `charges` for pay-in-advance billing
 *   3. Flags subscription refresh in Redis via `ZADD subscription_refreshed_v2`
 *   4. Emits `events_enriched` and `events_charged_in_advance` to Kafka
 *
 * When `patched === false` (L1 bug), Kafka billing events are emitted BEFORE
 * the Redis refresh flag completes — so a transient Redis `LOADING` error
 * causes an uncommitted retry and a duplicate charge on redelivery.
 *
 * When `patched === true`, all Postgres and Redis checks/flags complete BEFORE
 * any Kafka emit, guaranteeing atomic single emission across retries.
 */
export async function processBillingEvent(
  deps: BillingDeps,
  event: RawBillingEvent,
  patched: boolean,
): Promise<void> {
  const now = deps.kv.now();

  const subRes = await deps.pool.query<SubscriptionRow>(
    "SELECT id, organization_id, plan_code, status FROM subscriptions WHERE id = $1",
    [event.subscription_id],
  );
  const sub = subRes.rows[0];
  if (!sub || sub.status !== "active") {
    return;
  }

  const enrichedPayload = {
    transaction_id: event.transaction_id,
    subscription_id: sub.id,
    organization_id: sub.organization_id,
    plan_code: sub.plan_code,
    customer_email: event.customer_email,
    code: event.code,
    quantity: event.quantity,
    processed_at: now.toISOString(),
  };

  if (!patched) {
    // BUG (L1): emits enriched event BEFORE downstream checks/Redis refresh complete!
    await deps.producer.send({
      topic: "events_enriched",
      messages: [
        {
          key: event.transaction_id,
          value: JSON.stringify(enrichedPayload),
        },
      ],
    });

    const chargeRes = await deps.pool.query<ChargeRow>(
      "SELECT id, amount_cents, pay_in_advance FROM charges WHERE subscription_id = $1 AND code = $2",
      [sub.id, event.code],
    );
    const charge = chargeRes.rows[0];

    if (charge && charge.pay_in_advance) {
      // BUG (L1): emits pay-in-advance charge BEFORE Redis refresh flag!
      await deps.producer.send({
        topic: "events_charged_in_advance",
        messages: [
          {
            key: event.transaction_id,
            value: JSON.stringify({
              transaction_id: event.transaction_id,
              subscription_id: sub.id,
              charge_id: charge.id,
              amount_cents: charge.amount_cents * event.quantity,
              customer_email: event.customer_email,
            }),
          },
        ],
      });
    }

    // Trailing Redis ZADD with wall-clock score + time-bucketed member:
    // if Redis returns LOADING here, the message is retried and charged twice!
    const score = Math.floor(now.getTime() / 1000);
    const member = `${sub.organization_id}:${sub.id}|${Math.floor(score / 60)}`;
    await deps.redis.zadd("subscription_refreshed_v2", score, member);
    return;
  }

  // PATCHED: Perform ALL Postgres and Redis checks/flags BEFORE emitting any Kafka output.
  const chargeRes = await deps.pool.query<ChargeRow>(
    "SELECT id, amount_cents, pay_in_advance FROM charges WHERE subscription_id = $1 AND code = $2",
    [sub.id, event.code],
  );
  const charge = chargeRes.rows[0];

  const score = Math.floor(now.getTime() / 1000);
  const member = `${sub.organization_id}:${sub.id}|${Math.floor(score / 60)}`;
  await deps.redis.zadd("subscription_refreshed_v2", score, member);

  await deps.producer.send({
    topic: "events_enriched",
    messages: [
      {
        key: event.transaction_id,
        value: JSON.stringify(enrichedPayload),
      },
    ],
  });

  if (charge && charge.pay_in_advance) {
    await deps.producer.send({
      topic: "events_charged_in_advance",
      messages: [
        {
          key: event.transaction_id,
          value: JSON.stringify({
            transaction_id: event.transaction_id,
            subscription_id: sub.id,
            charge_id: charge.id,
            amount_cents: charge.amount_cents * event.quantity,
            customer_email: event.customer_email,
          }),
        },
      ],
    });
  }
}
