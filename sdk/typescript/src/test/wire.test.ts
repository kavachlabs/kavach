import assert from "node:assert/strict";
import { test } from "node:test";
import { Writer, clockFrame, configFrame, factsFrame, gatewayFrame, inputFrame, markerFrame, snapshotFrame, stepEndFrame } from "../wire.js";

const hex = (b: Uint8Array) => Buffer.from(b).toString("hex");
const u = (s: string) => new Uint8Array(Buffer.from(s));

test("uvarint matches Go's PutUvarint", () => {
  for (const [n, want] of [
    [0, "00"],
    [1, "01"],
    [127, "7f"],
    [128, "8001"],
    [300, "ac02"],
    [16384, "808001"],
  ] as const) {
    assert.equal(hex(new Writer().uvarint(n).finish()), want, `uvarint ${n}`);
  }
});

test("input frame bytes", () => {
  // len=9 | kind=02 | type=01 flags=00 | "a" | "b" | data 01
  assert.equal(hex(inputFrame("a", "b", new Uint8Array([1]))), "09" + "02" + "0100" + "0161" + "0162" + "0101");
});

test("step_end and snapshot frames", () => {
  assert.equal(hex(stepEndFrame()), "0103");
  assert.equal(hex(snapshotFrame(u("x"))), "03" + "05" + "01" + "78");
});

test("clock is a little-endian i64", () => {
  assert.equal(hex(clockFrame(1n)), "0b" + "02" + "0200" + "0100000000000000");
  assert.equal(hex(clockFrame(-1n)), "0b" + "02" + "0200" + "ffffffffffffffff");
});

test("gateway and config records are critical, others are not", () => {
  const g = gatewayFrame("g", u("q"), u("r"), "", false);
  assert.equal(g[2], 0x07);
  assert.equal(g[3], 1);
  const c = configFrame("k", undefined, "env");
  assert.equal(c[3], 1);
  const m = markerFrame("panic", "boom", "stack");
  assert.equal(m[3], 0);
});

test("facts frame", () => {
  const f = factsFrame(new Map([["host.runtime", u("node-22")]]));
  assert.equal(f[1], 0x04);
  assert.equal(f[2], 1); // one fact
  assert.equal(f[3], "host.runtime".length);
});
