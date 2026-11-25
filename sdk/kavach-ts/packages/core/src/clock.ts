import crypto from "node:crypto";

export interface ClockProvider {
  nowNanos(): bigint;
  readRand(n: number): Uint8Array;
}

/**
 * Fills `out` deterministically from (stepSeq, counter) via SHA-256,
 * matching Go `clock.DeterministicBytes` and `replay.fillDeterministic` byte-for-byte.
 */
export function deterministicBytes(stepSeq: bigint, counter: bigint, out: Uint8Array): Uint8Array {
  const block = Buffer.alloc(24);
  block.writeBigUInt64LE(stepSeq, 0);
  block.writeBigUInt64LE(counter, 8);
  for (let i = 0; i < out.length; i += 32) {
    block.writeBigUInt64LE(BigInt(i), 16);
    const sum = crypto.createHash("sha256").update(block).digest();
    const len = Math.min(32, out.length - i);
    out.set(sum.subarray(0, len), i);
  }
  return out;
}

/**
 * Deterministic PRNG backed by a step's RecordType::Rand provider (or crypto.randomBytes outside a step).
 */
export class DeterministicRNG {
  private readonly provider: ClockProvider | undefined;

  constructor(provider?: ClockProvider) {
    this.provider = provider;
  }

  bytes(n: number): Uint8Array {
    if (!Number.isInteger(n) || n < 0) {
      throw new RangeError("kavach: rand bytes(n) requires a non-negative integer");
    }
    if (n === 0) return new Uint8Array(0);
    if (this.provider) {
      return this.provider.readRand(n);
    }
    return new Uint8Array(crypto.randomBytes(n));
  }

  read(buf: Uint8Array): number {
    if (buf.length === 0) return 0;
    const data = this.bytes(buf.length);
    buf.set(data);
    return data.length;
  }

  uint64(): bigint {
    const b = this.bytes(8);
    const dv = new DataView(b.buffer, b.byteOffset, 8);
    return dv.getBigUint64(0, true);
  }

  int63(): bigint {
    return this.uint64() & ((1n << 63n) - 1n);
  }

  /** Returns a deterministic integer in [0, max). */
  int(max: number): number {
    if (!Number.isInteger(max) || max <= 0) {
      throw new RangeError("kavach: rand int(max) requires a positive integer");
    }
    return Number(this.uint64() % BigInt(max));
  }

  /** Returns a deterministic float in [0.0, 1.0). */
  float(): number {
    return Number(this.int63() >> 10n) / 9007199254740992; // 2^53
  }

  /** Generates a deterministic RFC 4122 v4 UUID backed by RecordType::Rand. */
  uuidv4(): string {
    const u = this.bytes(16);
    u[6] = (u[6] & 0x0f) | 0x40;
    u[8] = (u[8] & 0x3f) | 0x80;
    const hex = Buffer.from(u).toString("hex");
    return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20, 32)}`;
  }
}

export class ReplayClockProvider implements ClockProvider {
  private readonly stepSeq: bigint;
  private readonly clocks: bigint[];
  private readonly rands: Uint8Array[];
  private lastClock: bigint;
  private readonly lenient: boolean;
  private clockIdx = 0;
  private randIdx = 0;
  private synthesizedCount = 0n;

  constructor(opts: {
    stepSeq?: bigint;
    clocks?: bigint[];
    rands?: Uint8Array[];
    lastClock?: bigint;
    lenient?: boolean;
  }) {
    this.stepSeq = opts.stepSeq ?? 1n;
    this.clocks = opts.clocks ? [...opts.clocks] : [];
    this.rands = opts.rands ? opts.rands.map((b) => new Uint8Array(b)) : [];
    this.lastClock = opts.lastClock ?? 0n;
    this.lenient = opts.lenient ?? true;
  }

  nowNanos(): bigint {
    if (this.clockIdx < this.clocks.length) {
      const ns = this.clocks[this.clockIdx++];
      this.lastClock = ns;
      return ns;
    }
    if (this.lenient) {
      this.synthesizedCount++;
    }
    return this.lastClock;
  }

  readRand(n: number): Uint8Array {
    if (n <= 0) return new Uint8Array(0);
    if (this.randIdx < this.rands.length && this.rands[this.randIdx].length === n) {
      return new Uint8Array(this.rands[this.randIdx++]);
    }
    this.synthesizedCount++;
    return deterministicBytes(this.stepSeq, this.synthesizedCount, new Uint8Array(n));
  }

  synthesized(): bigint {
    return this.synthesizedCount;
  }
}

/** Returns current wall-clock UTC nanoseconds as bigint. */
export function wallNowNanos(): bigint {
  return BigInt(Date.now()) * 1_000_000n;
}

/** Converts UTC Unix nanoseconds to a JavaScript Date. */
export function nanosToDate(nanos: bigint): Date {
  return new Date(Number(nanos / 1_000_000n));
}
