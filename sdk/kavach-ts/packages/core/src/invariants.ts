import fs from "node:fs";

export const KIND_AT_MOST_ONCE_EMIT = "at_most_once_emit";
export const KIND_ANTI_LOBOTOMY = "anti_lobotomy";
export const KIND_NON_NEGATIVE = "non_negative";
export const KIND_CONSERVATION = "conservation";

export interface InvariantRule {
  name: string;
  kind: string;
  sink?: string;
  sinks?: string[];
  key?: string;
  field?: string;
  on_error_emit_count?: number;
  min_ratio_vs_baseline?: number;
  expr?: string;
}

export interface InvariantsFile {
  service: string;
  invariants: InvariantRule[];
}

export interface InvariantOutput {
  sink: string;
  key?: string;
  data: Uint8Array;
}

export interface StepSnapshot {
  seq: bigint;
  attempt: number;
  outputs: InvariantOutput[];
  baselineOutputs?: InvariantOutput[];
  err?: Error;
  panicked?: boolean;
}

export interface Violation {
  name: string;
  kind: string;
  detail: string;
}

export function loadInvariantsFile(filePath: string): InvariantsFile {
  const data = fs.readFileSync(filePath, "utf8");
  return parseInvariantsYAML(data);
}

/**
 * Zero-dependency YAML subset parser for `.kavach/invariants.yaml`, matching
 * Go `sdk/kavach-go/invariants/yaml.go`: supports comments, quoted/unquoted
 * scalars, inline flow lists (`[a, b]`), and indented block sequences.
 */
export function parseInvariantsYAML(input: string | Uint8Array): InvariantsFile {
  const text = typeof input === "string" ? input : Buffer.from(input).toString("utf8");
  const lines = text.replace(/\r\n/g, "\n").split("\n");
  const out: InvariantsFile = { service: "", invariants: [] };

  let inInvariants = false;
  let inSinksList = false;
  let sinksIndent = 0;
  let cur: InvariantRule | null = null;

  const flushRule = () => {
    if (cur) {
      out.invariants.push(cur);
      cur = null;
    }
    inSinksList = false;
  };

  for (let i = 0; i < lines.length; i++) {
    const lineNo = i + 1;
    const line = stripYAMLComment(lines[i]);
    const trimmed = line.trim();
    if (!trimmed || trimmed === "---" || trimmed === "...") continue;
    const indent = countIndent(line);

    if (indent === 0) {
      inSinksList = false;
      if (trimmed.startsWith("invariants:")) {
        flushRule();
        inInvariants = true;
        const rest = trimmed.slice("invariants:".length).trim();
        if (rest === "[]") inInvariants = false;
        continue;
      }
      inInvariants = false;
      const kv = splitYAMLKV(trimmed);
      if (!kv) {
        throw new Error(`invariants.yaml:${lineNo}: expected key: value, got ${JSON.stringify(trimmed)}`);
      }
      if (kv.key === "service") {
        out.service = unquoteYAML(kv.val);
      }
      continue;
    }

    if (!inInvariants) continue;

    if (inSinksList && indent > sinksIndent && trimmed.startsWith("-")) {
      const item = trimmed.slice(1).trim();
      if (cur && item) {
        cur.sinks = cur.sinks ?? [];
        cur.sinks.push(unquoteYAML(item));
      }
      continue;
    }
    inSinksList = false;

    if (trimmed.startsWith("-") && (trimmed.length === 1 || trimmed[1] === " " || trimmed[1] === "\t")) {
      flushRule();
      cur = { name: "", kind: "" };
      const rest = trimmed.slice(1).trim();
      if (rest) {
        const st = applyRuleField(cur, rest, lineNo, indent);
        inSinksList = st.inSinks;
        sinksIndent = st.sinksIndent;
      }
      continue;
    }

    if (!cur) {
      throw new Error(`invariants.yaml:${lineNo}: unexpected field outside invariant entry: ${JSON.stringify(trimmed)}`);
    }
    const st = applyRuleField(cur, trimmed, lineNo, indent);
    inSinksList = st.inSinks;
    sinksIndent = st.sinksIndent;
  }
  flushRule();

  for (let i = 0; i < out.invariants.length; i++) {
    const r = out.invariants[i];
    if (!r.name) throw new Error(`invariants.yaml: invariant #${i + 1} missing required field "name"`);
    if (!r.kind) throw new Error(`invariants.yaml: invariant ${JSON.stringify(r.name)} missing required field "kind"`);
  }
  return out;
}

function applyRuleField(
  r: InvariantRule,
  text: string,
  lineNo: number,
  indent: number,
): { inSinks: boolean; sinksIndent: number } {
  const kv = splitYAMLKV(text);
  if (!kv) {
    throw new Error(`invariants.yaml:${lineNo}: expected key: value, got ${JSON.stringify(text)}`);
  }
  switch (kv.key) {
    case "name":
      r.name = unquoteYAML(kv.val);
      break;
    case "kind":
      r.kind = unquoteYAML(kv.val);
      break;
    case "sink":
      r.sink = unquoteYAML(kv.val);
      break;
    case "sinks": {
      const v = kv.val.trim();
      if (!v) {
        return { inSinks: true, sinksIndent: indent };
      }
      r.sinks = parseInlineList(v, lineNo);
      break;
    }
    case "key":
      r.key = unquoteYAML(kv.val);
      break;
    case "field":
      r.field = unquoteYAML(kv.val);
      break;
    case "expr":
      r.expr = unquoteYAML(kv.val);
      break;
    case "on_error_emit_count": {
      const raw = unquoteYAML(kv.val);
      const n = Number(raw);
      if (!Number.isInteger(n)) {
        throw new Error(`invariants.yaml:${lineNo}: invalid on_error_emit_count ${JSON.stringify(kv.val)}`);
      }
      r.on_error_emit_count = n;
      break;
    }
    case "min_ratio_vs_baseline": {
      const raw = unquoteYAML(kv.val);
      const f = Number(raw);
      if (!Number.isFinite(f)) {
        throw new Error(`invariants.yaml:${lineNo}: invalid min_ratio_vs_baseline ${JSON.stringify(kv.val)}`);
      }
      r.min_ratio_vs_baseline = f;
      break;
    }
  }
  return { inSinks: false, sinksIndent: 0 };
}

function parseInlineList(v: string, lineNo: number): string[] {
  if (!v.startsWith("[") || !v.endsWith("]")) {
    return [unquoteYAML(v)];
  }
  const inner = v.slice(1, -1).trim();
  if (!inner) return [];
  const out: string[] = [];
  let cur = "";
  let quote = "";
  for (let i = 0; i < inner.length; i++) {
    const c = inner[i];
    if (quote) {
      if (c === quote) {
        quote = "";
      } else if (c === "\\" && quote === '"' && i + 1 < inner.length) {
        i++;
        cur += inner[i];
      } else {
        cur += c;
      }
      continue;
    }
    if (c === "'" || c === '"') {
      quote = c;
      continue;
    }
    if (c === ",") {
      const item = cur.trim();
      if (item) out.push(item);
      cur = "";
      continue;
    }
    cur += c;
  }
  if (quote) {
    throw new Error(`invariants.yaml:${lineNo}: unterminated quote in ${JSON.stringify(v)}`);
  }
  const last = cur.trim();
  if (last) out.push(last);
  return out;
}

function splitYAMLKV(s: string): { key: string; val: string } | null {
  let quote = "";
  for (let i = 0; i < s.length; i++) {
    const c = s[i];
    if (quote) {
      if (c === quote) quote = "";
      continue;
    }
    if (c === "'" || c === '"') {
      quote = c;
      continue;
    }
    if (c === ":" && (i + 1 === s.length || s[i + 1] === " " || s[i + 1] === "\t")) {
      return { key: s.slice(0, i).trim(), val: s.slice(i + 1).trim() };
    }
  }
  return null;
}

function stripYAMLComment(line: string): string {
  let quote = "";
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (quote) {
      if (c === quote) quote = "";
      else if (c === "\\" && quote === '"' && i + 1 < line.length) i++;
      continue;
    }
    if (c === "'" || c === '"') {
      quote = c;
      continue;
    }
    if (c === "#" && (i === 0 || line[i - 1] === " " || line[i - 1] === "\t")) {
      return line.slice(0, i);
    }
  }
  return line;
}

function countIndent(line: string): number {
  let n = 0;
  for (let i = 0; i < line.length; i++) {
    if (line[i] === " ") n++;
    else if (line[i] === "\t") n += 2;
    else break;
  }
  return n;
}

function unquoteYAML(s: string): string {
  const t = s.trim();
  if (t.length >= 2) {
    if (t[0] === '"' && t[t.length - 1] === '"') {
      try {
        const parsed: unknown = JSON.parse(t);
        if (typeof parsed === "string") return parsed;
      } catch {
        // fall through
      }
      return t.slice(1, -1);
    }
    if (t[0] === "'" && t[t.length - 1] === "'") {
      return t.slice(1, -1);
    }
  }
  return t;
}

interface PathToken {
  kind: "field" | "index" | "wildcard";
  field?: string;
  index?: number;
}

function parsePathTokens(expr: string): PathToken[] {
  let s = expr.trim();
  if (s.startsWith("$")) s = s.slice(1);
  if (s.startsWith(".")) s = s.slice(1);
  if (!s) return [];
  const tokens: PathToken[] = [];
  for (let i = 0; i < s.length; ) {
    if (s[i] === ".") {
      i++;
      continue;
    }
    if (s[i] === "[") {
      const end = s.indexOf("]", i);
      if (end < 0) break;
      const inside = s.slice(i + 1, end).trim();
      if (inside === "*") {
        tokens.push({ kind: "wildcard" });
      } else if (/^-?\d+$/.test(inside)) {
        tokens.push({ kind: "index", index: Number(inside) });
      } else {
        tokens.push({ kind: "field", field: inside.replace(/^["']|["']$/g, "") });
      }
      i = end + 1;
      continue;
    }
    let j = i;
    while (j < s.length && s[j] !== "." && s[j] !== "[") j++;
    if (j > i) {
      const field = s.slice(i, j);
      if (field === "*") tokens.push({ kind: "wildcard" });
      else tokens.push({ kind: "field", field });
    }
    i = j;
  }
  return tokens;
}

/**
 * Extracts values addressed by a JSONPath expression (`$.transaction_id`, `transaction_id`,
 * `$.nested.amount`, `$.items[0].id`, `$.entries[*].delta`).
 */
export function extractJSONPath(data: Uint8Array, expr: string): { values: unknown[]; ok: boolean } {
  if (data.length === 0) return { values: [], ok: false };
  let root: unknown;
  try {
    root = JSON.parse(Buffer.from(data).toString("utf8"));
  } catch {
    return { values: [], ok: false };
  }
  const tokens = parsePathTokens(expr);
  if (tokens.length === 0) return { values: [root], ok: true };
  let cur: unknown[] = [root];
  for (const tok of tokens) {
    const next: unknown[] = [];
    for (const node of cur) {
      if (tok.kind === "field" && node && typeof node === "object" && !Array.isArray(node)) {
        const rec = node as Record<string, unknown>;
        if (tok.field !== undefined && Object.prototype.hasOwnProperty.call(rec, tok.field)) {
          next.push(rec[tok.field]);
        }
      } else if (tok.kind === "index" && Array.isArray(node) && tok.index !== undefined) {
        if (tok.index >= 0 && tok.index < node.length) {
          next.push(node[tok.index]);
        }
      } else if (tok.kind === "wildcard") {
        if (Array.isArray(node)) {
          next.push(...node);
        } else if (node && typeof node === "object") {
          next.push(...Object.values(node as Record<string, unknown>));
        }
      }
    }
    if (next.length === 0) return { values: [], ok: false };
    cur = next;
  }
  return { values: cur, ok: true };
}

export function extractJSONPathString(data: Uint8Array, expr: string): { value: string; ok: boolean } {
  const res = extractJSONPath(data, expr);
  if (!res.ok || res.values.length === 0) return { value: "", ok: false };
  const v = res.values[0];
  if (v === null || v === undefined) return { value: "", ok: false };
  if (typeof v === "string") return { value: v, ok: true };
  if (typeof v === "number" || typeof v === "boolean") return { value: String(v), ok: true };
  try {
    return { value: JSON.stringify(v), ok: true };
  } catch {
    return { value: String(v), ok: true };
  }
}

export function extractJSONPathNumbers(data: Uint8Array, expr: string): { values: number[]; ok: boolean } {
  const res = extractJSONPath(data, expr);
  if (!res.ok || res.values.length === 0) return { values: [], ok: false };
  const out: number[] = [];
  for (const v of res.values) {
    if (typeof v === "number" && Number.isFinite(v)) {
      out.push(v);
    } else if (typeof v === "string") {
      const f = Number(v.trim());
      if (v.trim() !== "" && Number.isFinite(f)) out.push(f);
    } else if (Array.isArray(v)) {
      for (const elem of v) {
        if (typeof elem === "number" && Number.isFinite(elem)) out.push(elem);
      }
    }
  }
  return out.length > 0 ? { values: out, ok: true } : { values: [], ok: false };
}

export function sinkMatches(outSink: string, ruleSink?: string): boolean {
  if (!ruleSink || ruleSink === "*") return true;
  if (outSink === ruleSink) return true;
  if (!ruleSink.includes(":")) {
    const idx = outSink.indexOf(":");
    if (idx >= 0 && outSink.slice(idx + 1) === ruleSink) return true;
  }
  return false;
}

/**
 * Evaluates declarative `.kavach/invariants.yaml` rules and session-wide
 * assertions across all steps of a run. Mirrors Go `invariants.Evaluator`.
 */
export class InvariantEvaluator {
  private readonly _rules: InvariantRule[];
  private seenByRule = new Map<string, Map<string, bigint>>();
  private emittedBySink = new Map<string, number>();
  private baselineBySink = new Map<string, number>();

  constructor(rules: InvariantRule[] = []) {
    this._rules = rules.map((r) => ({ ...r, sinks: r.sinks ? [...r.sinks] : undefined }));
  }

  reset(): void {
    this.seenByRule.clear();
    this.emittedBySink.clear();
  }

  rules(): InvariantRule[] {
    return this._rules.map((r) => ({ ...r, sinks: r.sinks ? [...r.sinks] : undefined }));
  }

  ruleNames(): string[] {
    return this._rules.map((r) => r.name);
  }

  setBaselineCounts(counts: Record<string, number>): void {
    this.baselineBySink.clear();
    for (const [k, v] of Object.entries(counts)) {
      this.baselineBySink.set(k, v);
    }
  }

  assertAtMostOnceEmit(sink: string, jsonPathKey: string, s: StepSnapshot): Error | null {
    return this.checkAtMostOnce(`code:${sink}:${jsonPathKey}`, sink, jsonPathKey, undefined, s);
  }

  checkStep(s: StepSnapshot): Violation | null {
    for (const out of s.outputs) {
      this.emittedBySink.set(out.sink, (this.emittedBySink.get(out.sink) ?? 0) + 1);
    }
    const failed = Boolean(s.err || s.panicked);

    for (const r of this._rules) {
      switch (r.kind) {
        case KIND_AT_MOST_ONCE_EMIT: {
          const err = this.checkAtMostOnce(`rule:${r.name}`, r.sink ?? "", r.key ?? "", r.on_error_emit_count, s);
          if (err) return { name: r.name, kind: r.kind, detail: err.message };
          break;
        }
        case KIND_ANTI_LOBOTOMY: {
          if (failed) continue;
          const v = this.checkAntiLobotomyStep(r, s);
          if (v) return v;
          break;
        }
        case KIND_NON_NEGATIVE: {
          if (failed) continue;
          const v = checkNonNegative(r, s);
          if (v) return v;
          break;
        }
        case KIND_CONSERVATION: {
          if (failed) continue;
          const v = checkConservation(r, s);
          if (v) return v;
          break;
        }
      }
    }
    return null;
  }

  checkSession(): Violation | null {
    for (const r of this._rules) {
      if (r.kind !== KIND_ANTI_LOBOTOMY) continue;
      const minRatio = r.min_ratio_vs_baseline ?? 1.0;
      const sinks = r.sinks && r.sinks.length > 0 ? r.sinks : r.sink ? [r.sink] : [];
      for (const wantSink of sinks) {
        const baseCount = countMatchingSinks(this.baselineBySink, wantSink);
        if (baseCount <= 0) continue;
        const gotCount = countMatchingSinks(this.emittedBySink, wantSink);
        const ratio = gotCount / baseCount;
        if (ratio + 1e-9 < minRatio) {
          return {
            name: r.name,
            kind: r.kind,
            detail: `sink ${JSON.stringify(wantSink)} emitted ${gotCount} output(s) vs baseline ${baseCount} (ratio ${ratio.toFixed(2)} < min ${minRatio.toFixed(2)})`,
          };
        }
      }
    }
    return null;
  }

  private checkAtMostOnce(
    bucketKey: string,
    sink: string,
    keyExpr: string,
    onErrorLimit: number | undefined,
    s: StepSnapshot,
  ): Error | null {
    let seen = this.seenByRule.get(bucketKey);
    if (!seen) {
      seen = new Map<string, bigint>();
      this.seenByRule.set(bucketKey, seen);
    }

    let matchingCount = 0;
    let dupErr: Error | null = null;
    for (const out of s.outputs) {
      if (!sinkMatches(out.sink, sink)) continue;
      matchingCount++;
      const k = extractOutputKey(out, keyExpr);
      if (!k) continue;
      const prevSeq = seen.get(k);
      if (prevSeq !== undefined) {
        if (!dupErr) {
          dupErr = new Error(
            `duplicate emit to sink ${JSON.stringify(out.sink)} for key ${JSON.stringify(keyExpr)}=${JSON.stringify(k)} (already emitted at step ${prevSeq})`,
          );
        }
      } else {
        seen.set(k, s.seq);
      }
    }

    if ((s.err || s.panicked) && onErrorLimit !== undefined && matchingCount > onErrorLimit) {
      return new Error(
        `emitted ${matchingCount} output(s) to sink ${JSON.stringify(sink)} during failing step ${s.seq} (on_error_emit_count limit is ${onErrorLimit})`,
      );
    }
    return dupErr;
  }

  private checkAntiLobotomyStep(r: InvariantRule, s: StepSnapshot): Violation | null {
    if (!s.baselineOutputs || s.baselineOutputs.length === 0) return null;
    const minRatio = r.min_ratio_vs_baseline ?? 1.0;
    const sinks = r.sinks && r.sinks.length > 0 ? r.sinks : r.sink ? [r.sink] : [];
    for (const wantSink of sinks) {
      let base = 0;
      for (const o of s.baselineOutputs) {
        if (sinkMatches(o.sink, wantSink)) base++;
      }
      if (base === 0) continue;
      let got = 0;
      for (const o of s.outputs) {
        if (sinkMatches(o.sink, wantSink)) got++;
      }
      const ratio = got / base;
      if (ratio + 1e-9 < minRatio) {
        return {
          name: r.name,
          kind: r.kind,
          detail: `step ${s.seq} sink ${JSON.stringify(wantSink)} emitted ${got} output(s) vs baseline ${base} (ratio ${ratio.toFixed(2)} < min ${minRatio.toFixed(2)})`,
        };
      }
    }
    return null;
  }
}

function checkNonNegative(r: InvariantRule, s: StepSnapshot): Violation | null {
  const field = r.field || r.key || "";
  if (!field) return null;
  for (const out of s.outputs) {
    if (!sinkMatches(out.sink, r.sink)) continue;
    const res = extractJSONPathNumbers(out.data, field);
    if (!res.ok) continue;
    for (const v of res.values) {
      if (v < 0) {
        return {
          name: r.name,
          kind: r.kind,
          detail: `sink ${JSON.stringify(out.sink)} field ${JSON.stringify(field)} = ${v} < 0`,
        };
      }
    }
  }
  return null;
}

function checkConservation(r: InvariantRule, s: StepSnapshot): Violation | null {
  const field = r.field || r.key || "";
  if (!field) return null;
  let sum = 0;
  let foundAny = false;
  for (const out of s.outputs) {
    if (!sinkMatches(out.sink, r.sink)) continue;
    const res = extractJSONPathNumbers(out.data, field);
    if (!res.ok) continue;
    for (const v of res.values) {
      sum += v;
      foundAny = true;
    }
  }
  if (foundAny && Math.abs(sum) > 1e-9) {
    return {
      name: r.name,
      kind: r.kind,
      detail: `step ${s.seq} sink ${JSON.stringify(r.sink ?? "")} field ${JSON.stringify(field)} sum = ${sum} != 0`,
    };
  }
  return null;
}

function extractOutputKey(out: InvariantOutput, keyExpr: string): string {
  if (keyExpr) {
    const res = extractJSONPathString(out.data, keyExpr);
    if (res.ok && res.value) return res.value;
  }
  return out.key ?? "";
}

function countMatchingSinks(counts: Map<string, number>, wantSink: string): number {
  let total = 0;
  for (const [s, n] of counts.entries()) {
    if (sinkMatches(s, wantSink)) total += n;
  }
  return total;
}
