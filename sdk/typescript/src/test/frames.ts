// Test helper: decodes a record stream (SPEC §10.2) into plain objects.

export interface RawFrame {
  kind: number;
  payload: Buffer;
}

export function parseFrames(buf: Buffer): RawFrame[] {
  const out: RawFrame[] = [];
  let i = 0;
  while (i < buf.length) {
    let len = 0;
    let shift = 0;
    for (;;) {
      const c = buf[i++]!;
      len |= (c & 0x7f) << shift;
      if (c < 0x80) break;
      shift += 7;
    }
    out.push({ kind: buf[i]!, payload: buf.subarray(i + 1, i + len) });
    i += len;
  }
  return out;
}

class Reader {
  i = 0;
  constructor(readonly b: Buffer) {}
  u8() {
    return this.b[this.i++]!;
  }
  uvarint() {
    let x = 0;
    let shift = 0;
    for (;;) {
      const c = this.u8();
      x |= (c & 0x7f) << shift;
      if (c < 0x80) return x;
      shift += 7;
    }
  }
  bytes() {
    const n = this.uvarint();
    const out = this.b.subarray(this.i, this.i + n);
    this.i += n;
    return out;
  }
  string() {
    return this.bytes().toString("utf8");
  }
  i64() {
    const v = this.b.readBigInt64LE(this.i);
    this.i += 8;
    return v;
  }
}

const NAMES: Record<number, string> = { 1: "open", 2: "record", 3: "step_end", 4: "facts", 5: "snapshot", 6: "flush", 7: "close" };
const TYPES: Record<number, string> = { 1: "input", 2: "clock", 3: "rand", 4: "output", 5: "marker", 7: "gateway", 9: "config" };

export function decode(f: RawFrame): Record<string, unknown> {
  const frame = NAMES[f.kind] ?? `unknown-${f.kind}`;
  const r = new Reader(f.payload);
  switch (f.kind) {
    case 1:
      return { frame, open: JSON.parse(f.payload.toString("utf8")) };
    case 2: {
      const t = r.u8();
      const critical = (r.u8() & 1) === 1;
      const type = TYPES[t] ?? `unknown-${t}`;
      const rec: Record<string, unknown> = { frame, type, critical };
      if (t === 1) Object.assign(rec, { source: r.string(), position: r.string(), data: r.bytes().toString("utf8") });
      else if (t === 2) rec.unix_nanos = r.i64().toString();
      else if (t === 3) rec.data = r.bytes().toString("hex");
      else if (t === 4) Object.assign(rec, { sink: r.string(), data: r.bytes().toString("utf8"), local: r.u8() === 1 });
      else if (t === 5) Object.assign(rec, { kind: r.string(), message: r.string(), data: r.bytes().toString("utf8") });
      else if (t === 7)
        Object.assign(rec, {
          gateway: r.string(),
          request: r.bytes().toString("utf8"),
          response: r.bytes().toString("utf8"),
          error: r.string(),
          local: r.u8() === 1,
        });
      else if (t === 9)
        Object.assign(rec, { key: r.string(), present: r.u8() === 1, value: r.bytes().toString("utf8"), source: r.string() });
      return rec;
    }
    case 4: {
      const facts: Record<string, string> = {};
      for (let n = r.uvarint(); n > 0; n--) {
        const k = r.string();
        r.u8();
        facts[k] = r.bytes().toString("utf8");
      }
      return { frame, facts };
    }
    case 5:
      return { frame, data: r.bytes().toString("utf8") };
    case 6:
      return { frame, durable: r.u8() === 1 };
    default:
      return { frame };
  }
}
