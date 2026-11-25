import fs from "node:fs";
import path from "node:path";

/**
 * KVRING02 binary layout constants matching include/kavach/daemon/shm_consumer.hpp
 * and sdk/kavach-go/ring/codec.go byte-for-byte.
 */
export const RING_MAGIC = "KVRING02";
export const RING_HEADER_BYTES = 65536;
export const RING_MIN_CAPACITY = 65536;
export const RING_DEFAULT_CAPACITY = 8 << 20; // 8 MiB
export const OFFSET_MAGIC = 0;
export const OFFSET_CAPACITY = 8;
export const OFFSET_WRITE = 64;
export const OFFSET_READ = 128;
export const MAX_FRAME_LEN = (16 << 20) + 65536;
export const PROTOCOL_VERSION = 1;

export const FrameKind = {
  Open: 0x01,
  Record: 0x02,
  StepEnd: 0x03,
  Facts: 0x04,
  Snapshot: 0x05,
  Flush: 0x06,
  Close: 0x07,
} as const;
export type FrameKindValue = (typeof FrameKind)[keyof typeof FrameKind];

export const RecordType = {
  Input: 0x01,
  Clock: 0x02,
  Rand: 0x03,
  Output: 0x04,
  Marker: 0x05,
  Snapshot: 0x06,
  Gateway: 0x07,
  Environment: 0x08,
  Config: 0x09,
} as const;
export type RecordTypeValue = (typeof RecordType)[keyof typeof RecordType];

export const FLAG_CRITICAL = 1 << 0;
export const SCOPE_REMOTE = 0;
export const SCOPE_LOCAL = 1;

export const FACT_VALUE = 0;
export const FACT_SHA256 = 1;
export const FACT_UNSET = 2;

export const MarkerKind = {
  Panic: "panic",
  Error: "error",
  Trigger: "trigger",
  Invariant: "invariant",
  Crash: "crash",
  Exit: "exit",
  Dropped: "dropped",
} as const;

export interface OpenMeta {
  protocol?: number;
  service: string;
  start?: string;
  producer?: string;
  handler?: string;
  snapshots?: boolean;
  ring?: number;
  ring_path?: string;
}

export interface RingFact {
  key: string;
  form: number;
  value: Uint8Array;
}

export interface RingRecord {
  type: RecordTypeValue;
  flags?: number;
  source?: string;
  position?: string;
  sink?: string;
  kind?: string;
  message?: string;
  unixNanos?: bigint;
  data?: Uint8Array;
  gateway?: string;
  request?: Uint8Array;
  response?: Uint8Array;
  error?: string;
  scope?: number;
  facts?: RingFact[];
  key?: string;
  present?: boolean;
  value?: Uint8Array;
}

export interface StreamFrame {
  kind: FrameKindValue;
  payload: Uint8Array;
}

export interface DrainResult {
  frames: StreamFrame[];
  synthesizedMarker?: RingRecord;
  discardedTornBytes: number;
  cleanClose: boolean;
}

const utf8DecoderStrict = new TextDecoder("utf-8", { fatal: true });

function isValidUtf8(bytes: Uint8Array): boolean {
  try {
    utf8DecoderStrict.decode(bytes);
    return true;
  } catch {
    return false;
  }
}

class ByteBuilder {
  private buf: Uint8Array;
  private len = 0;

  constructor(initialCap = 256) {
    this.buf = new Uint8Array(initialCap);
  }

  private reserve(extra: number): void {
    if (this.len + extra <= this.buf.length) return;
    let nextCap = this.buf.length * 2;
    while (nextCap < this.len + extra) nextCap *= 2;
    const next = new Uint8Array(nextCap);
    next.set(this.buf.subarray(0, this.len));
    this.buf = next;
  }

  u8(v: number): this {
    this.reserve(1);
    this.buf[this.len++] = v & 0xff;
    return this;
  }

  u64LE(v: bigint): this {
    this.reserve(8);
    const dv = new DataView(this.buf.buffer, this.buf.byteOffset + this.len, 8);
    dv.setBigUint64(0, BigInt.asUintN(64, v), true);
    this.len += 8;
    return this;
  }

  uvarint(v: number | bigint): this {
    let x = typeof v === "bigint" ? v : BigInt(v);
    if (x < 0n) throw new RangeError("ring: uvarint cannot be negative");
    while (x >= 0x80n) {
      this.u8(Number(x & 0x7fn) | 0x80);
      x >>= 7n;
    }
    return this.u8(Number(x));
  }

  raw(b: Uint8Array): this {
    if (b.length === 0) return this;
    this.reserve(b.length);
    this.buf.set(b, this.len);
    this.len += b.length;
    return this;
  }

  bytes(b?: Uint8Array): this {
    if (!b || b.length === 0) return this.uvarint(0);
    return this.uvarint(b.length).raw(b);
  }

  str(s?: string): this {
    if (!s) return this.uvarint(0);
    const enc = Buffer.from(s, "utf8");
    return this.uvarint(enc.length).raw(enc);
  }

  finish(): Uint8Array {
    return this.buf.slice(0, this.len);
  }
}

export function encodeUvarint(v: number | bigint): Uint8Array {
  return new ByteBuilder(10).uvarint(v).finish();
}

export function decodeUvarint(buf: Uint8Array, offset = 0): { status: "ok" | "incomplete" | "invalid"; value: bigint; consumed: number } {
  let value = 0n;
  for (let i = 0; offset + i < buf.length; i++) {
    const b = buf[offset + i];
    if (i >= 10 || (i === 9 && b > 0x01)) {
      return { status: "invalid", value: 0n, consumed: 0 };
    }
    value |= BigInt(b & 0x7f) << BigInt(i * 7);
    if ((b & 0x80) === 0) {
      return { status: "ok", value, consumed: i + 1 };
    }
  }
  return { status: "incomplete", value: 0n, consumed: 0 };
}

export function encodeFrame(kind: FrameKindValue, payload?: Uint8Array): Uint8Array {
  const bodyLen = payload ? payload.length : 0;
  const flen = 1 + bodyLen;
  if (flen > MAX_FRAME_LEN) {
    throw new Error(`ring: frame length ${flen} exceeds max ${MAX_FRAME_LEN}`);
  }
  const b = new ByteBuilder(flen + 10);
  b.uvarint(flen).u8(kind);
  if (payload && payload.length > 0) b.raw(payload);
  return b.finish();
}

export function appendOpenFrame(meta: OpenMeta): Uint8Array {
  const wireMeta: Record<string, unknown> = {
    protocol: meta.protocol ?? PROTOCOL_VERSION,
    service: meta.service,
    start: meta.start || "genesis",
    snapshots: meta.snapshots ?? false,
  };
  if (meta.producer) wireMeta.producer = meta.producer;
  if (meta.handler) wireMeta.handler = meta.handler;
  if (meta.ring) wireMeta.ring = meta.ring;
  if (meta.ring_path) wireMeta.ring_path = meta.ring_path;
  return encodeFrame(FrameKind.Open, Buffer.from(JSON.stringify(wireMeta), "utf8"));
}

export function appendStepEndFrame(): Uint8Array {
  return encodeFrame(FrameKind.StepEnd);
}

export function appendFlushFrame(durable: boolean): Uint8Array {
  return encodeFrame(FrameKind.Flush, new Uint8Array([durable ? 1 : 0]));
}

export function appendCloseFrame(): Uint8Array {
  return encodeFrame(FrameKind.Close);
}

export function appendFactsFrame(facts: RingFact[]): Uint8Array {
  const b = new ByteBuilder();
  b.uvarint(facts.length);
  for (const f of facts) {
    b.str(f.key).u8(f.form).bytes(f.value);
  }
  return encodeFrame(FrameKind.Facts, b.finish());
}

export function appendSnapshotFrame(state: Uint8Array): Uint8Array {
  return encodeFrame(FrameKind.Snapshot, new ByteBuilder(state.length + 10).bytes(state).finish());
}

export function encodeRecordPayload(r: RingRecord): Uint8Array {
  const b = new ByteBuilder();
  switch (r.type) {
    case RecordType.Input:
      b.str(r.source).str(r.position).bytes(r.data);
      break;
    case RecordType.Clock:
      b.u64LE(r.unixNanos ?? 0n);
      break;
    case RecordType.Rand:
    case RecordType.Snapshot:
      b.bytes(r.data);
      break;
    case RecordType.Output:
      b.str(r.sink).bytes(r.data).u8(r.scope ?? SCOPE_REMOTE);
      break;
    case RecordType.Marker:
      b.str(r.kind).str(r.message).bytes(r.data);
      break;
    case RecordType.Gateway:
      b.str(r.gateway).bytes(r.request).bytes(r.response).str(r.error).u8(r.scope ?? SCOPE_REMOTE);
      break;
    case RecordType.Environment: {
      const facts = r.facts ?? [];
      b.uvarint(facts.length);
      for (const f of facts) {
        b.str(f.key).u8(f.form).bytes(f.value);
      }
      break;
    }
    case RecordType.Config:
      b.str(r.key).u8(r.present ? 1 : 0).bytes(r.value).str(r.source);
      break;
    default:
      throw new Error(`ring: invalid record type 0x${Number(r.type).toString(16)}`);
  }
  return b.finish();
}

export function appendRecordFrame(r: RingRecord): Uint8Array {
  if (!r.type || r.type === RecordType.Environment) {
    throw new Error(`ring: record type 0x${Number(r.type).toString(16)} cannot be sent in FrameRecord`);
  }
  const flags = r.flags ?? 0;
  if ((flags & ~FLAG_CRITICAL) !== 0) {
    throw new Error(`ring: invalid record flags 0x${flags.toString(16)}`);
  }
  const payload = encodeRecordPayload(r);
  const body = new Uint8Array(2 + payload.length);
  body[0] = r.type;
  body[1] = flags;
  body.set(payload, 2);
  return encodeFrame(FrameKind.Record, body);
}

export function decodeRecord(framePayload: Uint8Array): RingRecord {
  if (framePayload.length < 2) {
    throw new Error("ring: record frame shorter than type+flags");
  }
  const rawTy = framePayload[0];
  const flags = framePayload[1];
  if (rawTy === 0 || (flags & ~FLAG_CRITICAL) !== 0) {
    throw new Error(`ring: invalid record header type=0x${rawTy.toString(16)} flags=0x${flags.toString(16)}`);
  }
  const ty = rawTy as RecordTypeValue;
  return decodeRecordPayload(ty, flags, framePayload.subarray(2));
}

export function decodeRecordPayload(ty: RecordTypeValue, flags: number, b: Uint8Array): RingRecord {
  let off = 0;
  const u8 = (): number => {
    if (off + 1 > b.length) throw new Error("ring: truncated u8");
    return b[off++];
  };
  const u64 = (): bigint => {
    if (off + 8 > b.length) throw new Error("ring: truncated u64");
    const dv = new DataView(b.buffer, b.byteOffset + off, 8);
    off += 8;
    return dv.getBigInt64(0, true);
  };
  const uvarint = (): number => {
    const res = decodeUvarint(b, off);
    if (res.status !== "ok") throw new Error("ring: invalid uvarint");
    off += res.consumed;
    return Number(res.value);
  };
  const bytes = (): Uint8Array => {
    const len = uvarint();
    if (off + len > b.length) throw new Error(`ring: field length ${len} exceeds remaining ${b.length - off}`);
    const out = b.slice(off, off + len);
    off += len;
    return out;
  };
  const str = (): string => {
    const v = bytes();
    if (!isValidUtf8(v)) throw new Error("ring: string field is not valid UTF-8");
    return Buffer.from(v).toString("utf8");
  };

  const r: RingRecord = { type: ty, flags };
  switch (ty) {
    case RecordType.Input:
      r.source = str();
      r.position = str();
      r.data = bytes();
      break;
    case RecordType.Clock:
      r.unixNanos = u64();
      break;
    case RecordType.Rand:
    case RecordType.Snapshot:
      r.data = bytes();
      break;
    case RecordType.Output:
      r.sink = str();
      r.data = bytes();
      r.scope = u8();
      break;
    case RecordType.Marker:
      r.kind = str();
      r.message = str();
      r.data = bytes();
      break;
    case RecordType.Gateway:
      r.gateway = str();
      r.request = bytes();
      r.response = bytes();
      r.error = str();
      r.scope = u8();
      break;
    case RecordType.Environment: {
      const n = uvarint();
      r.facts = [];
      for (let i = 0; i < n; i++) {
        r.facts.push({ key: str(), form: u8(), value: bytes() });
      }
      break;
    }
    case RecordType.Config: {
      r.key = str();
      const p = u8();
      if (p > 1) throw new Error(`ring: invalid config present byte ${p}`);
      r.present = p === 1;
      r.value = bytes();
      r.source = str();
      break;
    }
    default:
      r.data = b.slice();
  }
  return r;
}

/**
 * Mirrors kavach::daemon::FrameStreamDecoder in shm_consumer.hpp.
 */
export class FrameDecoder {
  private pending: Uint8Array = new Uint8Array(0);
  private _frames: StreamFrame[] = [];
  private _activeStep = false;
  private _closed = false;

  activeStep(): boolean {
    return this._activeStep;
  }

  closed(): boolean {
    return this._closed;
  }

  frames(): StreamFrame[] {
    return this._frames;
  }

  feed(chunk: Uint8Array): number {
    if (chunk.length === 0) return 0;
    if (this.pending.length === 0) {
      this.pending = chunk.slice();
    } else {
      const merged = new Uint8Array(this.pending.length + chunk.length);
      merged.set(this.pending);
      merged.set(chunk, this.pending.length);
      this.pending = merged;
    }

    let count = 0;
    let off = 0;
    while (off < this.pending.length) {
      const rem = this.pending.subarray(off);
      const dv = decodeUvarint(rem, 0);
      if (dv.status === "incomplete") break;
      const flen = Number(dv.value);
      if (dv.status === "invalid" || flen === 0 || flen > MAX_FRAME_LEN) {
        throw new Error("ring: invalid frame length");
      }
      if (rem.length - dv.consumed < flen) break;
      if (this._closed) {
        throw new Error("ring: unexpected frame after close");
      }
      const rk = rem[dv.consumed];
      if (rk < FrameKind.Open || rk > FrameKind.Close) {
        throw new Error(`ring: unknown frame kind 0x${rk.toString(16)}`);
      }
      const kind = rk as FrameKindValue;
      const body = rem.subarray(dv.consumed + 1, dv.consumed + flen);
      if (kind === FrameKind.Open && !isValidUtf8(body)) {
        throw new Error("ring: open frame not valid UTF-8");
      } else if (kind === FrameKind.Record) {
        if (
          body.length < 2 ||
          body[0] === 0 ||
          body[0] === RecordType.Environment ||
          (body[1] & ~FLAG_CRITICAL) !== 0
        ) {
          throw new Error("ring: invalid record frame");
        }
        if (body[0] === RecordType.Input) {
          if (this._activeStep) throw new Error("ring: nested input without step_end");
          this._activeStep = true;
        }
      } else if (kind === FrameKind.StepEnd) {
        if (body.length !== 0 || !this._activeStep) throw new Error("ring: invalid step_end");
        this._activeStep = false;
      } else if (kind === FrameKind.Flush) {
        if (body.length !== 1 || body[0] > 1) throw new Error("ring: invalid flush payload");
      } else if (kind === FrameKind.Close) {
        if (body.length !== 0 || this._activeStep) throw new Error("ring: invalid close");
        this._closed = true;
      }
      this._frames.push({ kind, payload: body.slice() });
      off += dv.consumed + flen;
      count++;
    }
    if (off > 0) {
      this.pending = this.pending.slice(off);
    }
    return count;
  }

  finishEOF(): DrainResult {
    const res: DrainResult = {
      frames: this._frames,
      discardedTornBytes: this.pending.length,
      cleanClose: false,
    };
    this._frames = [];
    this.pending = new Uint8Array(0);
    if (this._activeStep) {
      const crash: RingRecord = {
        type: RecordType.Marker,
        flags: FLAG_CRITICAL,
        kind: MarkerKind.Crash,
        message: "process ended during step",
      };
      const p = encodeRecordPayload(crash);
      const body = new Uint8Array(2 + p.length);
      body[0] = RecordType.Marker;
      body[1] = FLAG_CRITICAL;
      body.set(p, 2);
      res.frames.push({ kind: FrameKind.Record, payload: body });
      res.synthesizedMarker = crash;
      this._activeStep = false;
    } else if (!this._closed) {
      res.synthesizedMarker = {
        type: RecordType.Marker,
        kind: MarkerKind.Exit,
        message: "process ended without closing the recorder",
      };
    } else {
      res.cleanClose = true;
    }
    return res;
  }
}

function isValidCapacity(cap: bigint): boolean {
  return cap >= BigInt(RING_MIN_CAPACITY) && (cap & (cap - 1n)) === 0n;
}

/**
 * Lock-free KVRING02 shared-memory ring file writer/reader matching
 * kavach::daemon::ShmRingConsumer (shm_consumer.hpp) and kavach dev byte-for-byte.
 *
 * Uses positioned POSIX pread/pwrite into the kernel unified page cache:
 * data bytes are written into [65536..65536+capacity) before the aligned 8-byte
 * uint64 write_cursor is published at offset 64, guaranteeing lock-free visibility
 * to MAP_SHARED consumers (kavach dev and C++23 ShmRingConsumer) with zero external deps.
 */
export class Ring {
  private fd: number;
  private readonly cap: bigint;
  private readonly mask: bigint;
  private readonly filePath: string;
  private _dropped = 0n;
  private _closed = false;
  private readonly cursorBuf = Buffer.alloc(8);

  private constructor(fd: number, cap: bigint, filePath: string) {
    this.fd = fd;
    this.cap = cap;
    this.mask = cap - 1n;
    this.filePath = filePath;
  }

  static create(filePath: string, capacity = RING_DEFAULT_CAPACITY): Ring {
    const cap = BigInt(capacity || RING_DEFAULT_CAPACITY);
    if (!isValidCapacity(cap)) {
      throw new Error(`ring: capacity ${cap} must be a power of two >= ${RING_MIN_CAPACITY}`);
    }
    const dir = path.dirname(filePath);
    if (dir && dir !== ".") {
      fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
    }
    const fd = fs.openSync(filePath, fs.constants.O_RDWR | fs.constants.O_CREAT | fs.constants.O_TRUNC, 0o600);
    try {
      const total = RING_HEADER_BYTES + Number(cap);
      fs.ftruncateSync(fd, total);
      const hdr = Buffer.alloc(RING_HEADER_BYTES);
      hdr.write(RING_MAGIC, OFFSET_MAGIC, "ascii");
      hdr.writeBigUInt64LE(cap, OFFSET_CAPACITY);
      writeAllSync(fd, hdr, 0, hdr.length, 0);
      return new Ring(fd, cap, filePath);
    } catch (e) {
      fs.closeSync(fd);
      throw e;
    }
  }

  static open(filePath: string): Ring {
    const fd = fs.openSync(filePath, fs.constants.O_RDWR);
    try {
      const st = fs.fstatSync(fd);
      if (st.size < RING_HEADER_BYTES + RING_MIN_CAPACITY) {
        throw new Error(`ring: file ${filePath} too small (${st.size} bytes)`);
      }
      const hdr = Buffer.alloc(16);
      readAllSync(fd, hdr, 0, 16, 0);
      const magic = hdr.subarray(OFFSET_MAGIC, OFFSET_MAGIC + 8).toString("ascii");
      if (magic !== RING_MAGIC) {
        throw new Error(`ring: invalid magic ${JSON.stringify(magic)} (want ${JSON.stringify(RING_MAGIC)})`);
      }
      const cap = hdr.readBigUInt64LE(OFFSET_CAPACITY);
      if (!isValidCapacity(cap) || BigInt(st.size - RING_HEADER_BYTES) < cap) {
        throw new Error(`ring: invalid capacity ${cap} for file size ${st.size}`);
      }
      return new Ring(fd, cap, filePath);
    } catch (e) {
      fs.closeSync(fd);
      throw e;
    }
  }

  static openOrCreate(filePath: string, capacity = RING_DEFAULT_CAPACITY): Ring {
    try {
      const st = fs.statSync(filePath);
      if (st.isFile() && st.size >= RING_HEADER_BYTES + RING_MIN_CAPACITY) {
        return Ring.open(filePath);
      }
    } catch {
      // fall through to create
    }
    return Ring.create(filePath, capacity);
  }

  capacity(): bigint {
    return this.cap;
  }

  path(): string {
    return this.filePath;
  }

  dropped(): bigint {
    return this._dropped;
  }

  writeCursor(): bigint {
    if (this._closed) return 0n;
    readAllSync(this.fd, this.cursorBuf, 0, 8, OFFSET_WRITE);
    return this.cursorBuf.readBigUInt64LE(0);
  }

  readCursor(): bigint {
    if (this._closed) return 0n;
    readAllSync(this.fd, this.cursorBuf, 0, 8, OFFSET_READ);
    return this.cursorBuf.readBigUInt64LE(0);
  }

  /**
   * Non-blocking drop-on-full publish into the circular data region followed by
   * an aligned 8-byte write_cursor update at offset 64.
   */
  tryPublish(p: Uint8Array): { ok: boolean; used: bigint } {
    if (p.length === 0) return { ok: true, used: 0n };
    if (this._closed) return { ok: false, used: 0n };

    const w = this.writeCursor();
    const r = this.readCursor();
    const used = w - r;
    const n = BigInt(p.length);
    if (used > this.cap || n > this.cap - used) {
      this._dropped++;
      return { ok: false, used };
    }

    const pos = Number(w & this.mask);
    const first = Number(this.cap) - pos;
    if (first >= p.length) {
      writeAllSync(this.fd, p, 0, p.length, RING_HEADER_BYTES + pos);
    } else {
      writeAllSync(this.fd, p, 0, first, RING_HEADER_BYTES + pos);
      writeAllSync(this.fd, p, first, p.length - first, RING_HEADER_BYTES);
    }

    // Publish new write cursor AFTER all payload bytes are committed to the page cache.
    this.cursorBuf.writeBigUInt64LE(w + n, 0);
    writeAllSync(this.fd, this.cursorBuf, 0, 8, OFFSET_WRITE);
    return { ok: true, used: used + n };
  }

  /** Drains unread bytes and advances read_cursor at offset 128. Mirrors ShmRingConsumer::poll_once. */
  pollOnce(): Uint8Array | null {
    if (this._closed) throw new Error("ring: closed");
    const w = this.writeCursor();
    const r = this.readCursor();
    if (w < r) throw new Error("ring: write cursor moved backwards");
    const avail = w - r;
    if (avail > this.cap) throw new Error(`ring: unread bytes ${avail} exceed capacity ${this.cap}`);
    if (avail === 0n) return null;

    const len = Number(avail);
    const out = Buffer.allocUnsafe(len);
    const pos = Number(r & this.mask);
    const first = Number(this.cap) - pos;
    if (first >= len) {
      readAllSync(this.fd, out, 0, len, RING_HEADER_BYTES + pos);
    } else {
      readAllSync(this.fd, out, 0, first, RING_HEADER_BYTES + pos);
      readAllSync(this.fd, out, first, len - first, RING_HEADER_BYTES);
    }
    this.cursorBuf.writeBigUInt64LE(w, 0);
    writeAllSync(this.fd, this.cursorBuf, 0, 8, OFFSET_READ);
    return new Uint8Array(out.buffer, out.byteOffset, out.byteLength);
  }

  close(): void {
    if (this._closed) return;
    this._closed = true;
    fs.closeSync(this.fd);
    this.fd = -1;
  }
}

function writeAllSync(fd: number, buf: Uint8Array, offset: number, length: number, position: number | null): void {
  let written = 0;
  while (written < length) {
    const pos = position === null ? null : position + written;
    const n = fs.writeSync(fd, buf, offset + written, length - written, pos);
    if (n <= 0) throw new Error("ring: short write");
    written += n;
  }
}

function readAllSync(fd: number, buf: Uint8Array, offset: number, length: number, position: number): void {
  let read = 0;
  while (read < length) {
    const n = fs.readSync(fd, buf, offset + read, length - read, position + read);
    if (n <= 0) throw new Error("ring: short read");
    read += n;
  }
}

export interface StreamSink {
  write(chunk: Uint8Array): void;
  close?: () => void;
}

/**
 * Thread-safe / re-entrant KVRING02 frame writer supporting both KVRING02 shared-memory
 * rings and append-stream fallbacks (for FIFOs, pipes, or custom buffers).
 */
export class RingWriter {
  private readonly shm?: Ring;
  private readonly stream?: StreamSink;
  private _dropped = 0n;
  private _closed = false;

  private constructor(shm?: Ring, stream?: StreamSink) {
    this.shm = shm;
    this.stream = stream;
  }

  static fromRing(ring: Ring, meta: OpenMeta): RingWriter {
    const w = new RingWriter(ring, undefined);
    if (ring.writeCursor() === 0n) {
      const m: OpenMeta = {
        ...meta,
        ring: meta.ring || Number(ring.capacity()),
        ring_path: meta.ring_path || ring.path(),
      };
      w.writeOpen(m);
    }
    return w;
  }

  static fromStream(sink: StreamSink, meta?: OpenMeta): RingWriter {
    const w = new RingWriter(undefined, sink);
    if (meta && meta.service) {
      w.writeOpen(meta);
    }
    return w;
  }

  ring(): Ring | undefined {
    return this.shm;
  }

  dropped(): bigint {
    const shmDrop = this.shm ? this.shm.dropped() : 0n;
    return shmDrop > this._dropped ? shmDrop : this._dropped;
  }

  writeOpen(meta: OpenMeta): void {
    if (this._closed) return;
    this.publish(appendOpenFrame(meta));
  }

  writeRecord(r: RingRecord): void {
    if (this._closed) return;
    this.publish(appendRecordFrame(r));
  }

  /**
   * Atomically encodes and publishes a complete step (Input + interior records + optional Marker + StepEnd)
   * in one contiguous publish so frames never interleave or trigger nested-input errors.
   */
  writeStep(recs: RingRecord[], stepEnd = true): void {
    if (this._closed) return;
    const chunks: Uint8Array[] = [];
    for (const r of recs) {
      chunks.push(appendRecordFrame(r));
    }
    if (stepEnd) {
      chunks.push(appendStepEndFrame());
    }
    if (chunks.length === 0) return;
    let total = 0;
    for (const c of chunks) total += c.length;
    const merged = new Uint8Array(total);
    let off = 0;
    for (const c of chunks) {
      merged.set(c, off);
      off += c.length;
    }
    this.publish(merged);
  }

  writeRaw(p: Uint8Array): void {
    if (this._closed || p.length === 0) return;
    this.publish(p);
  }

  writeFacts(facts: RingFact[]): void {
    if (this._closed) return;
    this.publish(appendFactsFrame(facts));
  }

  writeSnapshot(state: Uint8Array): void {
    if (this._closed) return;
    this.publish(appendSnapshotFrame(state));
  }

  writeStepEnd(): void {
    if (this._closed) return;
    this.publish(appendStepEndFrame());
  }

  writeFlush(durable: boolean): void {
    if (this._closed) return;
    this.publish(appendFlushFrame(durable));
  }

  close(): void {
    if (this._closed) return;
    try {
      this.publish(appendCloseFrame());
    } catch {
      // best effort on close
    }
    this._closed = true;
    if (this.shm) {
      this.shm.close();
    }
    if (this.stream?.close) {
      this.stream.close();
    }
  }

  private publish(p: Uint8Array): void {
    if (p.length === 0) return;
    if (this.shm) {
      const res = this.shm.tryPublish(p);
      if (!res.ok) this._dropped++;
      return;
    }
    if (this.stream) {
      this.stream.write(p);
    }
  }
}

/**
 * Opens a RingWriter for `filePath`: attempts a KVRING02 shared-memory ring first
 * (compatible with `kavach dev` and C++23 `ShmRingConsumer`), and transparently falls
 * back to an append stream file if ring open/create is unavailable (e.g. FIFO/pipe).
 */
export function openRingPath(filePath: string, capacity: number, meta: OpenMeta): RingWriter {
  try {
    const r = Ring.openOrCreate(filePath, capacity || RING_DEFAULT_CAPACITY);
    return RingWriter.fromRing(r, meta);
  } catch {
    const dir = path.dirname(filePath);
    if (dir && dir !== ".") {
      fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
    }
    const fd = fs.openSync(filePath, fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_APPEND, 0o600);
    const sink: StreamSink = {
      write(chunk: Uint8Array) {
        writeAllSync(fd, chunk, 0, chunk.length, null);
      },
      close() {
        fs.closeSync(fd);
      },
    };
    return RingWriter.fromStream(sink, meta);
  }
}
