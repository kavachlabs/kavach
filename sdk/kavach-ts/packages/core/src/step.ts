import crypto from "node:crypto";
import {
  type ClockProvider,
  DeterministicRNG,
  deterministicBytes,
  nanosToDate,
  wallNowNanos,
} from "./clock.js";
import type { Mode } from "./config.js";
import type { HostEnv } from "./host.js";
import type { InvariantOutput, StepSnapshot } from "./invariants.js";
import {
  type FaultError,
  type GatewayCall,
  type StepTable,
  UnmatchedGatewayError,
  canonicalizeGatewayCall,
} from "./matcher.js";
import { scrubString } from "./pii.js";
import { FLAG_CRITICAL, MarkerKind, RecordType, type RingRecord } from "./ring.js";

export const ErrNotReplaying = new Error("kavach: replayGateway called outside replay mode");
export const ErrNoActiveStep = new Error("kavach: no active step in AsyncLocalStorage context");

export interface Input {
  source: string;
  position?: string;
  key?: Uint8Array;
  headers?: Record<string, string>;
  data: Uint8Array;
}

export interface Output {
  sink: string;
  key?: string;
  data: Uint8Array;
  scope?: number;
}

export interface StepView {
  seq(): bigint;
  input(): Input;
  outputs(): Output[];
  error(): Error | undefined;
  attempt(): number;
  assertAtMostOnceEmit(sink: string, jsonPathKey: string): Error | null;
}

export class InvariantError extends Error {
  readonly invariantName: string;
  readonly kind: string;
  readonly detail: string;

  constructor(name: string, kind: string, detail: string) {
    super(detail ? `kavach: invariant ${name} violated: ${detail}` : `kavach: invariant ${name} violated`);
    this.name = "InvariantError";
    this.invariantName = name;
    this.kind = kind;
    this.detail = detail;
  }
}

export interface StepRuntimeHooks {
  readonly mode: Mode;
  readonly pii: "off" | "standard" | "strict";
  readonly piiKey: Uint8Array;
  scrub(data: Uint8Array): Uint8Array;
  matchFault(call: ReturnType<typeof canonicalizeGatewayCall>): FaultError | null;
  checkInvariants(step: StepContext): Promise<InvariantError | null>;
  checkInvariantsSync(step: StepContext): InvariantError | null;
  assertAtMostOnceEmit(sink: string, jsonPathKey: string, snap: StepSnapshot): Error | null;
  writeRecordedStep(records: RingRecord[]): void;
  clearActiveStep(step: StepContext): void;
}

/**
 * Tracks the lifecycle, recorded/replayed clock/rand reads, gateway calls, and
 * emitted outputs of a single Kavach step. Propagated automatically via AsyncLocalStorage.
 */
export class StepContext implements StepView, ClockProvider {
  private readonly hooks: StepRuntimeHooks;
  private readonly _seq: bigint;
  private readonly _attempt: number;
  private readonly _input: Input;
  private readonly _mode: Mode;
  private readonly hostEnv?: HostEnv;
  private readonly _table?: StepTable;
  private readonly clockProv?: ClockProvider;

  private readonly records: RingRecord[] = [];
  private readonly _outputs: Output[] = [];
  private _err?: Error;
  private _panicked = false;
  private _ended = false;
  private _endErr?: Error;
  private _randSynthCounter = 0n;

  constructor(opts: {
    hooks: StepRuntimeHooks;
    seq: bigint;
    attempt: number;
    input: Input;
    hostEnv?: HostEnv;
    table?: StepTable;
    clockProv?: ClockProvider;
  }) {
    this.hooks = opts.hooks;
    this._seq = opts.seq;
    this._attempt = opts.attempt;
    this._input = opts.input;
    this._mode = opts.hooks.mode;
    this.hostEnv = opts.hostEnv;
    this._table = opts.table;
    this.clockProv = opts.clockProv;

    if (this._mode === "record") {
      const data = this.hooks.scrub(cloneBytes(opts.input.data));
      this.records.push({
        type: RecordType.Input,
        source: opts.input.source,
        position: opts.input.position ?? "",
        data,
      });
    }
  }

  seq(): bigint {
    return this._seq;
  }

  attempt(): number {
    return this._attempt;
  }

  input(): Input {
    return this._input;
  }

  mode(): Mode {
    return this._mode;
  }

  ended(): boolean {
    return this._ended;
  }

  error(): Error | undefined {
    return this._err;
  }

  table(): StepTable | undefined {
    return this._table;
  }

  outputs(): Output[] {
    return this._outputs.map((o) => ({
      sink: o.sink,
      key: o.key,
      data: cloneBytes(o.data),
      scope: o.scope,
    }));
  }

  markPanicked(): void {
    this._panicked = true;
    this._ended = true;
    this.hooks.clearActiveStep(this);
  }

  nowNanos(): bigint {
    if (this._ended) return wallNowNanos();
    if (this._mode === "replay") {
      if (this.hostEnv) return this.hostEnv.nowNanos();
      if (this.clockProv) return this.clockProv.nowNanos();
      return 0n;
    }
    const ns = wallNowNanos();
    if (this._mode === "record") {
      this.records.push({
        type: RecordType.Clock,
        unixNanos: ns,
      });
    }
    return ns;
  }

  now(): Date {
    return nanosToDate(this.nowNanos());
  }

  readRand(n: number): Uint8Array {
    if (n <= 0) return new Uint8Array(0);
    if (this._ended) return new Uint8Array(crypto.randomBytes(n));
    if (this._mode === "replay") {
      if (this.hostEnv) return this.hostEnv.readRand(n);
      if (this.clockProv) return this.clockProv.readRand(n);
      this._randSynthCounter++;
      return deterministicBytes(this._seq, this._randSynthCounter, new Uint8Array(n));
    }
    const bytes = new Uint8Array(crypto.randomBytes(n));
    if (this._mode === "record") {
      this.records.push({
        type: RecordType.Rand,
        data: cloneBytes(bytes),
      });
    }
    return bytes;
  }

  rand(): DeterministicRNG {
    return new DeterministicRNG(this);
  }

  recordGateway(call: GatewayCall): void {
    if (this._mode !== "record" || this._ended) return;
    const c = canonicalizeGatewayCall(call);
    const req = this.hooks.scrub(c.request);
    const resp = c.response ? this.hooks.scrub(c.response) : new Uint8Array(0);
    let errStr = c.error;
    if (errStr && this.hooks.pii !== "off") {
      errStr = scrubString(this.hooks.pii, this.hooks.piiKey, "error", errStr);
    }
    this.records.push({
      type: RecordType.Gateway,
      flags: FLAG_CRITICAL,
      gateway: c.gateway,
      request: req,
      response: resp,
      error: errStr,
      scope: c.scope,
    });
  }

  async replayGateway(call: GatewayCall): Promise<Uint8Array> {
    if (this._mode !== "replay") {
      throw ErrNotReplaying;
    }
    const c = canonicalizeGatewayCall(call);

    const faultErr = this.hooks.matchFault(c);
    if (faultErr) {
      throw faultErr;
    }

    if (this._table) {
      const res = this._table.matchGateway(c);
      if (res.error) throw res.error;
      return res.response ?? new Uint8Array(0);
    }

    if (this.hostEnv) {
      return this.hostEnv.query(c.gateway, c.request, c.scope);
    }

    throw new UnmatchedGatewayError({
      driver: c.driver,
      gateway: c.gateway,
      op: c.op,
      normalizedOp: c.normalizedOp,
      keyArgs: c.keyArgs,
      stepSeq: this._seq,
    });
  }

  async doGateway(call: GatewayCall, liveFn: () => Promise<Uint8Array> | Uint8Array): Promise<Uint8Array> {
    if (this._mode === "replay") {
      return this.replayGateway(call);
    }
    try {
      const resp = await liveFn();
      if (this._mode === "record") {
        this.recordGateway({ ...call, response: resp });
      }
      return resp;
    } catch (e) {
      if (this._mode === "record") {
        const msg = e instanceof Error ? e.message : String(e);
        this.recordGateway({ ...call, error: msg });
      }
      throw e;
    }
  }

  recordOutput(out: Output): void {
    if (this._ended) return;
    let data = cloneBytes(out.data);
    let key = out.key ?? "";
    if (this._mode === "record") {
      data = this.hooks.scrub(data);
      if (key && this.hooks.pii !== "off") {
        key = scrubString(this.hooks.pii, this.hooks.piiKey, "key", key);
      }
    }
    const stored: Output = { sink: out.sink, key, data, scope: out.scope ?? 0 };
    this._outputs.push(stored);

    if (this._mode === "record") {
      this.records.push({
        type: RecordType.Output,
        sink: stored.sink,
        data: stored.data,
        scope: stored.scope,
      });
    } else if (this._mode === "replay") {
      if (this._table) {
        this._table.recordOutput({
          sink: stored.sink,
          key: stored.key,
          data: stored.data,
          scope: stored.scope,
        });
      }
      if (this.hostEnv) {
        this.hostEnv.emit(stored.sink, stored.data, stored.scope ?? 0);
      }
    }
  }

  config(
    key: string,
    liveFn?: (key: string) => { value?: Uint8Array; source?: string; present: boolean },
  ): { value?: Uint8Array; present: boolean } {
    if (this._mode === "replay" && this.hostEnv) {
      return this.hostEnv.config(key);
    }
    if (!liveFn) return { present: false };
    const res = liveFn(key);
    if (this._mode === "record" && !this._ended) {
      this.records.push({
        type: RecordType.Config,
        flags: FLAG_CRITICAL,
        key,
        present: res.present,
        value: res.value ? this.hooks.scrub(cloneBytes(res.value)) : new Uint8Array(0),
        source: res.source ?? "",
      });
    }
    return { value: res.value, present: res.present };
  }

  assertAtMostOnceEmit(sink: string, jsonPathKey: string): Error | null {
    return this.hooks.assertAtMostOnceEmit(sink, jsonPathKey, this.snapshot());
  }

  snapshot(): StepSnapshot {
    const outputs: InvariantOutput[] = this._outputs.map((o) => ({
      sink: o.sink,
      key: o.key,
      data: o.data,
    }));
    let baselineOutputs: InvariantOutput[] | undefined;
    if (this._table) {
      baselineOutputs = this._table.recordedOutputs().map((o) => ({
        sink: o.sink,
        key: o.key,
        data: o.data,
      }));
    }
    return {
      seq: this._seq,
      attempt: this._attempt,
      outputs,
      baselineOutputs,
      err: this._err,
      panicked: this._panicked,
    };
  }

  async end(err?: unknown): Promise<Error | undefined> {
    if (this._ended) return this._endErr;
    const normalizedErr = normalizeError(err);
    this._err = normalizedErr;
    this._ended = true;
    this.hooks.clearActiveStep(this);

    const invErr = await this.hooks.checkInvariants(this);
    this.flushRecordedStep(normalizedErr, invErr);
    this._endErr = invErr ?? normalizedErr;
    return this._endErr;
  }

  endSync(err?: unknown): Error | undefined {
    if (this._ended) return this._endErr;
    const normalizedErr = normalizeError(err);
    this._err = normalizedErr;
    this._ended = true;
    this.hooks.clearActiveStep(this);

    const invErr = this.hooks.checkInvariantsSync(this);
    this.flushRecordedStep(normalizedErr, invErr);
    this._endErr = invErr ?? normalizedErr;
    return this._endErr;
  }

  private flushRecordedStep(err?: Error, invErr?: InvariantError | null): void {
    if (this._mode !== "record") return;
    const recs = [...this.records];
    if (this._panicked) {
      recs.push({
        type: RecordType.Marker,
        flags: FLAG_CRITICAL,
        kind: MarkerKind.Panic,
        message: err ? err.message : "panic",
      });
    } else if (err) {
      recs.push({
        type: RecordType.Marker,
        flags: FLAG_CRITICAL,
        kind: MarkerKind.Error,
        message: err.message,
      });
    } else if (invErr) {
      recs.push({
        type: RecordType.Marker,
        flags: FLAG_CRITICAL,
        kind: MarkerKind.Invariant,
        message: invErr.invariantName,
        data: new Uint8Array(Buffer.from(invErr.detail || invErr.message, "utf8")),
      });
    }
    this.hooks.writeRecordedStep(recs);
  }
}

export function normalizeError(err: unknown): Error | undefined {
  if (err === undefined || err === null) return undefined;
  if (err instanceof Error) return err;
  return new Error(String(err));
}

export function cloneBytes(b: Uint8Array | undefined): Uint8Array {
  if (!b || b.length === 0) return new Uint8Array(0);
  return new Uint8Array(b);
}

export function inputFingerprint(input: Input): string {
  const h = crypto.createHash("sha256");
  h.update(Buffer.from(input.source, "utf8"));
  h.update(Buffer.from([0]));
  h.update(Buffer.from(input.position ?? "", "utf8"));
  h.update(Buffer.from([0]));
  h.update(input.data);
  return h.digest().subarray(0, 16).toString("hex");
}
