import json
import logging
import os
import sys
import time
import unittest

import kavach
from kavach import Gateway, GatewayError, HandlerError, Input, Panic, Recorder, RecorderError, wire
from kavach.conformance import Conformance

from .helpers import open_obj, read_stream, stub_command, tmpdir

NOREC = ["/nonexistent/kavach-recorder"]


def inp(ops: str, pos="0") -> Input:
    return Input("test", pos, ops.encode())


class Echo:
    """Emits its input; panics/errors on request."""

    def handle(self, env, input):
        env.emit("out", input.data)
        if input.data == b"panic":
            raise Panic("kaboom")
        if input.data == b"error":
            raise HandlerError("nope")
        if input.data == b"key":
            {}["amount"]


class FrameLevel(unittest.TestCase):
    def run_steps(self, handler, inputs, mode="ok", **kw):
        with tmpdir() as tmp:
            cmd, dump = stub_command(tmp, mode)
            rec = Recorder(handler, service="svc", recorder_command=cmd, **kw)
            results = [rec.step(i) for i in inputs]
            rec.close()
            return results, read_stream(dump)

    def test_open_facts_and_order(self):
        results, frames = self.run_steps(Echo(), [Input("s", "1", b"hi")], dir="/tmp/j", level=5, secret_keys=["X"])
        self.assertEqual([k for k, _ in frames], [wire.OPEN, wire.FACTS, wire.RECORD, wire.RECORD, wire.STEP_END, wire.CLOSE])
        o = open_obj(frames)
        self.assertEqual((o["protocol"], o["service"], o["start"], o["snapshots"]), (1, "svc", "genesis", False))
        self.assertEqual((o["dir"], o["level"], o["secret_keys"]), ("/tmp/j", 5, ["X"]))
        self.assertTrue(o["producer"].startswith("kavach-python/"))
        self.assertNotIn("compression", o)
        self.assertTrue(results[0].ok)

    def test_panic_marker(self):
        results, frames = self.run_steps(Echo(), [Input("s", "1", b"panic")])
        r = results[0]
        self.assertEqual((r.ok, r.kind, r.message), (False, "panic", "kaboom"))
        recs = [p for k, p in frames if k == wire.RECORD]
        self.assertEqual(recs[-1][0], wire.MARKER)
        self.assertIn(b"panic", recs[-1])
        self.assertIn(b"kaboom", recs[-1])

    def test_uncaught_exception_marker_message_and_traceback(self):
        results, frames = self.run_steps(Echo(), [Input("s", "1", b"key")])
        self.assertEqual(results[0].message, "KeyError: 'amount'")
        last = [p for k, p in frames if k == wire.RECORD][-1]
        self.assertIn(b"Traceback", last)

    def test_error_marker_and_stepping_continues(self):
        results, frames = self.run_steps(Echo(), [Input("s", "1", b"error"), Input("s", "2", b"ok")])
        self.assertEqual((results[0].kind, results[0].message), ("error", "nope"))
        self.assertTrue(results[1].ok)
        self.assertEqual(sum(1 for k, _ in frames if k == wire.STEP_END), 2)

    def test_raise_for_failure(self):
        results, _ = self.run_steps(Echo(), [Input("s", "1", b"panic")])
        with self.assertRaises(Panic):
            results[0].raise_for_failure()

    def test_input_is_written_before_the_handler_runs(self):
        with tmpdir() as tmp:
            cmd, dump = stub_command(tmp)
            seen = []

            class H:
                def handle(self, env, input):
                    import time

                    time.sleep(0.3)  # let the stub drain the pipe
                    seen.append([k for k, _ in read_stream(dump)])

            rec = Recorder(H(), service="svc", recorder_command=cmd)
            rec.step(Input("s", "1", b"x"))
            rec.close()
        self.assertEqual(seen[0], [wire.OPEN, wire.FACTS, wire.RECORD])

    def test_outputs_delivered_only_after_success(self):
        delivered = []
        results, _ = self.run_steps(
            Echo(), [Input("s", "1", b"a"), Input("s", "2", b"panic"), Input("s", "3", b"b")],
            deliver=lambda outs: delivered.append([o.data for o in outs]),
        )
        self.assertEqual(delivered, [[b"a"], [b"b"]])

    def test_invariant_failure_is_recorded_and_not_delivered(self):
        delivered = []
        h = Conformance()
        h.count = 999
        results, frames = self.run_steps(h, [inp('[{"op":"emit","sink":"s","data":"d"}]')], start="snapshot",
                                         deliver=delivered.append)
        self.assertEqual((results[0].kind, results[0].message), ("invariant", "below_limit"))
        self.assertEqual(delivered, [])
        self.assertEqual([k for k, _ in frames[:4]], [wire.OPEN, wire.FACTS, wire.SNAPSHOT, wire.RECORD])

    def test_keyboard_interrupt_is_recorded_then_reraised(self):
        class H:
            def handle(self, env, input):
                raise KeyboardInterrupt

        with tmpdir() as tmp:
            cmd, dump = stub_command(tmp)
            rec = Recorder(H(), service="svc", recorder_command=cmd)
            with self.assertRaises(KeyboardInterrupt):
                rec.step(Input("s", "1", b""))
            rec.close()
            kinds = [k for k, _ in read_stream(dump)]
        self.assertIn(wire.STEP_END, kinds)

    def test_gateway_scope_and_error_text(self):
        class H:
            def handle(self, env, input):
                for g in ("a", "b"):
                    try:
                        env.query(g, b"q")
                    except GatewayError as e:
                        env.emit("err", e.error.encode())

        def boom(req):
            raise RuntimeError("down")

        results, frames = self.run_steps(H(), [Input("s", "1", b"")], gateways={"a": (lambda r: b"ok", "local"), "b": boom})
        recs = [p for k, p in frames if k == wire.RECORD]
        gws = [p for p in recs if p[0] == wire.GATEWAY]
        self.assertEqual(len(gws), 2)
        self.assertEqual(gws[0][-1], 1)  # local
        self.assertIn(b"down", gws[1])
        self.assertTrue(results[0].ok)

    def test_config_default_provider_reads_environment(self):
        class H:
            def handle(self, env, input):
                env.emit("v", env.config("KAVACH_TEST_CFG") or b"-")

        os.environ["KAVACH_TEST_CFG"] = "yes"
        self.addCleanup(os.environ.pop, "KAVACH_TEST_CFG", None)
        results, _ = self.run_steps(H(), [Input("s", "1", b"")])
        self.assertEqual(results[0].outputs[0].data, b"yes")

    def test_random_zero_bytes_records_nothing(self):
        class H:
            def handle(self, env, input):
                assert env.random(0) == b""

        results, frames = self.run_steps(H(), [Input("s", "1", b"")])
        self.assertTrue(results[0].ok)
        self.assertEqual(sum(1 for k, _ in frames if k == wire.RECORD), 1)

    def test_flush_durable_roundtrip(self):
        with tmpdir() as tmp:
            cmd, dump = stub_command(tmp)
            rec = Recorder(Echo(), service="svc", recorder_command=cmd)
            self.assertTrue(rec.flush(durable=True, timeout=5))
            rec.close()
            self.assertIn(wire.FLUSH, [k for k, _ in read_stream(dump)])

    def test_flush_inside_step_is_refused(self):
        errs = []

        class H:
            def handle(self, env, input):
                try:
                    rec.flush()
                except RuntimeError as e:
                    errs.append(e)

        with tmpdir() as tmp:
            cmd, _ = stub_command(tmp)
            rec = Recorder(H(), service="svc", recorder_command=cmd)
            rec.step(Input("s", "1", b""))
            rec.close()
        self.assertEqual(len(errs), 1)


class RecorderFailure(unittest.TestCase):
    def test_no_recorder_logs_and_steps_run(self):
        with self.assertLogs("kavach", "ERROR") as cm:
            rec = Recorder(Echo(), service="svc", recorder_command=NOREC)
        self.assertIn("recording has stopped", cm.output[0])
        self.assertFalse(rec.recording)
        r = rec.step(Input("s", "1", b"x"))
        self.assertTrue(r.ok)
        self.assertEqual(r.outputs[0].data, b"x")
        self.assertFalse(rec.flush(durable=True))
        rec.close()

    def test_required_fails_startup(self):
        with self.assertRaises(RecorderError):
            Recorder(Echo(), service="svc", recorder_command=NOREC, required=True)

    def test_missing_recorder_everywhere(self):
        old = os.environ.pop("KAVACH_RECORDER", None)
        oldp = os.environ["PATH"]
        os.environ["PATH"] = ""
        try:
            with self.assertLogs("kavach", "ERROR") as cm:
                Recorder(Echo(), service="svc")
            self.assertIn("not found", cm.output[0])
        finally:
            os.environ["PATH"] = oldp
            if old is not None:
                os.environ["KAVACH_RECORDER"] = old

    def test_env_var_selects_recorder(self):
        with tmpdir() as tmp:
            cmd, dump = stub_command(tmp)
            # KAVACH_RECORDER is a single executable; use a wrapper script.
            wrapper = os.path.join(tmp, "rec.sh")
            with open(wrapper, "w") as f:
                f.write("#!/bin/sh\nexec " + " ".join(f"'{c}'" for c in cmd) + "\n")
            os.chmod(wrapper, 0o755)
            os.environ["KAVACH_RECORDER"] = wrapper
            try:
                rec = Recorder(Echo(), service="svc")
                rec.step(Input("s", "1", b"x"))
                rec.close()
            finally:
                del os.environ["KAVACH_RECORDER"]
            self.assertEqual(read_stream(dump)[0][0], wire.OPEN)

    def test_recorder_dies_mid_run(self):
        with tmpdir() as tmp:
            cmd, _ = stub_command(tmp, "die")
            with self.assertLogs("kavach", "ERROR") as cm:
                rec = Recorder(Echo(), service="svc", recorder_command=cmd)
                for i in range(50):  # the pipe breaks at some point; no step fails
                    r = rec.step(Input("s", str(i), b"x"))
                    self.assertTrue(r.ok)
                    if not rec.recording:
                        break
                import time

                for _ in range(100):
                    if not rec.recording:
                        break
                    time.sleep(0.05)
            self.assertFalse(rec.recording)
            self.assertEqual(len([o for o in cm.output if "recording has stopped" in o]), 1)
            self.assertTrue(rec.step(Input("s", "99", b"x")).ok)
            rec.close()

    def test_required_but_dies(self):
        with tmpdir() as tmp:
            cmd, _ = stub_command(tmp, "die")
            with self.assertRaises(RecorderError):
                Recorder(Echo(), service="svc", recorder_command=cmd, required=True)

    def test_fatal_error_stops_recording(self):
        with tmpdir() as tmp:
            cmd, _ = stub_command(tmp, "fatal")
            with self.assertLogs("kavach", "ERROR") as cm:
                rec = Recorder(Echo(), service="svc", recorder_command=cmd)
                import time

                for _ in range(100):
                    if not rec.recording:
                        break
                    time.sleep(0.05)
            self.assertFalse(rec.recording)
            self.assertTrue(any("disk full" in o for o in cm.output))
            self.assertTrue(rec.step(Input("s", "1", b"x")).ok)
            rec.close()

    def test_fixture_messages_are_logged_and_passed_on(self):
        got = []
        with tmpdir() as tmp:
            cmd, _ = stub_command(tmp, "fixture")
            with self.assertLogs("kavach", "WARNING") as cm:
                rec = Recorder(Echo(), service="svc", recorder_command=cmd, on_fixture=got.append, required=True)
                rec.flush(durable=True)  # the fixture line precedes the durable answer
                rec.close()
        self.assertTrue(any("/fx/1.kavach" in o for o in cm.output))
        self.assertEqual(got[0]["seq"], "3")

    def test_close_times_out_quietly(self):
        with tmpdir() as tmp:
            cmd, _ = stub_command(tmp, "silent")
            rec = Recorder(Echo(), service="svc", recorder_command=cmd, close_timeout=0.2)
            self.assertFalse(rec.flush(durable=True, timeout=0.2))
            with self.assertLogs("kavach", "WARNING"):
                rec.close()
        with self.assertRaises(RuntimeError):
            rec.step(Input("s", "1", b""))

    def test_silent_recorder_delays_construction_once(self):
        with tmpdir() as tmp:
            cmd, _ = stub_command(tmp, "silent")
            t0 = time.monotonic()
            rec = Recorder(Echo(), service="svc", recorder_command=cmd, close_timeout=0.2)
            self.assertTrue(1.0 < time.monotonic() - t0 < 3.0)
            t0 = time.monotonic()
            for i in range(100):
                rec.step(inp("x", str(i)))
            self.assertLess(time.monotonic() - t0, 0.5)
            with self.assertLogs("kavach", "WARNING"):
                rec.close()

    def test_snapshot_option_needs_a_snapshotter(self):
        with self.assertRaises(ValueError):
            Recorder(Echo(), service="svc", recorder_command=NOREC, start="snapshot")


if __name__ == "__main__":
    unittest.main()
