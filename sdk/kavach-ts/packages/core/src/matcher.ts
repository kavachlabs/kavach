import crypto from "node:crypto";

export const CODE_REDIS_LOADING = "REDIS_LOADING";
export const CODE_PG_40001 = "PG_40001";
export const CODE_ETIMEDOUT = "ETIMEDOUT";

export const FAULT_REDIS_LOADING = "LOADING Redis is loading the dataset in memory";
export const FAULT_PG_40001 = "ERROR: could not serialize access due to concurrent update (SQLSTATE 40001)";
export const FAULT_ETIMEDOUT = "ETIMEDOUT: connection timed out";

export type MatchTier = "strict_seq" | "keyed_template" | "fault_injected" | "unmatched_gateway";

export interface GatewayCall {
  driver?: string;
  gateway?: string;
  op?: string;
  normalizedOp?: string;
  opFingerprint?: string;
  keyArgs?: string[];
  request?: Uint8Array;
  response?: Uint8Array;
  error?: string;
  scope?: number;
}

export interface CanonicalGatewayCall {
  driver: string;
  gateway: string;
  op: string;
  normalizedOp: string;
  opFingerprint: string;
  keyArgs: string[];
  request: Uint8Array;
  response?: Uint8Array;
  error: string;
  scope: number;
}

export interface OutputRecord {
  sink: string;
  key?: string;
  data: Uint8Array;
  scope?: number;
}

export interface TelemetryNote {
  kind: string;
  driver: string;
  gateway: string;
  normalizedOp?: string;
  recordedIdx?: number;
  callIdx?: number;
  detail?: string;
}

export interface FaultRuleInput {
  driver?: string;
  gateway?: string;
  opContains?: string;
  nth?: number;
  errString: string;
  once?: boolean;
}

export class FaultRule {
  readonly driver: string;
  readonly gateway: string;
  readonly opContains: string;
  readonly nth: number;
  readonly errString: string;
  readonly once: boolean;
  private matchesSeen = 0;
  private fired = false;

  constructor(input: FaultRuleInput) {
    this.driver = input.driver ?? "";
    this.gateway = input.gateway ?? "";
    this.opContains = input.opContains ?? "";
    this.nth = input.nth ?? 1;
    this.errString = input.errString;
    this.once = input.once ?? false;
  }

  match(call: GatewayCall): boolean {
    if (!this.errString) return false;
    return this.matchesCanonical(canonicalizeGatewayCall(call));
  }

  matchesCanonical(call: CanonicalGatewayCall): boolean {
    if (this.once && this.fired) return false;
    if (this.driver && this.driver.toLowerCase() !== call.driver.toLowerCase()) return false;
    if (this.gateway && this.gateway.toLowerCase() !== call.gateway.toLowerCase()) return false;
    if (this.opContains) {
      const needle = this.opContains.toLowerCase();
      if (!call.op.toLowerCase().includes(needle) && !call.normalizedOp.toLowerCase().includes(needle)) {
        return false;
      }
    }
    this.matchesSeen++;
    const target = this.nth <= 0 ? 1 : this.nth;
    if (this.matchesSeen !== target) return false;
    this.fired = true;
    return true;
  }

  reset(): void {
    this.matchesSeen = 0;
    this.fired = false;
  }
}

export class FaultError extends Error {
  readonly code: string;
  readonly driver: string;
  readonly gateway: string;

  constructor(opts: { code: string; message?: string; driver?: string; gateway?: string }) {
    const msg = opts.message || expandFault(opts.code);
    super(msg);
    this.name = "FaultError";
    this.code = opts.code;
    this.driver = opts.driver ?? "";
    this.gateway = opts.gateway ?? "";
  }
}

export class UnmatchedGatewayError extends Error {
  readonly driver: string;
  readonly gateway: string;
  readonly op: string;
  readonly normalizedOp: string;
  readonly keyArgs: string[];
  readonly stepSeq: bigint;
  readonly callIndex: number;
  readonly available: string[];

  constructor(opts: {
    driver: string;
    gateway: string;
    op: string;
    normalizedOp: string;
    keyArgs: string[];
    stepSeq: bigint;
    callIndex?: number;
    available?: string[];
  }) {
    const parts: string[] = ["kavach: unmatched gateway call"];
    if (opts.stepSeq > 0n) parts.push(` in step ${opts.stepSeq}`);
    parts.push(` (#${opts.callIndex ?? 0}): driver=${JSON.stringify(opts.driver)} gateway=${JSON.stringify(opts.gateway)} op=${JSON.stringify(opts.normalizedOp)}`);
    if (opts.keyArgs.length > 0) parts.push(` keys=${JSON.stringify(opts.keyArgs)}`);
    if (opts.available && opts.available.length > 0) {
      parts.push(`; remaining recorded gateways: [${opts.available.join(", ")}]`);
    } else {
      parts.push("; no unconsumed recorded gateways remain in this step");
    }
    super(parts.join(""));
    this.name = "UnmatchedGatewayError";
    this.driver = opts.driver;
    this.gateway = opts.gateway;
    this.op = opts.op;
    this.normalizedOp = opts.normalizedOp;
    this.keyArgs = [...opts.keyArgs];
    this.stepSeq = opts.stepSeq;
    this.callIndex = opts.callIndex ?? 0;
    this.available = opts.available ? [...opts.available] : [];
  }
}

export function expandFault(codeOrMessage: string): string {
  switch (codeOrMessage.trim().toUpperCase()) {
    case CODE_REDIS_LOADING:
      return FAULT_REDIS_LOADING;
    case CODE_PG_40001:
      return FAULT_PG_40001;
    case CODE_ETIMEDOUT:
      return FAULT_ETIMEDOUT;
    default:
      return codeOrMessage;
  }
}

export function detectFaultCode(msg: string): string {
  const upper = msg.toUpperCase();
  if (upper.includes("REDIS_LOADING") || upper.includes("LOADING REDIS IS LOADING")) {
    return CODE_REDIS_LOADING;
  }
  if (upper.includes("PG_40001") || upper.includes("SQLSTATE 40001") || upper.includes("40001")) {
    return CODE_PG_40001;
  }
  if (upper.includes("ETIMEDOUT") || upper.includes("TIMED OUT") || upper.includes("CONTEXT DEADLINE EXCEEDED")) {
    return CODE_ETIMEDOUT;
  }
  return "";
}

export function isFault(err: unknown, code: string): boolean {
  if (!err) return false;
  if (err instanceof FaultError && err.code.toLowerCase() === code.toLowerCase()) {
    return true;
  }
  const msg = err instanceof Error ? err.message : String(err);
  return (
    detectFaultCode(msg).toLowerCase() === code.toLowerCase() ||
    msg.toUpperCase().includes(code.toUpperCase())
  );
}

export function normalizeOp(driver: string, op: string): string {
  const trimmed = op.trim();
  if (!trimmed) return "";
  const d = driver.trim().toLowerCase();
  switch (d) {
    case "pg":
    case "postgres":
    case "postgresql":
    case "pgx":
    case "sql":
      return normalizeSQL(trimmed);
    case "redis": {
      const fields = trimmed.split(/\s+/);
      return fields.length > 0 ? fields[0].toUpperCase() : "";
    }
    case "http":
    case "https": {
      const fields = trimmed.split(/\s+/);
      if (fields.length >= 2) {
        const q = fields[1].indexOf("?");
        const pathOnly = q >= 0 ? fields[1].slice(0, q) : fields[1];
        return `${fields[0].toUpperCase()} ${pathOnly}`;
      }
      return trimmed;
    }
    default:
      return trimmed.split(/\s+/).join(" ");
  }
}

/**
 * Normalizes SQL queries matching Go `matcher.NormalizeSQL` byte-for-byte:
 * collapses whitespace, strips line/block comments, replaces string literals,
 * numeric literals, and positional placeholders ($1, $2, ...) with "?".
 */
export function normalizeSQL(sql: string): string {
  let out = "";
  const n = sql.length;
  let inSpace = false;
  const writeSpace = () => {
    if (out.length > 0 && !inSpace) {
      out += " ";
      inSpace = true;
    }
  };
  for (let i = 0; i < n; ) {
    const c = sql[i];
    if (c === "-" && i + 1 < n && sql[i + 1] === "-") {
      while (i < n && sql[i] !== "\n") i++;
      writeSpace();
      continue;
    }
    if (c === "/" && i + 1 < n && sql[i + 1] === "*") {
      i += 2;
      while (i + 1 < n && !(sql[i] === "*" && sql[i + 1] === "/")) i++;
      if (i + 1 < n) i += 2;
      writeSpace();
      continue;
    }
    if (/\s/.test(c)) {
      writeSpace();
      i++;
      continue;
    }
    if (c === "'") {
      i++;
      while (i < n) {
        if (sql[i] === "'") {
          if (i + 1 < n && sql[i + 1] === "'") {
            i += 2;
            continue;
          }
          i++;
          break;
        }
        i++;
      }
      out += "?";
      inSpace = false;
      continue;
    }
    if (c === "$" && i + 1 < n && sql[i + 1] >= "0" && sql[i + 1] <= "9") {
      i++;
      while (i < n && sql[i] >= "0" && sql[i] <= "9") i++;
      out += "?";
      inSpace = false;
      continue;
    }
    if (c >= "0" && c <= "9" && (i === 0 || !isIdentChar(sql[i - 1]))) {
      let j = i;
      while (j < n && sql[j] >= "0" && sql[j] <= "9") j++;
      if (j < n && sql[j] === "." && j + 1 < n && sql[j + 1] >= "0" && sql[j + 1] <= "9") {
        j++;
        while (j < n && sql[j] >= "0" && sql[j] <= "9") j++;
      }
      if (j === n || !isIdentChar(sql[j])) {
        out += "?";
        inSpace = false;
        i = j;
        continue;
      }
    }
    out += c;
    inSpace = false;
    i++;
  }
  return out.trim().replace(/;+$/, "").trim();
}

function isIdentChar(ch: string): boolean {
  const c = ch.charCodeAt(0);
  return (c >= 97 && c <= 122) || (c >= 65 && c <= 90) || (c >= 48 && c <= 57) || c === 95;
}

export function sqlKind(sql: string): string {
  const trimmed = sql.trim();
  if (!trimmed) return "query";
  const first = trimmed.split(/\s+/)[0].toUpperCase();
  switch (first) {
    case "SELECT":
    case "WITH":
    case "SHOW":
    case "EXPLAIN":
    case "VALUES":
      return "select";
    case "INSERT":
      return "insert";
    case "UPDATE":
      return "update";
    case "DELETE":
      return "delete";
    default:
      return "exec";
  }
}

/**
 * Deterministic 24-hex-char fingerprint for (driver, normalizedOp, keyArgs),
 * matching Go `matcher.ComputeFingerprint` byte-for-byte.
 */
export function computeFingerprint(driver: string, normalizedOp: string, keyArgs: readonly string[]): string {
  const h = crypto.createHash("sha256");
  const writePart = (s: string) => {
    const bytes = Buffer.from(s, "utf8");
    const lenBuf = Buffer.allocUnsafe(4);
    lenBuf.writeUInt32BE(bytes.length, 0);
    h.update(lenBuf);
    h.update(bytes);
  };
  writePart(driver.trim().toLowerCase());
  writePart(normalizedOp);
  for (const k of keyArgs) {
    writePart(k);
  }
  return h.digest().subarray(0, 12).toString("hex");
}

export function canonicalizeGatewayCall(g: GatewayCall): CanonicalGatewayCall {
  let driver = (g.driver ?? "").trim().toLowerCase();
  let gateway = g.gateway ?? "";
  let op = g.op ?? "";

  if (!driver && gateway) {
    const idx = gateway.indexOf(":");
    if (idx > 0) {
      driver = gateway.slice(0, idx).trim().toLowerCase();
      if (!op) op = gateway.slice(idx + 1).trim();
    } else {
      driver = gateway.trim().toLowerCase();
    }
  }

  const normalizedOp = g.normalizedOp ? g.normalizedOp.trim() : op ? normalizeOp(driver, op) : "";

  if (!gateway) {
    if (driver && (driver === "pg" || driver === "sql" || driver === "postgres")) {
      gateway = `${driver}:${sqlKind(normalizedOp)}`;
    } else if (driver === "redis" && normalizedOp) {
      gateway = `redis:${normalizedOp.toLowerCase()}`;
    } else if (driver && normalizedOp) {
      gateway = `${driver}:${normalizedOp}`;
    } else {
      gateway = driver;
    }
  }

  const keyArgs = g.keyArgs ? [...g.keyArgs] : [];
  const opFingerprint = g.opFingerprint || computeFingerprint(driver, normalizedOp, keyArgs);

  let request: Uint8Array;
  if (!g.request || g.request.length === 0) {
    const env: Record<string, unknown> = { driver };
    if (normalizedOp) env.normalized_op = normalizedOp;
    if (keyArgs.length > 0) env.key_args = keyArgs;
    if (opFingerprint) env.op_fingerprint = opFingerprint;
    request = new Uint8Array(Buffer.from(JSON.stringify(env), "utf8"));
  } else {
    request = new Uint8Array(g.request);
  }

  return {
    driver,
    gateway,
    op,
    normalizedOp,
    opFingerprint,
    keyArgs,
    request,
    response: g.response ? new Uint8Array(g.response) : undefined,
    error: g.error ?? "",
    scope: g.scope ?? 0,
  };
}

export function parseGatewayRequest(gateway: string, request: Uint8Array): CanonicalGatewayCall {
  const gc: GatewayCall = {
    gateway,
    request: new Uint8Array(request),
  };
  if (request.length > 0 && request[0] === 0x7b /* { */) {
    try {
      const env = JSON.parse(Buffer.from(request).toString("utf8")) as Record<string, unknown>;
      if (typeof env.driver === "string" && env.driver) gc.driver = env.driver;
      if (typeof env.op === "string" && env.op) gc.op = env.op;
      else if (typeof env.sql === "string" && env.sql) gc.op = env.sql;
      else if (typeof env.cmd === "string" && env.cmd) gc.op = env.cmd;
      else if (typeof env.method === "string" && typeof env.url === "string" && env.method && env.url) {
        gc.op = `${env.method} ${env.url}`;
      }
      if (typeof env.normalized_op === "string" && env.normalized_op) gc.normalizedOp = env.normalized_op;
      if (Array.isArray(env.key_args) && env.key_args.length > 0) {
        gc.keyArgs = env.key_args.map((x) => String(x));
      } else if (typeof env.key === "string" && env.key) {
        gc.keyArgs = [env.key];
      }
      if (typeof env.op_fingerprint === "string" && env.op_fingerprint) gc.opFingerprint = env.op_fingerprint;
    } catch {
      // non-JSON request
    }
  }
  return canonicalizeGatewayCall(gc);
}

interface RecordedGatewaySlot {
  call: CanonicalGatewayCall;
  consumed: boolean;
}

export interface MatchGatewayResult {
  response?: Uint8Array;
  error?: Error;
  tier: MatchTier;
}

/**
 * Per-step separate Gateway and Output tables with two-tier Gateway matching
 * (StrictSeq -> KeyedTemplate fallback -> UnmatchedGatewayError) and driver-level
 * fault injection. Mirrors sdk/kavach-go/matcher/matcher.go.
 */
export class StepTable {
  readonly stepSeq: bigint;
  private readonly gateways: RecordedGatewaySlot[];
  private readonly _recordedOutputs: OutputRecord[];
  private readonly _emittedOutputs: OutputRecord[] = [];
  private readonly faults: FaultRule[] = [];
  private seqCursor = 0;
  private callCount = 0;
  private _reorderedCount = 0;
  private readonly _telemetry: TelemetryNote[] = [];

  constructor(stepSeq: bigint, recordedGateways: GatewayCall[] = [], recordedOutputs: OutputRecord[] = []) {
    this.stepSeq = stepSeq;
    this.gateways = recordedGateways.map((g) => ({
      call: canonicalizeGatewayCall(g),
      consumed: false,
    }));
    this._recordedOutputs = recordedOutputs.map((o) => ({
      sink: o.sink,
      key: o.key,
      data: new Uint8Array(o.data),
      scope: o.scope,
    }));
  }

  injectFault(rule: FaultRuleInput | FaultRule): void {
    this.faults.push(rule instanceof FaultRule ? rule : new FaultRule(rule));
  }

  matchGateway(req: GatewayCall): MatchGatewayResult {
    const call = canonicalizeGatewayCall(req);
    const callIdx = this.callCount++;

    // Check injected driver-level faults first.
    for (const rule of this.faults) {
      if (rule.matchesCanonical(call)) {
        const msg = expandFault(rule.errString);
        const code = detectFaultCode(rule.errString) || detectFaultCode(msg);
        this._telemetry.push({
          kind: "fault_injected",
          driver: call.driver,
          gateway: call.gateway,
          normalizedOp: call.normalizedOp,
          callIdx,
          detail: msg,
        });
        return {
          error: new FaultError({ code, message: msg, driver: call.driver, gateway: call.gateway }),
          tier: "fault_injected",
        };
      }
    }

    // Advance StrictSeq cursor past slots already consumed by KeyedTemplate fallback.
    while (this.seqCursor < this.gateways.length && this.gateways[this.seqCursor].consumed) {
      this.seqCursor++;
    }

    // Tier 1: StrictSeq on (driver, opFingerprint) or exact (gateway, request).
    if (this.seqCursor < this.gateways.length) {
      const slot = this.gateways[this.seqCursor];
      if (strictMatch(slot.call, call)) {
        slot.consumed = true;
        this.seqCursor++;
        return {
          response: gatewayResult(slot.call),
          error: gatewayErr(slot.call),
          tier: "strict_seq",
        };
      }
    }

    // Tier 2: KeyedTemplate fallback on (driver, normalizedOp, keyArgs).
    for (let i = 0; i < this.gateways.length; i++) {
      if (this.gateways[i].consumed) continue;
      if (keyedTemplateMatch(this.gateways[i].call, call)) {
        this.gateways[i].consumed = true;
        this._reorderedCount++;
        this._telemetry.push({
          kind: "reordered_gateway",
          driver: call.driver,
          gateway: call.gateway,
          normalizedOp: call.normalizedOp,
          recordedIdx: i,
          callIdx,
          detail: `matched recorded gateway #${i} out of order via KeyedTemplate`,
        });
        return {
          response: gatewayResult(this.gateways[i].call),
          error: gatewayErr(this.gateways[i].call),
          tier: "keyed_template",
        };
      }
    }

    // Tier 3: Hard fail with actionable diagnostics.
    const available: string[] = [];
    for (let i = 0; i < this.gateways.length; i++) {
      if (this.gateways[i].consumed) continue;
      const g = this.gateways[i].call;
      available.push(`#${i}(${g.gateway} ${JSON.stringify(g.normalizedOp)} keys=${JSON.stringify(g.keyArgs)})`);
    }
    return {
      error: new UnmatchedGatewayError({
        driver: call.driver,
        gateway: call.gateway,
        op: call.op,
        normalizedOp: call.normalizedOp,
        keyArgs: call.keyArgs,
        stepSeq: this.stepSeq,
        callIndex: callIdx,
        available,
      }),
      tier: "unmatched_gateway",
    };
  }

  recordOutput(out: OutputRecord): void {
    this._emittedOutputs.push({
      sink: out.sink,
      key: out.key,
      data: new Uint8Array(out.data),
      scope: out.scope,
    });
  }

  emittedOutputs(): OutputRecord[] {
    return this._emittedOutputs.map((o) => ({ ...o, data: new Uint8Array(o.data) }));
  }

  recordedOutputs(): OutputRecord[] {
    return this._recordedOutputs.map((o) => ({ ...o, data: new Uint8Array(o.data) }));
  }

  reorderedCount(): number {
    return this._reorderedCount;
  }

  telemetry(): TelemetryNote[] {
    return this._telemetry.map((t) => ({ ...t }));
  }

  unconsumedGateways(): CanonicalGatewayCall[] {
    return this.gateways.filter((g) => !g.consumed).map((g) => g.call);
  }

  compareOutputs(): Error | null {
    const want = this._recordedOutputs;
    const got = this._emittedOutputs;
    const n = Math.max(want.length, got.length);
    for (let i = 0; i < n; i++) {
      if (i >= got.length) {
        return new Error(`output ${i} missing: recorded sink=${JSON.stringify(want[i].sink)}`);
      }
      if (i >= want.length) {
        return new Error(`output ${i} unexpected: emitted sink=${JSON.stringify(got[i].sink)}`);
      }
      if (want[i].sink !== got[i].sink || !bytesEqual(want[i].data, got[i].data)) {
        return new Error(
          `output ${i} differs: recorded (${want[i].sink} ${Buffer.from(want[i].data).toString("utf8")}), replay (${got[i].sink} ${Buffer.from(got[i].data).toString("utf8")})`,
        );
      }
    }
    return null;
  }

  compareOutputsMultiset(): Error | null {
    const want = this._recordedOutputs;
    const got = this._emittedOutputs;
    if (want.length !== got.length) {
      return new Error(`output count differs: recorded ${want.length}, replay ${got.length}`);
    }
    const matched = new Array<boolean>(got.length).fill(false);
    for (let i = 0; i < want.length; i++) {
      let found = false;
      for (let j = 0; j < got.length; j++) {
        if (!matched[j] && want[i].sink === got[j].sink && bytesEqual(want[i].data, got[j].data)) {
          matched[j] = true;
          found = true;
          break;
        }
      }
      if (!found) {
        return new Error(`output ${i} missing in multiset comparison: recorded (${want[i].sink})`);
      }
    }
    return null;
  }
}

function strictMatch(rec: CanonicalGatewayCall, call: CanonicalGatewayCall): boolean {
  if (!driverCompatible(rec, call)) return false;
  if (rec.opFingerprint && call.opFingerprint && rec.opFingerprint === call.opFingerprint) return true;
  if (rec.gateway === call.gateway && rec.request.length > 0 && bytesEqual(rec.request, call.request)) return true;
  return false;
}

function keyedTemplateMatch(rec: CanonicalGatewayCall, call: CanonicalGatewayCall): boolean {
  if (!driverCompatible(rec, call)) return false;
  if (rec.normalizedOp && call.normalizedOp) {
    if (rec.normalizedOp !== call.normalizedOp) return false;
    return stringArrayEqual(rec.keyArgs, call.keyArgs);
  }
  if (rec.gateway && rec.gateway === call.gateway) {
    return stringArrayEqual(rec.keyArgs, call.keyArgs);
  }
  return false;
}

function driverCompatible(rec: CanonicalGatewayCall, call: CanonicalGatewayCall): boolean {
  if (rec.driver && call.driver) return rec.driver.toLowerCase() === call.driver.toLowerCase();
  return rec.gateway.toLowerCase() === call.gateway.toLowerCase();
}

function gatewayResult(rec: CanonicalGatewayCall): Uint8Array | undefined {
  if (rec.error) return undefined;
  return rec.response ? new Uint8Array(rec.response) : new Uint8Array(0);
}

function gatewayErr(rec: CanonicalGatewayCall): Error | undefined {
  if (!rec.error) return undefined;
  const code = detectFaultCode(rec.error);
  if (code) {
    return new FaultError({ code, message: rec.error, driver: rec.driver, gateway: rec.gateway });
  }
  return new Error(rec.error);
}

function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return false;
  }
  return true;
}

function stringArrayEqual(a: readonly string[], b: readonly string[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return false;
  }
  return true;
}
