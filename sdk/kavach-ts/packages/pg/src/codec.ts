import { CODE_PG_40001, type GatewayCall, isFault, normalizeSQL, sqlKind } from "@kavach/core";

export interface TypedCell {
  t: "null" | "bool" | "int64" | "float64" | "string" | "bytes" | "time" | "json";
  v?: unknown;
}

export interface RecordedResultSet {
  columns: string[];
  rows: TypedCell[][];
  command_tag?: string;
  rows_affected?: number;
  error?: string;
}

export interface RecordedExec {
  sql: string;
  normalized_sql: string;
  args?: string[];
  command_tag?: string;
  rows_affected?: number;
  error?: string;
}

export interface PgDatabaseError extends Error {
  code?: string;
  severity?: string;
  detail?: string;
}

const TX_CONTROL_VERBS = new Set(["BEGIN", "START", "COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE", "SET", "RESET"]);

export function sqlClassify(query: string): {
  gateway: string;
  normSQL: string;
  isWrite: boolean;
  hasReturning: boolean;
  isTxControl: boolean;
} {
  const normSQL = normalizeSQL(query);
  const kind = sqlKind(normSQL);
  const firstWord = normSQL.trim().split(/\s+/)[0]?.toUpperCase() ?? "";
  const isTxControl = TX_CONTROL_VERBS.has(firstWord);
  const upper = ` ${normSQL.toUpperCase()} `;
  const hasReturning = upper.includes(" RETURNING ");
  const isWrite = !isTxControl && (kind === "insert" || kind === "update" || kind === "delete" || kind === "exec");
  const gateway = isWrite ? `pg:${kind}` : "pg:select";
  return { gateway, normSQL, isWrite, hasReturning, isTxControl };
}

export function buildPgGatewayCall(query: string, args: string[]): {
  call: GatewayCall;
  normSQL: string;
  isWrite: boolean;
  hasReturning: boolean;
  isTxControl: boolean;
} {
  const cls = sqlClassify(query);
  const call: GatewayCall = {
    driver: "pg",
    gateway: cls.gateway,
    op: query,
    normalizedOp: cls.normSQL,
    keyArgs: args,
  };
  return { call, ...cls };
}

export function formatPgArgs(values?: readonly unknown[]): string[] {
  if (!values || values.length === 0) return [];
  return values.map((v) => formatPgArg(v));
}

function formatPgArg(v: unknown): string {
  if (v === null || v === undefined) return "<nil>";
  if (v instanceof Date) return v.toISOString();
  if (Buffer.isBuffer(v)) return v.toString("utf8");
  if (v instanceof Uint8Array) return Buffer.from(v).toString("utf8");
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "bigint" || typeof v === "boolean") return String(v);
  return JSON.stringify(v);
}

export function encodeCell(v: unknown): TypedCell {
  if (v === null || v === undefined) return { t: "null" };
  if (typeof v === "boolean") return { t: "bool", v };
  if (typeof v === "bigint") return { t: "int64", v: v.toString() };
  if (typeof v === "number") {
    if (Number.isInteger(v)) return { t: "int64", v: String(v) };
    return { t: "float64", v };
  }
  if (typeof v === "string") return { t: "string", v };
  if (v instanceof Date) return { t: "time", v: v.toISOString() };
  if (Buffer.isBuffer(v)) return { t: "bytes", v: v.toString("base64") };
  if (v instanceof Uint8Array) return { t: "bytes", v: Buffer.from(v).toString("base64") };
  return { t: "json", v };
}

export function decodeCell(c: TypedCell): unknown {
  switch (c.t) {
    case "null":
      return null;
    case "bool":
      return typeof c.v === "boolean" ? c.v : String(c.v) === "true";
    case "int64": {
      if (typeof c.v === "number") return c.v;
      const n = Number(c.v);
      return Number.isSafeInteger(n) ? n : String(c.v ?? "0");
    }
    case "float64":
      return typeof c.v === "number" ? c.v : Number(c.v ?? 0);
    case "string":
      return c.v === undefined || c.v === null ? "" : String(c.v);
    case "bytes":
      return typeof c.v === "string" ? Buffer.from(c.v, "base64") : Buffer.alloc(0);
    case "time":
      return typeof c.v === "string" && c.v ? new Date(c.v) : new Date(0);
    case "json":
      return c.v;
    default:
      return c.v ?? null;
  }
}

export function defaultCommand(query: string): string {
  const first = query.trim().split(/\s+/)[0]?.toUpperCase();
  return first || "SELECT";
}

export function defaultCommandTag(query: string, rowCount = 0): string {
  const verb = defaultCommand(query);
  switch (verb) {
    case "INSERT":
      return `INSERT 0 ${rowCount || 1}`;
    case "UPDATE":
      return `UPDATE ${rowCount || 1}`;
    case "DELETE":
      return `DELETE ${rowCount || 1}`;
    case "SELECT":
      return `SELECT ${rowCount}`;
    default:
      return verb;
  }
}

const SQLSTATE_RE = /SQLSTATE\s+([0-9A-Z]{5})/i;

/**
 * Reconstructs a realistic Postgres `DatabaseError` (with `.code = '40001'`, `.severity = 'ERROR'`)
 * from a recorded or injected fault error string (e.g. `PG_40001` / `SQLSTATE 40001`) so
 * application retry logic inspecting `err.code === '40001'` behaves identically to live Postgres.
 */
export function reconstructPgError(err: unknown): Error {
  if (!err) return new Error("unknown pg error");
  if (err instanceof Error && typeof (err as PgDatabaseError).code === "string" && /^[0-9A-Z]{5}$/i.test((err as PgDatabaseError).code!)) {
    return err;
  }
  const msg = err instanceof Error ? err.message : String(err);
  let code = "";
  const m = SQLSTATE_RE.exec(msg);
  if (m && m[1]) {
    code = m[1].toUpperCase();
  } else if (isFault(err, CODE_PG_40001)) {
    code = "40001";
  }
  if (!code) {
    return err instanceof Error ? err : new Error(msg);
  }
  let cleanMsg = msg.replace(/^ERROR:\s*/i, "");
  const idx = cleanMsg.toUpperCase().lastIndexOf(" (SQLSTATE ");
  if (idx > 0) cleanMsg = cleanMsg.slice(0, idx);

  const pgErr = new Error(cleanMsg) as PgDatabaseError;
  pgErr.name = "error";
  pgErr.code = code;
  pgErr.severity = "ERROR";
  return pgErr;
}
