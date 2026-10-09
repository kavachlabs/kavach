// Host protocol (SPEC §9): the service's own handler, driven by `kavach`.

import { spawnSync } from "node:child_process";
import { readSync, writeSync } from "node:fs";
import { GatewayError, KavachAbort } from "./errors.js";
import { checkInvariants, errorMessage, mapFailure } from "./failure.js";
import type { EmitOptions, Env, Failure, Gateway, Handler, Input, Output, Scope } from "./types.js";
import { SDK_NAME } from "./version.js";
import { toBytes, u8 } from "./wire.js";

export interface HostOptions {
  /**
   * Local setup (SPEC §6.3): creates local resources the handler uses. Run in
   * `sandbox` mode before `ready`. Must not reach the network.
   */
  localSetup?: () => void | Promise<void>;
  /** Gateways by name: their `scope` is reported to the driver, and local ones are executed when the driver answers `live`. */
  gateways?: Record<string, Gateway>;
  /** Delivers a successful step's local outputs in `sandbox` mode, as in production. */
  deliver?: (outputs: Output[]) => void | Promise<void>;
  /** argv of `kavach-recorder`, used only for its `facts` command. Else `KAVACH_RECORDER`, else PATH. */
  recorderCommand?: string[];
  /** Override `process.argv` (tests). */
  argv?: string[];
}

/** True when the last argument is `kavach-host`. */
export function isHost(argv: string[] = process.argv): boolean {
  return argv.length > 0 && argv[argv.length - 1] === "kavach-host";
}

/**
 * Call first thing in `main`. If the program was started as a host (last
 * argument `kavach-host`), serves the protocol until `end` and exits the
 * process; the returned promise never resolves. Otherwise returns at once.
 */
export async function maybeHost(
  factory: () => Handler | Promise<Handler>,
  options: HostOptions = {},
): Promise<void> {
  if (!isHost(options.argv)) return;
  const host = new Host(factory, options);
  await host.run();
  process.exit(0);
}

type Msg = Record<string, unknown>;

/** Synchronous line protocol on the original stdin/stdout fds. */
class Channel {
  private buf: Buffer = Buffer.alloc(0);
  private eof = false;
  private readonly out = 1;
  private readonly in = 0;

  private static sleep(ms: number): void {
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
  }

  send(msg: Msg): void {
    const buf = Buffer.from(JSON.stringify(msg) + "\n", "utf8");
    let off = 0;
    while (off < buf.length) {
      try {
        off += writeSync(this.out, buf, off, buf.length - off);
      } catch (e) {
        if ((e as NodeJS.ErrnoException).code === "EAGAIN") {
          Channel.sleep(1);
          continue;
        }
        throw e;
      }
    }
  }

  /** The next line, or null at end of input. */
  readLine(): string | null {
    let scanned = 0;
    for (;;) {
      const i = this.buf.indexOf(10, scanned);
      if (i >= 0) {
        const line = this.buf.subarray(0, i).toString("utf8");
        this.buf = this.buf.subarray(i + 1);
        return line;
      }
      scanned = this.buf.length;
      if (this.eof) {
        const line = this.buf.toString("utf8");
        this.buf = Buffer.alloc(0);
        return line.length ? line : null;
      }
      const chunk = Buffer.allocUnsafe(65536);
      let n = 0;
      try {
        n = readSync(this.in, chunk, 0, chunk.length, null);
      } catch (e) {
        const code = (e as NodeJS.ErrnoException).code;
        if (code === "EAGAIN") {
          Channel.sleep(1);
          continue;
        }
        if (code !== "EOF") throw e;
      }
      if (n === 0) this.eof = true;
      else this.buf = Buffer.concat([this.buf, chunk.subarray(0, n)]);
    }
  }
}

function b64(b: Uint8Array): string {
  return Buffer.from(b).toString("base64");
}

function fromB64(s: unknown): Uint8Array {
  return u8(Buffer.from(typeof s === "string" ? s : "", "base64"));
}

/** `host.runtime` plus, if `kavach-recorder` exists, the facts it prints (SPEC §9.2). */
export function collectEnvironment(recorderCommand?: string[]): Record<string, unknown> {
  const env: Record<string, unknown> = {};
  const argv = recorderCommand ?? [process.env.KAVACH_RECORDER || "kavach-recorder"];
  try {
    const r = spawnSync(argv[0]!, [...argv.slice(1), "facts"], {
      encoding: "utf8",
      timeout: 5000,
      stdio: ["ignore", "pipe", "ignore"],
    });
    if (!r.error && r.status === 0) {
      const parsed: unknown = JSON.parse(r.stdout);
      if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) Object.assign(env, parsed);
    }
  } catch {
    // Not found or unreadable: only host.runtime is sent.
  }
  env["host.runtime"] = { value: b64(Buffer.from(`node-${process.versions.node}`)) };
  return env;
}

class Host {
  private readonly ch = new Channel();
  private handler!: Handler;
  private sandbox = false;

  // Per-step state.
  private aborted = false;
  private abortDetail = "";
  private livePending = false;

  constructor(
    private readonly factory: () => Handler | Promise<Handler>,
    private readonly o: HostOptions,
  ) {}

  private fatal(message: string): never {
    try {
      this.ch.send({ t: "fatal", message });
    } catch {
      /* driver is gone */
    }
    process.exit(1);
  }

  private read(): Msg | null {
    const line = this.ch.readLine();
    if (line === null) return null;
    try {
      const m: unknown = JSON.parse(line);
      if (m && typeof m === "object" && !Array.isArray(m) && typeof (m as Msg).t === "string") return m as Msg;
    } catch {
      /* fall through */
    }
    return this.fatal(`malformed message from the driver: ${line.slice(0, 200)}`);
  }

  async run(): Promise<void> {
    protectStdout();
    const hello = this.read();
    if (!hello) return;
    if (hello.t !== "hello") this.fatal(`expected hello, got ${String(hello.t)}`);
    if (hello.protocol !== 1) this.fatal(`unsupported protocol ${String(hello.protocol)}`);
    this.sandbox = hello.mode === "sandbox";
    try {
      if (this.sandbox && this.o.localSetup) await this.o.localSetup();
      this.handler = await this.factory();
      if (hello.start === "snapshot") {
        if (!this.handler.restore) throw new Error("handler cannot restore a snapshot");
        this.handler.restore(fromB64(hello.snapshot));
      }
    } catch (e) {
      this.fatal(`could not create the handler: ${errorMessage(e)}`);
    }
    this.ch.send({
      t: "ready",
      protocol: 1,
      sdk: SDK_NAME,
      invariants: (this.handler.invariants?.() ?? []).map((i) => i.name),
      environment: collectEnvironment(this.o.recorderCommand),
    });

    for (;;) {
      const m = this.read();
      if (m === null || m.t === "end") return;
      if (m.t !== "step") this.fatal(`unexpected message ${String(m.t)} between steps`);
      await this.step(m);
    }
  }

  private async step(m: Msg): Promise<void> {
    this.aborted = false;
    this.abortDetail = "";
    this.livePending = false;
    const input: Input = {
      source: String(m.source ?? ""),
      position: String(m.position ?? ""),
      data: fromB64(m.data),
    };
    const outputs: Output[] = [];
    const env = new HostEnv(this, outputs);
    let failure: Failure | undefined;
    try {
      await this.handler.handle(env, input);
    } catch (e) {
      if (!this.aborted) failure = mapFailure(e);
    }
    env.done = true;
    // Aborted stays aborted, whatever the handler did with the KavachAbort.
    if (this.aborted) {
      this.ch.send({ t: "done", outcome: "aborted" });
      return;
    }
    failure ??= checkInvariants(this.handler);
    if (!failure && this.sandbox && this.o.deliver) {
      const local = outputs.filter((o) => o.scope === "local");
      if (local.length) {
        try {
          await this.o.deliver(local);
        } catch (e) {
          console.error(`kavach: delivering local outputs failed: ${errorMessage(e)}`);
        }
      }
    }
    if (!failure) {
      this.ch.send({ t: "done", outcome: "ok" });
      return;
    }
    const done: Msg = { t: "done", outcome: failure.kind, message: failure.message };
    if (failure.detail) done.detail = failure.detail;
    this.ch.send(done);
  }

  // Used by HostEnv.

  /** @internal Sends a request and waits for its answer. */
  request(msg: Msg, expect: string): Msg {
    if (this.aborted) throw new KavachAbort(this.abortDetail);
    if (this.livePending) {
      throw new Error("kavach: a live local query is still pending; await it before other reads");
    }
    this.ch.send(msg);
    const r = this.read();
    if (r === null) {
      // The driver went away mid-step.
      process.exit(1);
    }
    if (r.t === "abort") {
      this.aborted = true;
      this.abortDetail = typeof r.detail === "string" ? r.detail : "";
      throw new KavachAbort(this.abortDetail);
    }
    if (r.t !== expect) this.fatal(`expected ${expect}, got ${String(r.t)}`);
    return r;
  }

  /** @internal */
  send(msg: Msg): void {
    if (this.aborted) throw new KavachAbort(this.abortDetail);
    this.ch.send(msg);
  }

  /** @internal */
  gateway(name: string): Gateway | undefined {
    return this.o.gateways?.[name];
  }

  /** @internal */
  setLive(v: boolean): void {
    this.livePending = v;
  }
}

class HostEnv implements Env {
  done = false;
  constructor(
    private readonly host: Host,
    private readonly outputs: Output[],
  ) {}

  private live(): void {
    if (this.done) throw new Error("kavach: env used after its step ended");
  }

  nowNanos(): bigint {
    this.live();
    const r = this.host.request({ t: "clock" }, "clock");
    return BigInt(String(r.unix_nanos));
  }

  now(): Date {
    return new Date(Number(this.nowNanos() / 1_000_000n));
  }

  random(n: number): Uint8Array {
    this.live();
    if (!Number.isInteger(n) || n < 1) throw new RangeError("kavach: random(n) needs an integer n of at least 1");
    const r = this.host.request({ t: "rand", n }, "rand");
    const b = fromB64(r.data);
    if (b.length !== n) throw new Error(`kavach: driver sent ${b.length} random bytes, wanted ${n}`);
    return b;
  }

  config(key: string): Uint8Array | undefined {
    this.live();
    const r = this.host.request({ t: "config", key }, "config");
    return r.present === true ? fromB64(r.value) : undefined;
  }

  emit(sink: string, data: Uint8Array, options: EmitOptions = {}): void {
    this.live();
    const scope: Scope = options.local ? "local" : "remote";
    const copy = new Uint8Array(data);
    this.host.send({ t: "emit", sink, data: b64(copy), scope });
    this.outputs.push({ sink, data: copy, scope });
  }

  query(gateway: string, request: Uint8Array): Promise<Uint8Array> {
    // The exchange happens inside this call, so reads keep call order even
    // under Promise.all; the returned promise is already settled (except for
    // a live local query, which has to run).
    try {
      this.live();
      const gw = this.host.gateway(gateway);
      const scope: Scope = gw?.scope === "local" ? "local" : "remote";
      const req = new Uint8Array(request);
      const r = this.host.request({ t: "gateway", gateway, request: b64(req), scope }, "gateway");
      if (r.live === true) return this.runLive(gateway, gw, req);
      if (typeof r.error === "string") return Promise.reject(new GatewayError(r.error));
      return Promise.resolve(fromB64(r.response));
    } catch (e) {
      const p = Promise.reject(e);
      if (e instanceof KavachAbort) p.catch(() => undefined); // an unobserved abort must not crash the process
      return p;
    }
  }

  private async runLive(name: string, gw: Gateway | undefined, req: Uint8Array): Promise<Uint8Array> {
    this.host.setLive(true);
    try {
      if (!gw) throw new Error(`no local gateway registered as "${name}"`);
      const resp = toBytes(await gw.call(new Uint8Array(req)));
      this.host.setLive(false);
      this.host.send({ t: "observed", response: b64(resp) });
      return new Uint8Array(resp);
    } catch (e) {
      this.host.setLive(false);
      const msg = errorMessage(e);
      this.host.send({ t: "observed", error: msg });
      throw new GatewayError(msg);
    }
  }
}

let stdoutProtected = false;

/**
 * Points the language's standard output at standard error so a handler that
 * prints cannot corrupt the protocol stream, which uses fd 1 directly (SPEC §9.1).
 */
export function protectStdout(): void {
  if (stdoutProtected) return;
  stdoutProtected = true;
  const toStderr = (...args: unknown[]) => console.error(...args);
  console.log = toStderr;
  console.info = toStderr;
  console.debug = toStderr;
  const err = process.stderr;
  (process.stdout as unknown as { write: unknown }).write = (...args: Parameters<typeof err.write>) =>
    err.write(...args);
}
