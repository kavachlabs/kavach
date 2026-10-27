import json
import sqlite3
import time

from flask import Flask, request, abort

app = Flask(__name__)
db = sqlite3.connect("subs.db", check_same_thread=False)
db.execute("create table if not exists subs (customer text primary key, plan text, status text, renews_at integer)")


@app.post("/webhooks/stripe")
def stripe_webhook():
    event = json.loads(request.data)
    kind = event["type"]
    obj = event["data"]["object"]
    if kind == "customer.subscription.created":
        db.execute("insert into subs values (?, ?, 'active', ?)",
                   (obj["customer"], obj["plan"]["id"], obj["current_period_end"]))
    elif kind == "customer.subscription.updated":
        db.execute("update subs set plan = ?, renews_at = ? where customer = ?",
                   (obj["plan"]["id"], obj["current_period_end"], obj["customer"]))
    elif kind == "invoice.payment_failed":
        db.execute("update subs set status = 'past_due' where customer = ?", (obj["customer"],))
    elif kind == "customer.subscription.deleted":
        if obj["current_period_end"] < time.time():
            db.execute("update subs set status = 'canceled' where customer = ?", (obj["customer"],))
    else:
        abort(400)
    db.commit()
    return "", 204
