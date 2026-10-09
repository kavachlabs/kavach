import struct
import unittest

from kavach import HandlerError, Panic, wire
from kavach.core import Env, Failure, Invariant, check_invariants, classify, resolve_gateway, UnknownGatewayError, Gateway

from .helpers import split_frames


class Wire(unittest.TestCase):
    def test_uvarint(self):
        self.assertEqual(wire.uvarint(0), b"\x00")
        self.assertEqual(wire.uvarint(127), b"\x7f")
        self.assertEqual(wire.uvarint(128), b"\x80\x01")
        self.assertEqual(wire.uvarint(300), b"\xac\x02")
        self.assertEqual(wire.uvarint(2**64 - 1), b"\xff" * 9 + b"\x01")
        with self.assertRaises(ValueError):
            wire.uvarint(-1)

    def test_frame_length_covers_kind_and_payload(self):
        self.assertEqual(wire.frame(wire.STEP_END), b"\x01\x03")
        self.assertEqual(wire.close_frame(), b"\x01\x07")
        big = wire.frame(wire.SNAPSHOT, wire.bytes_field(b"x" * 200))
        self.assertEqual(split_frames(big)[0][0], wire.SNAPSHOT)

    def test_input_record(self):
        f = wire.input_record("s", "p", b"\x00\xff")
        self.assertEqual(f, bytes([10, 0x02, 0x01, 0x00]) + b"\x01s\x01p\x02\x00\xff")

    def test_clock_is_little_endian_i64(self):
        f = wire.clock_record(-2)
        self.assertEqual(f[-8:], struct.pack("<q", -2))

    def test_critical_flags(self):
        # Records the spec says writers MUST mark critical (§4.7, §4.9).
        self.assertEqual(wire.gateway_record("g", b"", b"", "", False)[2:4], bytes([wire.GATEWAY, 1]))
        self.assertEqual(wire.config_record("k", None, "env")[2:4], bytes([wire.CONFIG, 1]))
        for f in (wire.input_record("a", "b", b""), wire.clock_record(1), wire.rand_record(b"x"),
                  wire.output_record("s", b"", False), wire.marker_record("panic", "m")):
            self.assertEqual(f[3], 0)

    def test_output_scope(self):
        self.assertEqual(wire.output_record("s", b"d", False)[-1], 0)
        self.assertEqual(wire.output_record("s", b"d", True)[-1], 1)

    def test_config_present(self):
        self.assertEqual(wire.config_record("k", b"", "e"), wire.config_record("k", b"", "e"))
        self.assertNotEqual(wire.config_record("k", b"", "e"), wire.config_record("k", None, "e"))

    def test_facts(self):
        f = wire.facts_frame({"host.runtime": b"cpython-3"})
        kind, payload = split_frames(f)[0]
        self.assertEqual(kind, wire.FACTS)
        self.assertEqual(payload, b"\x01" + wire.string_field("host.runtime") + b"\x00" + wire.bytes_field(b"cpython-3"))

    def test_flush_and_snapshot(self):
        self.assertEqual(wire.flush_frame(True), b"\x02\x06\x01")
        self.assertEqual(wire.flush_frame(False), b"\x02\x06\x00")
        self.assertEqual(wire.snapshot_frame(b"ab"), b"\x04\x05\x02ab")


class FailureMapping(unittest.TestCase):
    def test_panic_message_is_exact(self):
        f = classify(Panic("boom: x"))
        self.assertEqual((f.kind, f.message), ("panic", "boom: x"))

    def test_handler_error_message_is_exact(self):
        f = classify(HandlerError("amount is null"))
        self.assertEqual((f.kind, f.message), ("error", "amount is null"))

    def test_uncaught_exception_is_a_panic_with_type_name(self):
        try:
            {}["amount"]
        except KeyError as e:
            f = classify(e)
        self.assertEqual((f.kind, f.message), ("panic", "KeyError: 'amount'"))
        self.assertIn("Traceback", f.detail)

    def test_empty_exception(self):
        self.assertEqual(classify(ValueError()).message, "ValueError: ")

    def test_invariants_first_failure_in_order(self):
        class H:
            def invariants(self):
                def bad():
                    raise AssertionError("no")

                return [Invariant("a", lambda: None), Invariant("b", bad), Invariant("c", lambda: False)]

        f = check_invariants(H())
        self.assertEqual((f.kind, f.message, f.detail), ("invariant", "b", "no"))
        self.assertIsNone(check_invariants(object()))

    def test_invariant_returning_false_fails(self):
        class H:
            def invariants(self):
                return [Invariant("x", lambda: False)]

        self.assertEqual(check_invariants(H()).message, "x")


class Gateways(unittest.TestCase):
    def test_forms(self):
        f = lambda r: r
        self.assertEqual(resolve_gateway({"a": f}, "a").scope, "remote")
        self.assertEqual(resolve_gateway({"a": (f, "local")}, "a").scope, "local")
        self.assertEqual(resolve_gateway({"a": Gateway(f, "local")}, "a").scope, "local")

    def test_unknown_and_bad_scope(self):
        with self.assertRaises(UnknownGatewayError):
            resolve_gateway({}, "nope")
        with self.assertRaises(UnknownGatewayError):
            resolve_gateway(None, "nope")
        with self.assertRaises(ValueError):
            resolve_gateway({"a": (lambda r: r, "galaxy")}, "a")


class Clock(unittest.TestCase):
    def test_now_is_aware_utc(self):
        class E(Env):
            def now_ns(self):
                return 1759752000_123456789

        t = E().now()
        self.assertEqual(t.utcoffset().total_seconds(), 0)
        self.assertEqual((t.year, t.month, t.day, t.microsecond), (2025, 10, 6, 123456))


if __name__ == "__main__":
    unittest.main()
