// Encoding of the record stream (SPEC §10.2) and record payloads (§4).

export const FrameKind = {
  open: 0x01,
  record: 0x02,
  stepEnd: 0x03,
  facts: 0x04,
  snapshot: 0x05,
  flush: 0x06,
  close: 0x07,
} as const;

export const RecordType = {
  input: 0x01,
  clock: 0x02,
  rand: 0x03,
  output: 0x04,
  marker: 0x05,
  snapshot: 0x06,
  gateway: 0x07,
  environment: 0x08,
  config: 0x09,
} as const;

const FLAG_CRITICAL = 1;

/** A growable little-endian byte writer. */
export class Writer {
  private buf = new Uint8Array(128);
  private len = 0;

  private reserve(n: number): void {
    if (this.len + n <= this.buf.length) return;
    let cap = this.buf.length * 2;
    while (cap < this.len + n) cap *= 2;
    const next = new Uint8Array(cap);
    next.set(this.buf.subarray(0, this.len));
    this.buf = next;
  }

  u8(v: number): this {
    this.reserve(1);
    this.buf[this.len++] = v & 0xff;
    return this;
  }

  /** Signed 64-bit little-endian. */
  i64(v: bigint): this {
    this.reserve(8);
    new DataView(this.buf.buffer, this.buf.byteOffset + this.len, 8).setBigInt64(0, v, true);
    this.len += 8;
    return this;
  }

  /** Unsigned LEB128, as Go's binary.PutUvarint writes it. */
  uvarint(v: number | bigint): this {
    let x = BigInt(v);
    if (x < 0n) throw new RangeError("uvarint must not be negative");
    while (x >= 0x80n) {
      this.u8(Number(x & 0x7fn) | 0x80);
      x >>= 7n;
    }
    return this.u8(Number(x));
  }

  raw(b: Uint8Array): this {
    this.reserve(b.length);
    this.buf.set(b, this.len);
    this.len += b.length;
    return this;
  }

  bytes(b: Uint8Array): this {
    return this.uvarint(b.length).raw(b);
  }

  string(s: string): this {
    return this.bytes(Buffer.from(s, "utf8"));
  }

  finish(): Uint8Array {
    return this.buf.slice(0, this.len);
  }
}

/** `len(kind+payload)` uvarint, `kind`, `payload`. */
export function frame(kind: number, payload: Uint8Array = new Uint8Array(0)): Uint8Array {
  return new Writer().uvarint(payload.length + 1).u8(kind).raw(payload).finish();
}

function recordFrame(type: number, critical: boolean, fields: Writer): Uint8Array {
  const body = new Writer().u8(type).u8(critical ? FLAG_CRITICAL : 0).raw(fields.finish());
  return frame(FrameKind.record, body.finish());
}

const scopeByte = (local: boolean | undefined) => (local ? 1 : 0);

export function openFrame(open: Record<string, unknown>): Uint8Array {
  return frame(FrameKind.open, Buffer.from(JSON.stringify(open), "utf8"));
}

export function inputFrame(source: string, position: string, data: Uint8Array): Uint8Array {
  return recordFrame(RecordType.input, false, new Writer().string(source).string(position).bytes(data));
}

export function clockFrame(unixNanos: bigint): Uint8Array {
  return recordFrame(RecordType.clock, false, new Writer().i64(unixNanos));
}

export function randFrame(data: Uint8Array): Uint8Array {
  return recordFrame(RecordType.rand, false, new Writer().bytes(data));
}

export function outputFrame(sink: string, data: Uint8Array, local: boolean): Uint8Array {
  return recordFrame(RecordType.output, false, new Writer().string(sink).bytes(data).u8(scopeByte(local)));
}

export function markerFrame(kind: string, message: string, data: Uint8Array | string = ""): Uint8Array {
  const d = typeof data === "string" ? Buffer.from(data, "utf8") : data;
  return recordFrame(RecordType.marker, false, new Writer().string(kind).string(message).bytes(d));
}

export function gatewayFrame(
  gateway: string,
  request: Uint8Array,
  response: Uint8Array,
  error: string,
  local: boolean,
): Uint8Array {
  return recordFrame(
    RecordType.gateway,
    true,
    new Writer().string(gateway).bytes(request).bytes(response).string(error).u8(scopeByte(local)),
  );
}

export function configFrame(key: string, value: Uint8Array | undefined, source: string): Uint8Array {
  return recordFrame(
    RecordType.config,
    true,
    new Writer().string(key).u8(value ? 1 : 0).bytes(value ?? new Uint8Array(0)).string(source),
  );
}

/** A `facts` frame holding only value-form (0) facts. */
export function factsFrame(facts: Map<string, Uint8Array>): Uint8Array {
  const w = new Writer().uvarint(facts.size);
  for (const [k, v] of facts) w.string(k).u8(0).bytes(v);
  return frame(FrameKind.facts, w.finish());
}

export function snapshotFrame(data: Uint8Array): Uint8Array {
  return frame(FrameKind.snapshot, new Writer().bytes(data).finish());
}

export function flushFrame(durable: boolean): Uint8Array {
  return frame(FrameKind.flush, new Writer().u8(durable ? 1 : 0).finish());
}

export const stepEndFrame = (): Uint8Array => frame(FrameKind.stepEnd);
export const closeFrame = (): Uint8Array => frame(FrameKind.close);

export function concat(parts: Uint8Array[]): Uint8Array {
  let n = 0;
  for (const p of parts) n += p.length;
  const out = new Uint8Array(n);
  let i = 0;
  for (const p of parts) {
    out.set(p, i);
    i += p.length;
  }
  return out;
}

/** A plain Uint8Array over the same memory (Buffers become plain arrays). */
export function u8(b: Uint8Array): Uint8Array {
  return new Uint8Array(b.buffer, b.byteOffset, b.byteLength);
}

export function toBytes(v: Uint8Array | string): Uint8Array {
  return typeof v === "string" ? new Uint8Array(Buffer.from(v, "utf8")) : u8(v);
}
