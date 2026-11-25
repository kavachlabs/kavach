import { AsyncLocalStorage } from "node:async_hooks";
import http from "node:http";
import { type ClockProvider, DeterministicRNG, nanosToDate, wallNowNanos } from "./clock.js";
import { type KavachConfig, type Mode, type ResolvedConfig, VERSION, resolveConfig } from "./config.js";
import { type HostEnv, type HostStepOutcome, KavachAbort, KavachPanic, serveHostProtocol } from "./host.js";
import {
  InvariantEvaluator,
  type InvariantRule,
  type StepSnapshot,
  loadInvariantsFile,
} from "./invariants.js";
import {
  type CanonicalGatewayCall,
  FaultError,
  FaultRule,
  type FaultRuleInput,
  type GatewayCall,
  type StepTable,
  detectFaultCode,
  expandFault,
} from "./matcher.js";
import { scrubBytes, scrubHeaders, scrubString } from "./pii.js";
import { PROTOCOL_VERSION, type RingRecord, type RingWriter, openRingPath } from "./ring.js";
import {
  ErrNoActiveStep,
  ErrNotReplaying,
  type Input,
  InvariantError,
  type Output,
  StepContext,
  type StepRuntimeHooks,
  type StepView,
  inputFingerprint,
} from "./step.js";

export interface StepRunner {
  runStep(input: Input): void | Promise<void>;
}

export type StepHandlerFn = (input: Input) => void | Promise<void>;

export interface ServerTarget {
  serve(): void | Promise<void>;
}

export interface StarterTarget {
  start(): void | Promise<void>;
}

export type ServeTarget =
  | StepRunner
  | StepHandlerFn
  | http.RequestListener
  | (StepRunner & Partial<ServerTarget> & Partial<StarterTarget>)
  | ServerTarget
  | StarterTarget;

export type CodeInvariantFn = (step: StepView) => void | Error | Promise<void | Error>;

interface CodeInvariant {
  name: string;
  fn: CodeInvariantFn;
}

/**
 * Root Kavach runtime handle returned by `init(config)`.
 * Propagates active steps automatically via Node's `AsyncLocalStorage`.
 */
export class Kavach {
  private readonly cfg: ResolvedConfig;
  private readonly _mode: Mode;
  private readonly als = new AsyncLocalStorage<StepContext>();
  private writer?: RingWriter;
  private _initErr?: Error;
  private _closed = false;
  private stepSeq = 0n;
  private fallbackActiveStep?: StepContext;

  private evaluator: InvariantEvaluator = new InvariantEvaluator();
  private readonly codeInvs: CodeInvariant[] = [];
  private readonly attempts = new Map<string, number>();
  private readonly faultRules: FaultRule[] = [];
  private readonly stepHooks: StepRuntimeHooks;

  constructor(config: KavachConfig = {}) {
    this.cfg = resolveConfig(config);
    this._mode = this.cfg.mode;

    this.stepHooks = {
      mode: this._mode,
      pii: this.cfg.pii,
      piiKey: this.cfg.piiKey,
      scrub: (data) => this.scrub(data),
      matchFault: (call) => this.matchFault(call),
      checkInvariants: (step) => this.checkInvariants(step),
      checkInvariantsSync: (step) => this.checkInvariantsSync(step),
      assertAtMostOnceEmit: (sink, key, snap) => this.evaluator.assertAtMostOnceEmit(sink, key, snap),
      writeRecordedStep: (recs) => {
        if (this.writer) {
          try {
            this.writer.writeStep(recs, true);
          } catch {
            // Production write-path safety: never crash app on ring write error.
          }
        }
      },
      clearActiveStep: (step) => {
        if (this.fallbackActiveStep === step) {
          this.fallbackActiveStep = undefined;
        }
      },
    };

    if (this.cfg.invariantsPath) {
      try {
        this.loadInvariants(this.cfg.invariantsPath);
      } catch (e) {
        this._initErr = e instanceof Error ? e : new Error(String(e));
      }
    }

    if (this._mode === "record" && this.cfg.ringPath) {
      try {
        this.writer = openRingPath(this.cfg.ringPath, this.cfg.ringCapacity, {
          protocol: PROTOCOL_VERSION,
          service: this.cfg.service,
          start: "genesis",
          producer: `kavach-ts/${VERSION}`,
        });
      } catch (e) {
        // Production write-path safety: degrade gracefully, never crash app.
        if (!this._initErr) {
          this._initErr = e instanceof Error ? e : new Error(String(e));
        }
      }
    }
  }

  mode(): Mode {
    return this._mode;
  }

  isOff(): boolean {
    return this._mode === "off";
  }

  isRecord(): boolean {
    return this._mode === "record";
  }

  isReplay(): boolean {
    return this._mode === "replay";
  }

  service(): string {
    return this.cfg.service;
  }

  listenAddr(): string {
    return this.cfg.listenAddr;
  }

  initError(): Error | undefined {
    return this._initErr;
  }

  ringWriter(): RingWriter | undefined {
    return this.writer;
  }

  closeSync(): void {
    if (this._closed) return;
    this._closed = true;
    if (this.writer) {
      this.writer.close();
    }
  }

  async close(): Promise<void> {
    this.closeSync();
  }

  scrub(data: Uint8Array): Uint8Array {
    return scrubBytes(this.cfg.pii, this.cfg.piiKey, data);
  }

  scrubString(keyName: string, val: string): string {
    return scrubString(this.cfg.pii, this.cfg.piiKey, keyName, val);
  }

  scrubHeaders(headers: Record<string, string>): Record<string, string> {
    return scrubHeaders(this.cfg.pii, this.cfg.piiKey, headers);
  }

  injectFault(rule: FaultRuleInput | FaultRule): void {
    this.faultRules.push(rule instanceof FaultRule ? rule : new FaultRule(rule));
  }

  private matchFault(call: CanonicalGatewayCall): FaultError | null {
    for (const rule of this.faultRules) {
      if (rule.matchesCanonical(call)) {
        const msg = expandFault(rule.errString);
        const code = detectFaultCode(rule.errString) || detectFaultCode(msg);
        return new FaultError({
          code,
          message: msg,
          driver: call.driver,
          gateway: call.gateway,
        });
      }
    }
    return null;
  }

  /**
   * Returns the active StepContext from AsyncLocalStorage, falling back to the
   * currently active step if called from a detached callback/timer.
   */
  activeStep(): StepContext | undefined {
    const store = this.als.getStore();
    if (store && !store.ended()) return store;
    if (this.fallbackActiveStep && !this.fallbackActiveStep.ended()) {
      return this.fallbackActiveStep;
    }
    return undefined;
  }

  /**
   * Starts a new step and enters it into AsyncLocalStorage for the current async execution context.
   * For scoped automatic cleanup, prefer `runStep(input, fn)`.
   */
  beginStep(input: Input): StepContext {
    this.stepSeq++;
    const attempt = this.nextAttempt(input);
    const sc = this.createStepContext(this.stepSeq, attempt, input);
    this.als.enterWith(sc);
    return sc;
  }

  /** Finalizes the currently active step. */
  async endStep(err?: unknown): Promise<Error | undefined> {
    const sc = this.activeStep();
    if (!sc) return err instanceof Error ? err : err ? new Error(String(err)) : undefined;
    return sc.end(err);
  }

  endStepSync(err?: unknown): Error | undefined {
    const sc = this.activeStep();
    if (!sc) return err instanceof Error ? err : err ? new Error(String(err)) : undefined;
    return sc.endSync(err);
  }

  /**
   * Runs `fn` inside a scoped `AsyncLocalStorage` step, automatically calling
   * `endStep` on return or throw and propagating any handler/invariant error.
   */
  async runStep<T>(input: Input, fn: (step: StepContext) => T | Promise<T>): Promise<T> {
    this.stepSeq++;
    const attempt = this.nextAttempt(input);
    const sc = this.createStepContext(this.stepSeq, attempt, input);
    return this.als.run(sc, async () => {
      let runErr: unknown = undefined;
      let hasRunErr = false;
      let result!: T;
      try {
        result = await fn(sc);
      } catch (e) {
        hasRunErr = true;
        runErr = e;
      }
      const endErr = await sc.end(hasRunErr ? runErr : undefined);
      if (endErr) throw endErr;
      if (hasRunErr) throw runErr;
      return result;
    });
  }

  /**
   * Executes one step in-process against a pre-populated `StepTable` (and optional `ClockProvider`)
   * without spawning a subprocess. Ideal for adapter tests and L1 reordered-gateway/fault tests.
   */
  async runStepWithTable<T>(
    input: Input,
    table: StepTable,
    fn: (step: StepContext) => T | Promise<T>,
    clockProv?: ClockProvider,
  ): Promise<{ step: StepContext; result?: T; error?: Error }> {
    this.stepSeq++;
    const attempt = this.nextAttempt(input);
    const sc = this.createStepContext(this.stepSeq, attempt, input, undefined, table, clockProv);
    return this.als.run(sc, async () => {
      let runErr: unknown = undefined;
      let hasRunErr = false;
      let result: T | undefined;
      try {
        result = await fn(sc);
      } catch (e) {
        hasRunErr = true;
        runErr = e;
      }
      const endErr = await sc.end(hasRunErr ? runErr : undefined);
      return { step: sc, result, error: endErr };
    });
  }

  recordGateway(call: GatewayCall): void {
    const sc = this.activeStep();
    if (sc) sc.recordGateway(call);
  }

  async replayGateway(call: GatewayCall): Promise<Uint8Array> {
    if (this._mode !== "replay") {
      throw ErrNotReplaying;
    }
    const sc = this.activeStep();
    if (!sc) {
      throw ErrNoActiveStep;
    }
    return sc.replayGateway(call);
  }

  async doGateway(call: GatewayCall, liveFn: () => Promise<Uint8Array> | Uint8Array): Promise<Uint8Array> {
    if (this._mode === "off") {
      return liveFn();
    }
    const sc = this.activeStep();
    if (sc) {
      return sc.doGateway(call, liveFn);
    }
    if (this._mode === "replay") {
      throw ErrNoActiveStep;
    }
    return liveFn();
  }

  recordOutput(out: Output): void {
    const sc = this.activeStep();
    if (sc) sc.recordOutput(out);
  }

  nowNanos(): bigint {
    const sc = this.activeStep();
    if (sc) return sc.nowNanos();
    return wallNowNanos();
  }

  now(): Date {
    const sc = this.activeStep();
    if (sc) return sc.now();
    return nanosToDate(wallNowNanos());
  }

  rand(): DeterministicRNG {
    const sc = this.activeStep();
    if (sc) return sc.rand();
    return new DeterministicRNG();
  }

  config(
    key: string,
    liveFn?: (key: string) => { value?: Uint8Array; source?: string; present: boolean },
  ): { value?: Uint8Array; present: boolean } {
    const sc = this.activeStep();
    if (sc) return sc.config(key, liveFn);
    if (!liveFn) return { present: false };
    const res = liveFn(key);
    return { value: res.value, present: res.present };
  }

  invariant(name: string, fn: CodeInvariantFn): void {
    if (!name || !fn) return;
    this.codeInvs.push({ name, fn });
  }

  loadInvariants(filePath: string): void {
    const parsed = loadInvariantsFile(filePath);
    const all = [...this.evaluator.rules(), ...parsed.invariants];
    this.evaluator = new InvariantEvaluator(all);
  }

  addInvariantRule(rule: InvariantRule): void {
    const all = [...this.evaluator.rules(), rule];
    this.evaluator = new InvariantEvaluator(all);
  }

  invariantNames(): string[] {
    const names = this.evaluator.ruleNames();
    for (const c of this.codeInvs) names.push(c.name);
    return names;
  }

  evaluatorInstance(): InvariantEvaluator {
    return this.evaluator;
  }

  /**
   * Runs `target` normally in `off`/`record` mode, or runs the deterministic SPEC §9
   * `kavach-host` stdio JSON loop in `replay` mode.
   */
  async serveOrReplay(target: ServeTarget): Promise<void> {
    if (!target) {
      throw new Error("kavach: serveOrReplay called with null/undefined target");
    }
    if (this._mode === "replay") {
      if (isStepRunner(target)) {
        return this.serveHost(target);
      }
      if (typeof target === "function") {
        // Distinguish Node http.RequestListener (arity >= 2) vs StepHandlerFn (arity <= 1)
        if (target.length >= 2) {
          throw new Error(
            "kavach: pass HTTP handlers through wrapHandler(kv, handler) from @kavachlabs/http before serveOrReplay",
          );
        }
        return this.serveHost(target as StepHandlerFn);
      }
      throw new Error("kavach: target does not implement StepRunner (runStep) for replay mode");
    }

    if (isServerTarget(target)) {
      await target.serve();
      return;
    }
    if (isStarterTarget(target)) {
      await target.start();
      return;
    }
    if (typeof target === "function" && target.length >= 2) {
      const listener = target as http.RequestListener;
      const { host, port } = parseListenAddr(this.cfg.listenAddr);
      await new Promise<void>((resolve, reject) => {
        const srv = http.createServer(listener);
        srv.on("error", reject);
        srv.listen(port, host || undefined, () => {
          srv.on("close", resolve);
        });
      });
      return;
    }
    throw new Error(`kavach: target cannot be served in ${this._mode} mode (provide serve(), start(), or RequestListener)`);
  }

  /** Runs the deterministic SPEC §9 `kavach-host` stdio JSON protocol loop directly for `runner`. */
  async serveHost(runner: StepRunner | StepHandlerFn): Promise<void> {
    const runFn: StepHandlerFn = typeof runner === "function" ? runner : (input) => runner.runStep(input);
    await serveHostProtocol({
      sdkVersion: `kavach-ts/${VERSION}`,
      invariantNames: () => this.invariantNames(),
      stdin: this.cfg.stdin,
      stdout: this.cfg.stdout,
      onInit: async () => {
        this.resetSession();
        if (this.cfg.onReset) {
          await this.cfg.onReset();
        }
      },
      runStep: async (stepIn, env: HostEnv): Promise<HostStepOutcome> => {
        const input: Input = {
          source: stepIn.source,
          position: stepIn.position,
          data: stepIn.data,
        };
        const seq = stepIn.seq > 0n ? stepIn.seq : ++this.stepSeq;
        const attempt = this.nextAttempt(input);
        const sc = this.createStepContext(seq, attempt, input, env);

        return this.als.run(sc, async (): Promise<HostStepOutcome> => {
          let runErr: unknown = undefined;
          let hasErr = false;
          try {
            await runFn(input);
          } catch (e) {
            if (e instanceof KavachAbort) {
              sc.markPanicked();
              throw e;
            }
            if (e instanceof KavachPanic) {
              sc.markPanicked();
              throw e;
            }
            hasErr = true;
            runErr = e;
          }

          if (env.aborted()) {
            sc.markPanicked();
            throw new KavachAbort(env.abortDetail());
          }

          const endErr = await sc.end(hasErr ? runErr : undefined);
          if (endErr) {
            if (endErr instanceof InvariantError) {
              return {
                err: runErr instanceof Error ? runErr : undefined,
                invariantName: endErr.invariantName,
                invariantDetail: endErr.detail,
              };
            }
            return { err: endErr };
          }
          return {};
        });
      },
    });
  }

  resetSession(): void {
    this.evaluator.reset();
    this.attempts.clear();
    for (const r of this.faultRules) r.reset();
    this.stepSeq = 0n;
    this.fallbackActiveStep = undefined;
  }

  private createStepContext(
    seq: bigint,
    attempt: number,
    input: Input,
    hostEnv?: HostEnv,
    table?: StepTable,
    clockProv?: ClockProvider,
  ): StepContext {
    const sc = new StepContext({
      hooks: this.stepHooks,
      seq,
      attempt,
      input,
      hostEnv,
      table,
      clockProv,
    });
    this.fallbackActiveStep = sc;
    return sc;
  }

  private nextAttempt(input: Input): number {
    const fp = inputFingerprint(input);
    const n = (this.attempts.get(fp) ?? 0) + 1;
    this.attempts.set(fp, n);
    return n;
  }

  private async checkInvariants(sc: StepContext): Promise<InvariantError | null> {
    const snap = sc.snapshot();
    const v = this.evaluator.checkStep(snap);
    if (v) {
      return new InvariantError(v.name, v.kind, v.detail);
    }
    if (snap.panicked) return null;
    for (const inv of this.codeInvs) {
      try {
        const res = await inv.fn(sc);
        if (res instanceof Error && !snap.err) {
          return new InvariantError(inv.name, "code", res.message);
        }
      } catch (e) {
        if (!snap.err) {
          return new InvariantError(inv.name, "code", e instanceof Error ? e.message : String(e));
        }
      }
    }
    return null;
  }

  private checkInvariantsSync(sc: StepContext): InvariantError | null {
    const snap = sc.snapshot();
    const v = this.evaluator.checkStep(snap);
    if (v) {
      return new InvariantError(v.name, v.kind, v.detail);
    }
    if (snap.panicked) return null;
    for (const inv of this.codeInvs) {
      try {
        const res = inv.fn(sc);
        if (res instanceof Error && !snap.err) {
          return new InvariantError(inv.name, "code", res.message);
        }
      } catch (e) {
        if (!snap.err) {
          return new InvariantError(inv.name, "code", e instanceof Error ? e.message : String(e));
        }
      }
    }
    return null;
  }
}

/** Initializes a Kavach v0.2 runtime handle according to `config`. */
export function init(config?: KavachConfig): Kavach {
  return new Kavach(config);
}

function isStepRunner(x: unknown): x is StepRunner {
  return Boolean(x && (typeof x === "object" || typeof x === "function") && typeof (x as StepRunner).runStep === "function");
}

function isServerTarget(x: unknown): x is ServerTarget {
  return Boolean(x && (typeof x === "object" || typeof x === "function") && typeof (x as ServerTarget).serve === "function");
}

function isStarterTarget(x: unknown): x is StarterTarget {
  return Boolean(x && (typeof x === "object" || typeof x === "function") && typeof (x as StarterTarget).start === "function");
}

function parseListenAddr(addr: string): { host: string; port: number } {
  const trimmed = addr.trim();
  const colon = trimmed.lastIndexOf(":");
  if (colon < 0) {
    const p = Number(trimmed);
    return { host: "", port: Number.isFinite(p) ? p : 8080 };
  }
  const host = trimmed.slice(0, colon);
  const port = Number(trimmed.slice(colon + 1));
  return { host, port: Number.isFinite(port) ? port : 8080 };
}
