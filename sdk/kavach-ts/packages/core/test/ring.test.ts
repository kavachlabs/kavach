import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import {
  FLAG_CRITICAL,
  FrameDecoder,
  FrameKind,
  MarkerKind,
  OFFSET_CAPACITY,
  OFFSET_MAGIC,
  OFFSET_READ,
  OFFSET_WRITE,
  RING_HEADER_BYTES,
  RING_MAGIC,
  RING_MIN_CAPACITY,
  RecordType,
  Ring,
  RingWriter,
  appendRecordFrame,
  decodeRecord,
  openRingPath,
} from "@kavach/core";

test("KVRING02 header layout, wrap-around, and FrameDecoder round-trip", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "kavach-ts-ring-"));
  const ringPath = path.join(dir, "events.shm");
  try {
    const r = Ring.create(ringPath, RING_MIN_CAPACITY);
    const raw = fs.readFileSync(ringPath);
    assert.equal(raw.length, RING_HEADER_BYTES + RING_MIN_CAPACITY);
    assert.equal(raw.subarray(OFFSET_MAGIC, OFFSET_MAGIC + 8).toString("ascii"), RING_MAGIC);
    assert.equal(raw.readBigUInt64LE(OFFSET_CAPACITY), BigInt(RING_MIN_CAPACITY));
    assert.equal(raw.readBigUInt64LE(OFFSET_WRITE), 0n);
    assert.equal(raw.readBigUInt64LE(OFFSET_READ), 0n);

    const w = RingWriter.fromRing(r, { service: "billing", producer: "kavach-ts/0.2.0" });
    w.writeStep(
      [
        {
          type: RecordType.Input,
          source: "kafka:events_raw",
          position: "0:1",
          data: Buffer.from('{"transaction_id":"tx_1"}'),
        },
        {
          type: RecordType.Clock,
          unixNanos: 1_700_000_000_123_456_789n,
        },
        {
          type: RecordType.Rand,
          data: new Uint8Array([1, 2, 3, 4]),
        },
        {
          type: RecordType.Gateway,
          flags: FLAG_CRITICAL,
          gateway: "pg:select",
          request: Buffer.from('{"driver":"pg"}'),
          response: Buffer.from('{"columns":["id"],"rows":[[{"t":"string","v":"sub_1"}]]}'),
          error: "",
          scope: 0,
        },
        {
          type: RecordType.Output,
          sink: "kafka:events_charged_in_advance",
          data: Buffer.from('{"transaction_id":"tx_1"}'),
          scope: 0,
        },
      ],
      true,
    );
    w.close();

    const reader = Ring.open(ringPath);
    const chunk = reader.pollOnce();
    assert.ok(chunk && chunk.length > 0);
    const dec = new FrameDecoder();
    dec.feed(chunk);
    const drained = dec.finishEOF();
    assert.equal(drained.cleanClose, true);
    assert.equal(drained.discardedTornBytes, 0);
    assert.equal(drained.synthesizedMarker, undefined);
    // Open + Input + Clock + Rand + Gateway + Output + StepEnd + Close = 8 frames
    assert.equal(drained.frames.length, 8);
    assert.equal(drained.frames[0].kind, FrameKind.Open);
    assert.equal(drained.frames[6].kind, FrameKind.StepEnd);
    assert.equal(drained.frames[7].kind, FrameKind.Close);

    const recIn = decodeRecord(drained.frames[1].payload);
    assert.equal(recIn.type, RecordType.Input);
    assert.equal(recIn.source, "kafka:events_raw");
    assert.equal(recIn.position, "0:1");

    const recClock = decodeRecord(drained.frames[2].payload);
    assert.equal(recClock.type, RecordType.Clock);
    assert.equal(recClock.unixNanos, 1_700_000_000_123_456_789n);

    const recGw = decodeRecord(drained.frames[4].payload);
    assert.equal(recGw.type, RecordType.Gateway);
    assert.equal(recGw.flags, FLAG_CRITICAL);
    assert.equal(recGw.gateway, "pg:select");
    reader.close();
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("KVRING02 circular wrap-around and non-blocking drop-on-full", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "kavach-ts-ring-wrap-"));
  const ringPath = path.join(dir, "wrap.shm");
  try {
    const r = Ring.create(ringPath, RING_MIN_CAPACITY);
    const payload = new Uint8Array(20_000).fill(0x41);
    assert.equal(r.tryPublish(payload).ok, true);
    assert.equal(r.tryPublish(payload).ok, true);
    assert.equal(r.tryPublish(payload).ok, true);
    // 60_000 used of 65_536 -> next 20_000 drops non-blockingly!
    assert.equal(r.tryPublish(payload).ok, false);
    assert.equal(r.dropped(), 1n);

    // Drain and wrap around past 65_536 boundary
    const drained = r.pollOnce();
    assert.equal(drained?.length, 60_000);
    const wrapped = new Uint8Array(12_000);
    for (let i = 0; i < wrapped.length; i++) wrapped[i] = i & 0xff;
    assert.equal(r.tryPublish(wrapped).ok, true);
    const readBack = r.pollOnce();
    assert.deepEqual(readBack, wrapped);
    r.close();
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("openRingPath attaches to pre-created KVRING02 ring and supports stream fallback", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "kavach-ts-ring-open-"));
  const ringPath = path.join(dir, "pre.shm");
  try {
    // Pre-create like `kavach dev`
    const pre = Ring.create(ringPath, RING_MIN_CAPACITY);
    pre.close();

    const w = openRingPath(ringPath, RING_MIN_CAPACITY, { service: "svc-a" });
    assert.ok(w.ring());
    w.writeRecord({
      type: RecordType.Marker,
      flags: FLAG_CRITICAL,
      kind: MarkerKind.Error,
      message: "boom",
    });
    w.close();

    const reader = Ring.open(ringPath);
    const bytes = reader.pollOnce();
    assert.ok(bytes);
    const dec = new FrameDecoder();
    dec.feed(bytes);
    const out = dec.finishEOF();
    assert.equal(out.cleanClose, true);
    assert.equal(out.frames.length, 3); // Open + Marker + Close
    reader.close();

    // Stream fallback sink
    const chunks: Uint8Array[] = [];
    const sw = RingWriter.fromStream(
      { write: (c) => chunks.push(new Uint8Array(c)) },
      { service: "stream-svc" },
    );
    sw.writeRaw(appendRecordFrame({ type: RecordType.Clock, unixNanos: 42n }));
    sw.close();
    const sdec = new FrameDecoder();
    for (const c of chunks) sdec.feed(c);
    const sd = sdec.finishEOF();
    assert.equal(sd.cleanClose, true);
    assert.equal(sd.frames.length, 3);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
