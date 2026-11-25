import fs from "node:fs";
import readline from "node:readline";
import type { Readable, Writable } from "node:stream";
import { VERSION } from "./config.js";
import type { InvariantOutput } from "./invariants.js";
import { FaultError, detectFaultCode } from "./matcher.js";

export const HOST_PROTOCOL_VERSION = 1;

export class KavachAbort extends Error {
  readonly detail: string;

  constructor(detail = "") {
    super(detail || "kavach: step aborted");
    this.name = "KavachAbort";
    this.detail = detail;
  }
}

export class KavachPanic extends Error {
  readonly stackTrace: string;

  constructor(message: string, stackTrace?: string) {
    super(message);
    this.name = "KavachPanic";
    this.stackTrace = stackTrace ?? this.stack ?? "";
  }
}

class HostFatal extends Error {
  constructor(message: string) {
    super(message);
    this.name = "HostFatal";
  }
}

export interface HostStepInput {
  seq: bigint;
  source: string;
  position: string;
  data: Uint8Array;
}

export interface HostStepOutcome {
  err?: Error;
  panicMessage?: string;
  panicStack?: string;
  invariantName?: string;
  invariantDetail?: string;
}

export interface HostServeOptions {
  sdkVersion?: string;
  invariantNames?: () => string[];
  onInit?: (service: string, start: string, snapshot?: Uint8Array) => void | Promise<void>;
  runStep: (input: HostStepInput, env: HostEnv) => HostStepOutcome | Promise<HostStepOutcome>;
  stdin?: Readable;
  stdout?: Writable;
  /** Disable redirecting process.stdout/console.log to stderr (e.g. in unit tests using custom streams). */
  skipProtectStdout?: boolean;
}

type WireMsg = Record<string, unknown>;

interface HostTransport {
  readonly supportsSync: boolean;
  send(msg: WireMsg): void;
  readLineSync(): string | null;
  readLineAsync(): Promise<string | null>;
  close?(): void;
}

/**
 * Synchronous fd 0/1 JSON-line channel matching SPEC §9.1.
 * Enables synchronous `kv.now()` / `kv.rand()` AND async gateway/handler calls
 * inside a single Node.js process with zero event-loop stalls.
 */
class FdHostTransport implements HostTransport {
  readonly supportsSync = true;
  private buf: Buffer = Buffer.alloc(0);
  private eof = false;
  private readonly inFd = 0;
  private readonly outFd = 1;
  private readonly sleepArr = new Int32Array(new SharedArrayBuffer(4));

  private sleep(ms: number): void {
    Atomics.wait(this.sleepArr, 0, 0, ms);
  }

  send(msg: WireMsg): void {
    const payload = Buffer.from(JSON.stringify(msg) + "\n", "utf8");
    let off = 0;
    while (off < payload.length) {
      try {
        off += fs.writeSync(this.outFd, payload, off, payload.length - off);
      } catch (e) {
        if ((e as NodeJS.ErrnoException).code === "EAGAIN") {
          this.sleep(1);
          continue;
        }
        throw new HostFatal(`write failed: ${errMsg(e)}`);
      }
    }
  }

  readLineSync(): string | null {
    let scanned = 0;
    for (;;) {
      const idx = this.buf.indexOf(10, scanned);
      if (idx >= 0) {
        const line = this.buf.subarray(0, idx).toString("utf8");
        this.buf = this.buf.subarray(idx + 1);
        return line;
      }
      scanned = this.buf.length;
      if (this.eof) {
        const line = this.buf.toString("utf8");
        this.buf = Buffer.alloc(0);
        return line.length > 0 ? line : null;
      }
      const chunk = Buffer.allocUnsafe(65536);
      let n = 0;
      try {
        n = fs.readSync(this.inFd, chunk, 0, chunk.length, null);
      } catch (e) {
        const code = (e as NodeJS.ErrnoException).code;
        if (code === "EAGAIN") {
          this.sleep(1);
          continue;
        }
        if (code !== "EOF") throw new HostFatal(`read failed: ${errMsg(e)}`);
      }
      if (n === 0) this.eof = true;
      else this.buf = Buffer.concat([this.buf, chunk.subarray(0, n)]);
    }
  }

  async readLineAsync(): Promise<string | null> {
    return this.readLineSync();
  }
}

/**
 * Async Readable/Writable stream transport (built on `node:readline`) for in-process
 * testing with PassThrough streams without touching fd 0/1.
 */
class StreamHostTransport implements HostTransport {
  readonly supportsSync = false;
  private readonly rl: readline.Interface;
  private readonly out: Writable;
  private readonly queue: Array<string | null> = [];
  private readonly waiters: Array<(line: string | null) => void> = [];
  private ended = false;

  constructor(input: Readable, output: Writable) {
    this.out = output;
    this.rl = readline.createInterface({ input, crlfDelay: Infinity });
    this.rl.on("line", (line) => {
      const waiter = this.waiters.shift();
      if (waiter) waiter(line);
      else this.queue.push(line);
    });
    this.rl.on("close", () => {
      this.ended = true;
      while (this.waiters.length > 0) {
        const waiter = this.waiters.shift();
        waiter?.(null);
      }
    });
  }

  send(msg: WireMsg): void {
    this.out.write(JSON.stringify(msg) + "\n");
  }

  readLineSync(): string | null {
    if (this.queue.length > 0) {
      return this.queue.shift() ?? null;
    }
    throw new HostFatal("synchronous protocol request requires stdio fd transport or async method");
  }

  readLineAsync(): Promise<string | null> {
    if (this.queue.length > 0) {
      return Promise.resolve(this.queue.shift() ?? null);
    }
    if (this.ended) {
      return Promise.resolve(null);
    }
    return new Promise((resolve) => {
      this.waiters.push(resolve);
    });
  }

  close(): void {
    this.rl.close();
  }
}

let stdoutProtected = false;

/**
 * Redirects `console.log`/`info`/`debug` and `process.stdout.write` to `process.stderr`
 * so stray application logs never corrupt the SPEC §9 stdio JSON protocol on fd 1.
 */
export function protectStdout(): void {
  if (stdoutProtected) return;
  stdoutProtected = true;
  const toStderr = (...args: unknown[]) => console.error(...args);
  console.log = toStderr;
  console.info = toStderr;
  console.debug = toStderr;
  const errStream = process.stderr;
  (process.stdout as unknown as { write: unknown }).write = (
    ...args: Parameters<typeof errStream.write>
  ) => errStream.write(...args);
}

/**
 * Per-step host environment communicating with the Kavach replay driver over stdio JSON.
 */
export class HostEnv {
  private readonly transport: HostTransport;
  private _aborted = false;
  private _abortDetail = "";
  private _closed = false;
  private readonly _outputs: InvariantOutput[] = [];
  private readonly askChain: { current: Promise<void> } = { current: Promise.resolve() };

  constructor(transport: HostTransport) {
    this.transport = transport;
  }

  close(): void {
    this._closed = true;
  }

  aborted(): boolean {
    return this._aborted;
  }

  abortDetail(): string {
    return this._abortDetail;
  }

  outputs(): InvariantOutput[] {
    return this._outputs.map((o) => ({ ...o, data: new Uint8Array(o.data) }));
  }

  private parseLine(line: string | null): WireMsg {
    if (line === null) {
      throw new HostFatal("driver closed the session without end");
    }
    try {
      const m: unknown = JSON.parse(line);
      if (m && typeof m === "object" && !Array.isArray(m) && typeof (m as WireMsg).t === "string") {
        return m as WireMsg;
      }
    } catch {
      // fall through
    }
    throw new HostFatal(`malformed message from the driver: ${line.slice(0, 200)}`);
  }

  private validateReply(expect: string, m: WireMsg): WireMsg {
    if (m.t === "abort") {
      this._aborted = true;
      this._abortDetail = typeof m.detail === "string" ? m.detail : "";
      throw new KavachAbort(this._abortDetail);
    }
    if (m.t !== expect) {
      throw new HostFatal(`want ${expect}, got ${JSON.stringify(m.t)}`);
    }
    return m;
  }

  private askSync(msg: WireMsg, expect: string): WireMsg | null {
    if (this._closed) return null;
    if (this._aborted) throw new KavachAbort(this._abortDetail);
    this.transport.send(msg);
    const m = this.parseLine(this.transport.readLineSync());
    return this.validateReply(expect, m);
  }

  private async askAsync(msg: WireMsg, expect: string): Promise<WireMsg | null> {
    if (this._closed) return null;
    if (this._aborted) throw new KavachAbort(this._abortDetail);
    if (this.transport.supportsSync) {
      return this.askSync(msg, expect);
    }
    // Serialize async requests on stream transport so replies match 1:1.
    let result: WireMsg | null = null;
    const prev = this.askChain.current;
    let release!: () => void;
    this.askChain.current = new Promise<void>((r) => {
      release = r;
    });
    await prev;
    try {
      if (this._closed) return null;
      if (this._aborted) throw new KavachAbort(this._abortDetail);
      this.transport.send(msg);
      const m = this.parseLine(await this.transport.readLineAsync());
      result = this.validateReply(expect, m);
    } finally {
      release();
    }
    return result;
  }

  nowNanos(): bigint {
    const m = this.askSync({ t: "clock" }, "clock");
    if (!m) return 0n;
    const raw = String(m.unix_nanos ?? "");
    try {
      return BigInt(raw);
    } catch {
      throw new HostFatal(`bad unix_nanos ${JSON.stringify(raw)}`);
    }
  }

  async nowNanosAsync(): Promise<bigint> {
    const m = await this.askAsync({ t: "clock" }, "clock");
    if (!m) return 0n;
    const raw = String(m.unix_nanos ?? "");
    try {
      return BigInt(raw);
    } catch {
      throw new HostFatal(`bad unix_nanos ${JSON.stringify(raw)}`);
    }
  }

  readRand(n: number): Uint8Array {
    if (n <= 0) return new Uint8Array(0);
    const m = this.askSync({ t: "rand", n }, "rand");
    if (!m) throw new Error("kavach: step already ended");
    const data = fromBase64(m.data);
    if (data.length !== n) {
      throw new HostFatal(`asked for ${n} random bytes, got ${data.length}`);
    }
    return data;
  }

  async readRandAsync(n: number): Promise<Uint8Array> {
    if (n <= 0) return new Uint8Array(0);
    const m = await this.askAsync({ t: "rand", n }, "rand");
    if (!m) throw new Error("kavach: step already ended");
    const data = fromBase64(m.data);
    if (data.length !== n) {
      throw new HostFatal(`asked for ${n} random bytes, got ${data.length}`);
    }
    return data;
  }

  async query(gateway: string, request: Uint8Array, scope = 0): Promise<Uint8Array> {
    const m = await this.askAsync(
      {
        t: "gateway",
        gateway,
        request: toBase64(request),
        scope: scope === 1 ? "local" : "remote",
      },
      "gateway",
    );
    if (!m) throw new Error("kavach: step already ended");
    if (typeof m.error === "string") {
      const code = detectFaultCode(m.error);
      if (code) {
        throw new FaultError({ code, message: m.error, gateway });
      }
      throw new Error(m.error);
    }
    return fromBase64(m.response);
  }

  config(key: string): { value?: Uint8Array; present: boolean } {
    const m = this.askSync({ t: "config", key }, "config");
    if (!m) return { present: false };
    return {
      present: m.present === true,
      value: m.present === true ? fromBase64(m.value) : undefined,
    };
  }

  async configAsync(key: string): Promise<{ value?: Uint8Array; present: boolean }> {
    const m = await this.askAsync({ t: "config", key }, "config");
    if (!m) return { present: false };
    return {
      present: m.present === true,
      value: m.present === true ? fromBase64(m.value) : undefined,
    };
  }

  emit(sink: string, data: Uint8Array, scope = 0): void {
    if (this._closed) return;
    if (this._aborted) throw new KavachAbort(this._abortDetail);
    const copy = new Uint8Array(data);
    this._outputs.push({ sink, data: copy });
    this.transport.send({
      t: "emit",
      sink,
      data: toBase64(copy),
      scope: scope === 1 ? "local" : "remote",
    });
  }
}

/**
 * Runs the deterministic SPEC §9 `kavach-host` stdio JSON protocol loop,
 * 100% wire-compatible with `kavach/replay/driver.go` (including warm `HostPool` `reset`).
 */
export async function serveHostProtocol(opts: HostServeOptions): Promise<void> {
  const useCustomStreams = Boolean(opts.stdin || opts.stdout);
  if (!useCustomStreams && !opts.skipProtectStdout) {
    protectStdout();
  }
  const transport: HostTransport = useCustomStreams
    ? new StreamHostTransport(opts.stdin ?? process.stdin, opts.stdout ?? process.stdout)
    : new FdHostTransport();

  const sdk = opts.sdkVersion || `kavach-ts/${VERSION}`;
  const getNames = (): string[] => opts.invariantNames?.() ?? [];

  const readMsg = async (): Promise<WireMsg | null> => {
    const line = await transport.readLineAsync();
    if (line === null) return null;
    try {
      const m: unknown = JSON.parse(line);
      if (m && typeof m === "object" && !Array.isArray(m) && typeof (m as WireMsg).t === "string") {
        return m as WireMsg;
      }
    } catch {
      // fall through
    }
    throw new HostFatal(`malformed message from the driver: ${line.slice(0, 200)}`);
  };

  try {
    const hello = await readMsg();
    if (hello === null) return;
    if (hello.t !== "hello" || hello.protocol !== HOST_PROTOCOL_VERSION) {
      throw new HostFatal(
        `want hello for protocol ${HOST_PROTOCOL_VERSION}, got ${JSON.stringify(hello.t)} protocol ${String(hello.protocol)}`,
      );
    }
    if (hello.mode === "sandbox") {
      throw new HostFatal("sandbox mode not supported");
    }
    if (opts.onInit) {
      await opts.onInit(
        String(hello.service ?? ""),
        String(hello.start ?? "genesis"),
        typeof hello.snapshot === "string" ? fromBase64(hello.snapshot) : undefined,
      );
    }
    transport.send({
      t: "ready",
      protocol: HOST_PROTOCOL_VERSION,
      sdk,
      invariants: getNames(),
      environment: {
        "host.runtime": {
          value: toBase64(Buffer.from(`node-${process.versions.node}`, "utf8")),
        },
      },
    });

    for (;;) {
      const m = await readMsg();
      if (m === null || m.t === "end") return;
      if (m.t === "reset") {
        if (m.protocol !== undefined && m.protocol !== 0 && m.protocol !== HOST_PROTOCOL_VERSION) {
          throw new HostFatal(`want reset for protocol ${HOST_PROTOCOL_VERSION}, got protocol ${String(m.protocol)}`);
        }
        if (opts.onInit) {
          await opts.onInit(
            String(m.service ?? ""),
            String(m.start ?? "genesis"),
            typeof m.snapshot === "string" ? fromBase64(m.snapshot) : undefined,
          );
        }
        transport.send({
          t: "ready",
          protocol: HOST_PROTOCOL_VERSION,
          sdk,
          invariants: getNames(),
        });
        continue;
      }
      if (m.t === "step") {
        await runHostStep(transport, opts, m);
        continue;
      }
      throw new HostFatal(`unexpected ${JSON.stringify(m.t)} between steps`);
    }
  } catch (e) {
    if (e instanceof HostFatal) {
      try {
        transport.send({ t: "fatal", message: e.message });
      } catch {
        // driver is gone
      }
      throw e;
    }
    throw e;
  } finally {
    transport.close?.();
  }
}

async function runHostStep(transport: HostTransport, opts: HostServeOptions, m: WireMsg): Promise<void> {
  const env = new HostEnv(transport);
  const stepIn: HostStepInput = {
    seq: parseSeq(m.seq),
    source: String(m.source ?? ""),
    position: String(m.position ?? ""),
    data: fromBase64(m.data),
  };

  let outcome: HostStepOutcome = {};
  let thrown: unknown = undefined;
  let didThrow = false;

  try {
    outcome = await opts.runStep(stepIn, env);
  } catch (e) {
    didThrow = true;
    thrown = e;
  }

  env.close();

  if (didThrow && thrown instanceof HostFatal) {
    throw thrown;
  }

  const done: WireMsg = { t: "done", outcome: "ok" };
  if (env.aborted() || (didThrow && thrown instanceof KavachAbort)) {
    done.outcome = "aborted";
  } else if (didThrow) {
    if (thrown instanceof KavachPanic) {
      done.outcome = "panic";
      done.message = thrown.message;
      if (thrown.stackTrace) done.detail = thrown.stackTrace;
    } else if (thrown instanceof Error) {
      done.outcome = "error";
      done.message = thrown.message;
    } else {
      done.outcome = "panic";
      done.message = String(thrown);
    }
  } else if (outcome.invariantName) {
    done.outcome = "invariant";
    done.message = outcome.invariantName;
    if (outcome.invariantDetail) done.detail = outcome.invariantDetail;
  } else if (outcome.panicMessage) {
    done.outcome = "panic";
    done.message = outcome.panicMessage;
    if (outcome.panicStack) done.detail = outcome.panicStack;
  } else if (outcome.err) {
    done.outcome = "error";
    done.message = outcome.err.message;
  }

  transport.send(done);
}

function parseSeq(raw: unknown): bigint {
  if (typeof raw === "bigint") return raw;
  if (typeof raw === "number" && Number.isFinite(raw) && raw >= 0) return BigInt(Math.trunc(raw));
  if (typeof raw === "string" && /^\d+$/.test(raw.trim())) return BigInt(raw.trim());
  return 0n;
}

function toBase64(b: Uint8Array): string {
  return Buffer.from(b.buffer, b.byteOffset, b.byteLength).toString("base64");
}

function fromBase64(s: unknown): Uint8Array {
  if (typeof s !== "string" || s.length === 0) return new Uint8Array(0);
  const buf = Buffer.from(s, "base64");
  return new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength);
}

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
