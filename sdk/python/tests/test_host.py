import base64
import io
import json
import unittest

from kavach import Gateway, GatewayError, HandlerError, Panic
from kavach.conformance import AnyGateway, Conformance
from kavach.host import Host


def b64(b: bytes) -> str:
    return base64.b64encode(b).decode()


def ops(*o) -> str:
    return b64(json.dumps(list(o)).encode())


HELLO = {"t": "hello", "protocol": 1, "service": "svc", "start": "genesis", "mode": "process"}


def step(data: str, seq="0") -> dict:
    return {"t": "step", "seq": seq, "source": "test", "position": seq, "data": data}


class Out(io.BytesIO):
    def lines(self):
        return [json.loads(l) for l in self.getvalue().splitlines()]


def play(factory, msgs, raw=None, **kw):
    """Run a host over a scripted driver; returns (exit status, host messages)."""
    data = raw if raw is not None else b"".join(json.dumps(m).encode() + b"\n" for m in msgs)
    out = Out()
    kw.setdefault("environment", lambda: {})
    code = Host(factory, io.BytesIO(data), out, **kw).run()
    return code, out.lines()


class HostLoop(unittest.TestCase):
    def test_clean_session(self):
        code, out = play(Conformance, [HELLO, step(ops({"op": "emit", "sink": "s", "data": "d"})), {"t": "end"}])
        self.assertEqual(code, 0)
        self.assertEqual([m["t"] for m in out], ["ready", "emit", "done"])
        self.assertEqual(out[0]["invariants"], ["below_limit"])
        self.assertEqual(out[2], {"t": "done", "outcome": "ok"})

    def test_eof_between_steps_is_a_clean_exit(self):
        code, out = play(Conformance, [HELLO])
        self.assertEqual(code, 0)

    def test_malformed_json_is_fatal(self):
        code, out = play(Conformance, [], raw=b"this is not json\n")
        self.assertEqual(code, 1)
        self.assertEqual(out[0]["t"], "fatal")

    def test_message_without_type_is_fatal(self):
        code, out = play(Conformance, [], raw=b'{"x":1}\n')
        self.assertEqual((code, out[0]["t"]), (1, "fatal"))

    def test_step_before_hello_is_fatal(self):
        code, out = play(Conformance, [step(ops())])
        self.assertEqual((code, out[0]["t"]), (1, "fatal"))

    def test_unknown_message_is_fatal(self):
        code, out = play(Conformance, [HELLO, {"t": "wat"}])
        self.assertEqual((code, out[-1]["t"]), (1, "fatal"))

    def test_wrong_protocol_is_fatal(self):
        code, out = play(Conformance, [{**HELLO, "protocol": 2}])
        self.assertEqual((code, out[0]["t"]), (1, "fatal"))

    def test_factory_failure_is_fatal(self):
        def bad():
            raise RuntimeError("no config")

        code, out = play(bad, [HELLO])
        self.assertEqual((code, out[0]["t"]), (1, "fatal"))
        self.assertIn("no config", out[0]["message"])

    def test_bad_snapshot_is_fatal(self):
        code, out = play(Conformance, [{**HELLO, "start": "snapshot", "snapshot": b64(b"xyz")}])
        self.assertEqual((code, out[0]["t"]), (1, "fatal"))

    def test_snapshot_without_restore_is_fatal(self):
        class H:
            def handle(self, env, input):
                pass

        code, out = play(H, [{**HELLO, "start": "snapshot", "snapshot": ""}])
        self.assertEqual((code, out[0]["t"]), (1, "fatal"))

    def test_wrong_answer_type_is_fatal(self):
        code, out = play(Conformance, [HELLO, step(ops({"op": "clock"})), {"t": "rand", "data": ""}])
        self.assertEqual((code, out[-1]["t"]), (1, "fatal"))

    def test_driver_gone_mid_step(self):
        code, out = play(Conformance, [HELLO, step(ops({"op": "clock"}))])
        self.assertEqual(code, 1)
        self.assertEqual(out[-1], {"t": "clock"})

    def test_exceptions_map_like_the_recorder(self):
        class H:
            def handle(self, env, input):
                {}["amount"]

        code, out = play(H, [HELLO, step(ops())])
        done = out[-1]
        self.assertEqual((done["outcome"], done["message"]), ("panic", "KeyError: 'amount'"))
        self.assertIn("Traceback", done["detail"])

    def test_u64_values_are_strings_in_requests_and_answers(self):
        code, out = play(Conformance, [
            HELLO, step(ops({"op": "clock"})), {"t": "clock", "unix_nanos": "1759752000000000001"}, {"t": "end"},
        ])
        self.assertEqual(base64.b64decode(out[2]["data"]), b'{"clock":"1759752000000000001"}')


class Aborts(unittest.TestCase):
    def test_abort_unwinds_and_makes_no_further_requests(self):
        code, out = play(Conformance, [
            HELLO, step(ops({"op": "clock"}, {"op": "emit", "sink": "s", "data": "x"}, {"op": "rand", "n": 1})),
            {"t": "abort", "detail": "x"}, {"t": "end"},
        ])
        self.assertEqual([m["t"] for m in out], ["ready", "clock", "done"])
        self.assertEqual(out[-1], {"t": "done", "outcome": "aborted"})

    def test_a_handler_that_swallows_everything_still_ends_aborted(self):
        class Stubborn:
            def handle(self, env, input):
                for _ in range(3):
                    try:
                        env.now_ns()
                    except BaseException:  # noqa: BLE001
                        pass
                env.emit("late", b"x")

        code, out = play(Stubborn, [HELLO, step(ops()), {"t": "abort", "detail": ""}, {"t": "end"}])
        self.assertEqual([m["t"] for m in out], ["ready", "clock", "done"])
        self.assertEqual(out[-1]["outcome"], "aborted")

    def test_next_step_after_abort_runs_normally(self):
        code, out = play(Conformance, [
            HELLO, step(ops({"op": "clock"})), {"t": "abort"}, step(ops(), "1"), {"t": "end"},
        ])
        self.assertEqual([m.get("outcome") for m in out if m["t"] == "done"], ["aborted", "ok"])

    def test_except_exception_in_handler_cannot_catch_abort(self):
        class Careless:
            def handle(self, env, input):
                try:
                    env.now_ns()
                except Exception:
                    env.emit("caught", b"")

        code, out = play(Careless, [HELLO, step(ops()), {"t": "abort"}, {"t": "end"}])
        self.assertNotIn("emit", [m["t"] for m in out])


class Gateways(unittest.TestCase):
    def query(self):
        return step(ops({"op": "gateway", "gateway": "g", "request": "q"}))

    def test_error_answer_raises_gateway_error(self):
        code, out = play(Conformance, [HELLO, self.query(), {"t": "gateway", "error": "timeout"}], gateways=AnyGateway())
        emit = out[2]
        self.assertEqual(base64.b64decode(emit["data"]), b'{"error":"timeout"}')

    def test_unregistered_gateway_fails_the_step_without_a_request(self):
        code, out = play(Conformance, [HELLO, self.query()], gateways={})
        self.assertEqual([m["t"] for m in out], ["ready", "done"])
        self.assertEqual(out[1]["outcome"], "panic")
        self.assertIn("not registered", out[1]["message"])

    def test_scope_is_reported(self):
        code, out = play(Conformance, [HELLO, self.query()], gateways={"g": Gateway(lambda r: b"", "local")})
        self.assertEqual(out[1]["scope"], "local")

    def test_live_answer_executes_locally_and_reports_observed(self):
        calls = []

        def conn(req):
            calls.append(req)
            return b"live-response"

        code, out = play(
            Conformance,
            [{**HELLO, "mode": "sandbox"}, self.query(), {"t": "gateway", "live": True}, {"t": "end"}],
            gateways={"g": Gateway(conn, "local")},
        )
        self.assertEqual(calls, [b"q"])
        obs = [m for m in out if m["t"] == "observed"][0]
        self.assertEqual(base64.b64decode(obs["response"]), b"live-response")
        emit = [m for m in out if m["t"] == "emit"][0]
        self.assertEqual(base64.b64decode(emit["data"]), b"live-response")

    def test_live_failure_is_observed_as_error(self):
        def conn(req):
            raise OSError("ENOENT")

        code, out = play(
            Conformance, [{**HELLO, "mode": "sandbox"}, self.query(), {"t": "gateway", "live": True}, {"t": "end"}],
            gateways={"g": Gateway(conn, "local")},
        )
        obs = [m for m in out if m["t"] == "observed"][0]
        self.assertEqual(obs, {"t": "observed", "error": "ENOENT"})


class Sandbox(unittest.TestCase):
    class H:
        def handle(self, env, input):
            env.emit("shm", b"local-data", local=True)
            env.emit("db", b"remote-data")
            if input.data == b"fail":
                raise HandlerError("no")

    def test_local_setup_runs_before_ready_in_sandbox_only(self):
        order = []
        play(self.H, [HELLO], local_setup=lambda: order.append("setup"))
        self.assertEqual(order, [])
        code, out = play(self.H, [{**HELLO, "mode": "sandbox"}], local_setup=lambda: order.append("setup"))
        self.assertEqual(order, ["setup"])
        self.assertEqual(out[0]["t"], "ready")

    def test_failing_local_setup_is_fatal(self):
        def bad():
            raise OSError("no shm")

        code, out = play(self.H, [{**HELLO, "mode": "sandbox"}], local_setup=bad)
        self.assertEqual((code, out[0]["t"]), (1, "fatal"))

    def test_local_outputs_delivered_after_success_in_sandbox_only(self):
        got = []
        sb = {**HELLO, "mode": "sandbox"}
        play(self.H, [sb, step(base64.b64encode(b"ok").decode()), step(base64.b64encode(b"fail").decode(), "1")],
             deliver_local=lambda outs: got.append([o.sink for o in outs]))
        self.assertEqual(got, [["shm"]])  # the failed step delivered nothing
        got.clear()
        play(self.H, [HELLO, step(base64.b64encode(b"ok").decode())], deliver_local=got.append)
        self.assertEqual(got, [])

    def test_emit_scope_on_the_wire(self):
        code, out = play(self.H, [HELLO, step(base64.b64encode(b"ok").decode())])
        self.assertEqual([m["scope"] for m in out if m["t"] == "emit"], ["local", "remote"])


if __name__ == "__main__":
    unittest.main()
