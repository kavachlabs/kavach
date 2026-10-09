// The SDK half of the flight recorder pipe (SPEC §3.6, §10).

import { spawn, type ChildProcess } from "node:child_process";
import { randomBytes } from "node:crypto";
import { GatewayError } from "./errors.js";
import { checkInvariants, errorMessage, mapFailure } from "./failure.js";
import type { EmitOptions, Env, Failure, FailureKind, Gateway, Handler, Input, Output } from "./types.js";
import { SDK_NAME } from "./version.js";
import {
  clockFrame,
  closeFrame,
  concat,
  configFrame,
  factsFrame,
  flushFrame,
  gatewayFrame,
  inputFrame,
  markerFrame,
  openFrame,
  outputFrame,
  randFrame,
  snapshotFrame,
  stepEndFrame,
  toBytes,
  u8,
} from "./wire.js";

/** A config value served to `env.config`. */
export interface ConfigValue {
  value: Uint8Array | string;
  /** Where it came from, e.g. "env" or "launchdarkly". Informational. */
  source?: string;
}

export interface RecorderOptions {
  /** Service name (journal header). Required. */
  service: string;
  /** The handler that consumes inputs. */
  handler: Handler;
  /** Build identity of the handler, e.g. a version or VCS revision. */
  handlerVersion?: string;
  /**
   * The handler's starting state. When given, the handler is restored from it
   * and the journal starts with a snapshot (`start: "snapshot"`).
   */
  snapshot?: Uint8Array;
  /** Gateways by name. Names MUST be stable across builds. */
  gateways?: Record<string, Gateway>;
  /**
   * Config provider for `env.config`. Defaults to environment variables
   * (source "env"). Return `undefined` for "not set".
   */
  config?: (key: string) => ConfigValue | Uint8Array | string | undefined;
  /** Feature flags this process could evaluate, as `flag.<name>` keys to values (SPEC §4.8). */
  flags?: () => Record<string, string | Uint8Array>;
  /** Called with a step's outputs, only after the step succeeded. */
  deliver?: (outputs: Output[]) => void | Promise<void>;

  /** The recorder's argv. Else `KAVACH_RECORDER`, else `kavach-recorder` on PATH. */
  recorderCommand?: string[];
  /** Fail `Recorder.start` if the recorder cannot be started (SPEC §10.1). Default false. */
  required?: boolean;
  /** Log sink for recorder problems and fixtures. Default: standard error. */
  log?: (message: string) => void;
  /** How long `flush({durable: true})` waits for the recorder. Default 10000. */
  flushTimeoutMs?: number;
  /** How long `close()` waits for the recorder's `closed`. Default 10000. */
  closeTimeoutMs?: number;
  /** How long a `required` start waits for the recorder's `ready`. Default 5000. */
  startTimeoutMs?: number;

  /** Journal settings sent in `open` (SPEC §10.2). Omitted ones use the recorder's defaults. */
  dir?: string;
  compression?: "zstd" | "none";
  level?: number;
  blockBytes?: number;
  flushMs?: number;
  segmentBytes?: number;
  segmentSeconds?: number;
  retainSegments?: number;
  secretKeys?: string[];

  /** Clock override returning Unix nanoseconds. Default: real time with nanosecond precision. */
  clock?: () => bigint;
  /** Random source override. Must return exactly `n` bytes. Default: crypto.randomBytes. */
  random?: (n: number) => Uint8Array;
}

export interface StepResult {
  outcome: "ok" | FailureKind;
  /** The marker message when the step failed. */
  message?: string;
  /** The marker data (stack trace or invariant detail) when the step failed. */
  detail?: string;
  /** The value the handler threw, if it threw. */
  thrown?: unknown;
  /** Outputs the handler produced (delivered only if `outcome` is "ok"). */
  outputs: Output[];
  /** Set if `deliver` failed. */
  deliverError?: unknown;
}

interface StepState {
  frames: (Uint8Array | undefined)[];
  pending: Promise<void>[];
  outputs: Output[];
  done: boolean;
}

/** Real time at nanosecond precision: wall-clock epoch anchored once, advanced by the monotonic clock. */
export function realClock(): () => bigint {
  const anchor = BigInt(Date.now()) * 1_000_000n - process.hrtime.bigint();
  return () => anchor + process.hrtime.bigint();
}

export class Recorder {
  private child: ChildProcess | undefined;
  private recording = false;
  private closed = false;
  private tail: Promise<unknown> = Promise.resolve();
  private snapshotRequested = false;
  private needDrain = false;
  private durableWaiters: ((ok: boolean) => void)[] = [];
  private closedWaiters: (() => void)[] = [];
  private readyWaiters: (() => void)[] = [];
  private ready = false;
  private closedSeen = false;
  private deadWaiters: (() => void)[] = [];
  private readonly clock: () => bigint;
  private readonly rand: (n: number) => Uint8Array;
  private readonly log: (message: string) => void;

  private constructor(private readonly o: RecorderOptions) {
    this.clock = o.clock ?? realClock();
    this.rand = o.random ?? ((n) => u8(randomBytes(n)));
    this.log = o.log ?? ((m) => console.error(`kavach: ${m}`));
  }

  /**
   * Starts the recorder process and sends `open` and `facts` (and the
   * `snapshot` of a snapshot start). If the recorder cannot be started, logs
   * loudly and returns a recorder that runs the handler without recording,
   * unless `required` is set, in which case it rejects.
   */
  static async start(options: RecorderOptions): Promise<Recorder> {
    const r = new Recorder(options);
    await r.launch();
    return r;
  }

  /** True while records are being written to the recorder. */
  get isRecording(): boolean {
    return this.recording;
  }

  private async launch(): Promise<void> {
    const o = this.o;
    const startsFromSnapshot = o.snapshot !== undefined;
    if (startsFromSnapshot) {
      if (!o.handler.restore) throw new Error("kavach: a snapshot start needs a handler with restore()");
      o.handler.restore(o.snapshot!);
    }
    const argv = o.recorderCommand ?? [process.env.KAVACH_RECORDER || "kavach-recorder"];
    try {
      const child = spawn(argv[0]!, argv.slice(1), { stdio: ["pipe", "pipe", "inherit"] });
      await new Promise<void>((resolve, reject) => {
        child.once("spawn", resolve);
        child.once("error", reject);
      });
      this.child = child;
    } catch (e) {
      this.startFailed(`could not start the recorder (${argv.join(" ")}): ${errorMessage(e)}`);
      return;
    }
    const child = this.child!;
    this.recording = true;
    child.on("error", (e) => this.fail(`recorder process error: ${errorMessage(e)}`));
    child.on("exit", (code, signal) => {
      if (!this.closed) this.fail(`recorder exited unexpectedly (${signal ?? `status ${code}`})`);
    });
    child.stdin!.on("error", (e) => this.fail(`writing to the recorder failed: ${errorMessage(e)}`));
    child.stdin!.on("close", () => {
      if (!this.closed) this.fail("the recorder closed its input");
    });
    child.stdout!.setEncoding("utf8");
    let pending = "";
    child.stdout!.on("data", (chunk: string) => {
      pending += chunk;
      let i: number;
      while ((i = pending.indexOf("\n")) >= 0) {
        const line = pending.slice(0, i);
        pending = pending.slice(i + 1);
        if (line.trim()) this.control(line);
      }
    });
    // Let the service exit on its own even if close() is never called; the
    // recorder then sees the pipe end and finishes the journal (SPEC §10.5).
    child.unref();
    (child.stdin as unknown as { unref?: () => void }).unref?.();
    (child.stdout as unknown as { unref?: () => void }).unref?.();

    const open: Record<string, unknown> = {
      protocol: 1,
      service: o.service,
      start: startsFromSnapshot ? "snapshot" : "genesis",
      producer: SDK_NAME,
      snapshots: typeof o.handler.snapshot === "function",
    };
    const settings = {
      handler: o.handlerVersion,
      dir: o.dir,
      compression: o.compression,
      level: o.level,
      block_bytes: o.blockBytes,
      flush_ms: o.flushMs,
      segment_bytes: o.segmentBytes,
      segment_seconds: o.segmentSeconds,
      retain_segments: o.retainSegments,
      secret_keys: o.secretKeys,
    };
    for (const [k, v] of Object.entries(settings)) if (v !== undefined) open[k] = v;
    this.write(openFrame(open));

    const facts = new Map<string, Uint8Array>();
    facts.set("host.runtime", toBytes(`node-${process.versions.node}`));
    try {
      for (const [k, v] of Object.entries(o.flags?.() ?? {})) facts.set(k, toBytes(v));
    } catch (e) {
      this.log(`flag provider failed, flag facts not recorded: ${errorMessage(e)}`);
    }
    this.write(factsFrame(facts));
    if (startsFromSnapshot) this.write(snapshotFrame(o.snapshot!));

    if (o.required) {
      const ok = await this.waitFor(this.readyWaiters, () => this.ready, o.startTimeoutMs ?? 5000);
      if (!ok || !this.recording) {
        this.stopChild();
        throw new Error("kavach: the recorder did not become ready and recording is required");
      }
    }
  }

  private startFailed(reason: string): void {
    if (this.o.required) throw new Error(`kavach: ${reason}; recording is required`);
    this.log(`${reason}. THE SERVICE IS RUNNING WITHOUT RECORDING.`);
  }

  private waitFor(waiters: (() => void)[], done: () => boolean, timeoutMs: number): Promise<boolean> {
    if (done()) return Promise.resolve(true);
    return new Promise<boolean>((resolve) => {
      const timer = setTimeout(() => resolve(false), timeoutMs);
      const wake = () => {
        clearTimeout(timer);
        resolve(done());
      };
      waiters.push(wake);
      this.deadWaiters.push(wake);
    });
  }

  private control(line: string): void {
    let m: Record<string, unknown>;
    try {
      m = JSON.parse(line) as Record<string, unknown>;
    } catch {
      this.log(`unreadable message from the recorder: ${line}`);
      return;
    }
    switch (m.t) {
      case "ready":
        this.ready = true;
        this.flushWaiters(this.readyWaiters);
        break;
      case "snapshot_request":
        this.snapshotRequested = true;
        break;
      case "durable": {
        const ws = this.durableWaiters;
        this.durableWaiters = [];
        for (const w of ws) w(true);
        break;
      }
      case "segment":
        break;
      case "fixture":
        this.log(`wrote fixture ${String(m.file)} for ${String(m.failure)} at input ${String(m.seq)}`);
        break;
      case "error":
        this.log(`recorder error: ${String(m.message)}`);
        if (m.fatal === true) this.fail(`recorder reported a fatal error: ${String(m.message)}`);
        break;
      case "closed":
        this.closedSeen = true;
        this.flushWaiters(this.closedWaiters);
        break;
      default:
        break; // receivers ignore what they do not know
    }
  }

  private flushWaiters(ws: (() => void)[]): void {
    const copy = ws.splice(0);
    for (const w of copy) w();
  }

  /** Stops recording after a recorder failure. Never fails a step. */
  private fail(reason: string): void {
    if (!this.recording) return;
    this.recording = false;
    this.log(`${reason}. RECORDING HAS STOPPED; the service keeps running without a journal.`);
    const ws = this.durableWaiters;
    this.durableWaiters = [];
    for (const w of ws) w(false);
    this.flushWaiters(this.deadWaiters);
    this.flushWaiters(this.readyWaiters);
    this.flushWaiters(this.closedWaiters);
  }

  private stopChild(): void {
    this.recording = false;
    try {
      this.child?.stdin?.destroy();
      this.child?.kill();
    } catch {
      /* already gone */
    }
  }

  private write(buf: Uint8Array): void {
    const stdin = this.child?.stdin;
    if (!this.recording || !stdin) return;
    if (stdin.destroyed || !stdin.writable) {
      this.fail("the recorder pipe is closed");
      return;
    }
    try {
      if (!stdin.write(buf)) this.needDrain = true;
    } catch (e) {
      this.fail(`writing to the recorder failed: ${errorMessage(e)}`);
    }
  }

  /** Applies backpressure: records are never dropped, the step waits for the pipe instead. */
  private async drain(): Promise<void> {
    if (!this.needDrain || !this.recording) return;
    this.needDrain = false;
    const stdin = this.child!.stdin!;
    await new Promise<void>((resolve) => {
      stdin.once("drain", resolve);
      this.deadWaiters.push(resolve);
    });
  }

  private enqueue<T>(fn: () => Promise<T>): Promise<T> {
    const p = this.tail.then(fn);
    this.tail = p.catch(() => undefined);
    return p;
  }

  /**
   * Runs one handler step. The `input` frame is written before the handler is
   * called; the rest of the step is written with its `step_end`. Never throws
   * for a failing handler or a failing recorder: see {@link StepResult}.
   * Calls are serialized, so steps never overlap.
   */
  step(input: Input): Promise<StepResult> {
    return this.enqueue(() => this.runStep(input));
  }

  private async runStep(input: Input): Promise<StepResult> {
    if (this.closed) throw new Error("kavach: the recorder is closed");
    this.answerSnapshotRequest();
    const st: StepState = { frames: [], pending: [], outputs: [], done: false };
    this.write(inputFrame(input.source, input.position, input.data));
    const env = new RecordingEnv(this, st);

    let failure: Failure | undefined;
    let thrown: unknown;
    try {
      await this.o.handler.handle(env, input);
    } catch (e) {
      thrown = e;
      failure = mapFailure(e);
    }
    await Promise.all(st.pending);
    st.done = true;
    failure ??= checkInvariants(this.o.handler);
    if (failure) st.frames.push(markerFrame(failure.kind, failure.message, failure.detail));
    st.frames.push(stepEndFrame());
    this.write(concat(st.frames.filter((f): f is Uint8Array => f !== undefined)));
    await this.drain();

    const result: StepResult = { outcome: failure ? failure.kind : "ok", outputs: st.outputs };
    if (failure) {
      result.message = failure.message;
      result.detail = failure.detail;
      if (thrown !== undefined) result.thrown = thrown;
    } else if (this.o.deliver && st.outputs.length > 0) {
      try {
        await this.o.deliver(st.outputs);
      } catch (e) {
        result.deliverError = e;
        this.log(`delivering outputs failed: ${errorMessage(e)}`);
      }
    }
    return result;
  }

  private answerSnapshotRequest(): void {
    if (!this.snapshotRequested) return;
    this.snapshotRequested = false;
    if (!this.recording) return;
    if (typeof this.o.handler.snapshot !== "function") return;
    try {
      this.write(snapshotFrame(this.o.handler.snapshot()));
    } catch (e) {
      this.log(`taking a snapshot failed, the segment is not rotated: ${errorMessage(e)}`);
    }
  }

  /**
   * Closes the recorder's open block. With `durable`, also waits until the
   * recorder reports it durable. Resolves true when that happened (or, without
   * `durable`, when the frame was written), false if recording is off or the
   * wait timed out.
   */
  flush(options: { durable?: boolean } = {}): Promise<boolean> {
    return this.enqueue(async () => {
      if (!this.recording) return false;
      const durable = options.durable === true;
      const wait = durable ? this.durableWait() : undefined;
      this.write(flushFrame(durable));
      if (!wait) return true;
      const ok = await wait;
      if (!ok) this.log("flush: the recorder did not confirm durability in time");
      return ok;
    });
  }

  private durableWait(): Promise<boolean> {
    return new Promise<boolean>((resolve) => {
      const timer = setTimeout(() => {
        this.durableWaiters = this.durableWaiters.filter((w) => w !== wrapped);
        resolve(false);
      }, this.o.flushTimeoutMs ?? 10000);
      const wrapped = (ok: boolean) => {
        clearTimeout(timer);
        resolve(ok);
      };
      this.durableWaiters.push(wrapped);
    });
  }

  /** Sends `close` and waits (boundedly) for `closed`. Call it when the service shuts down. */
  close(): Promise<void> {
    return this.enqueue(async () => {
      if (this.closed) return;
      this.closed = true;
      if (this.recording) {
        this.write(closeFrame());
        const ok = await this.waitFor(this.closedWaiters, () => !this.recording || this.closedSeen, this.o.closeTimeoutMs ?? 10000);
        if (!ok) this.log("close: the recorder did not answer in time");
      }
      this.recording = false;
      this.child?.stdin?.end();
    });
  }

  // Reads, used by RecordingEnv.

  /** @internal */
  _clock(): bigint {
    return this.clock();
  }
  /** @internal */
  _random(n: number): Uint8Array {
    const b = this.rand(n);
    if (b.length !== n) throw new Error(`kavach: random source returned ${b.length} bytes, wanted ${n}`);
    return u8(b);
  }
  /** @internal */
  _config(key: string): { value: Uint8Array | undefined; source: string } {
    let v: ConfigValue | Uint8Array | string | undefined;
    let defaultSource = "provider";
    if (this.o.config) v = this.o.config(key);
    else {
      defaultSource = "env";
      v = process.env[key];
    }
    if (v === undefined) return { value: undefined, source: defaultSource };
    if (typeof v === "string" || v instanceof Uint8Array) return { value: toBytes(v), source: defaultSource };
    return { value: toBytes(v.value), source: v.source ?? defaultSource };
  }
  /** @internal */
  _gateway(name: string): Gateway | undefined {
    return this.o.gateways?.[name];
  }
  /** @internal */
  _write(buf: Uint8Array): void {
    this.write(buf);
  }
}

class RecordingEnv implements Env {
  constructor(
    private readonly rec: Recorder,
    private readonly st: StepState,
  ) {}

  private live(): void {
    if (this.st.done) throw new Error("kavach: env used after its step ended");
  }

  nowNanos(): bigint {
    this.live();
    const n = this.rec._clock();
    this.st.frames.push(clockFrame(n));
    return n;
  }

  now(): Date {
    return new Date(Number(this.nowNanos() / 1_000_000n));
  }

  random(n: number): Uint8Array {
    this.live();
    if (!Number.isInteger(n) || n < 1) throw new RangeError("kavach: random(n) needs an integer n of at least 1");
    const b = this.rec._random(n);
    this.st.frames.push(randFrame(b));
    return b;
  }

  config(key: string): Uint8Array | undefined {
    this.live();
    const { value, source } = this.rec._config(key);
    this.st.frames.push(configFrame(key, value, source));
    return value;
  }

  emit(sink: string, data: Uint8Array, options: EmitOptions = {}): void {
    this.live();
    const copy = new Uint8Array(data);
    const local = options.local === true;
    this.st.frames.push(outputFrame(sink, copy, local));
    this.st.outputs.push({ sink, data: copy, scope: local ? "local" : "remote" });
  }

  query(gateway: string, request: Uint8Array): Promise<Uint8Array> {
    this.live();
    const gw = this.rec._gateway(gateway);
    if (!gw) return Promise.reject(new Error(`kavach: no gateway registered as "${gateway}"`));
    // The record's place in the step is reserved now, in call order, not when
    // the response arrives (SPEC §4.7).
    const slot = this.st.frames.length;
    this.st.frames.push(undefined);
    const req = new Uint8Array(request);
    const local = gw.scope === "local";
    const result = (async () => {
      let response: Uint8Array;
      try {
        response = toBytes(await gw.call(new Uint8Array(req)));
      } catch (e) {
        const msg = errorMessage(e);
        this.st.frames[slot] = gatewayFrame(gateway, req, new Uint8Array(0), msg, local);
        throw new GatewayError(msg);
      }
      this.st.frames[slot] = gatewayFrame(gateway, req, response, "", local);
      return new Uint8Array(response);
    })();
    this.st.pending.push(result.then(() => undefined, () => undefined));
    return result;
  }
}
