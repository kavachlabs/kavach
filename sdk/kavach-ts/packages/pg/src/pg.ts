import type { Kavach } from "@kavach/core";
import {
  type RecordedExec,
  type RecordedResultSet,
  buildPgGatewayCall,
  decodeCell,
  defaultCommand,
  defaultCommandTag,
  encodeCell,
  formatPgArgs,
  reconstructPgError,
} from "./codec.js";

export interface PgFieldDef {
  name: string;
  tableID?: number;
  columnID?: number;
  dataTypeID?: number;
  dataTypeSize?: number;
  dataTypeModifier?: number;
  format?: string;
}

export interface PgQueryConfig {
  text: string;
  values?: unknown[];
  name?: string;
  rowMode?: "array" | string;
}

export interface PgQueryResult<R = Record<string, unknown>> {
  command: string;
  rowCount: number | null;
  oid: number | null;
  rows: R[];
  fields: PgFieldDef[];
}

export type PgQueryCallback<R = Record<string, unknown>> = (err: Error | null, result?: PgQueryResult<R>) => void;

export interface PgClientLike {
  query<R = Record<string, unknown>>(text: string | PgQueryConfig, values?: unknown[]): Promise<PgQueryResult<R>>;
  query<R = Record<string, unknown>>(text: string | PgQueryConfig, callback: PgQueryCallback<R>): void;
  query<R = Record<string, unknown>>(text: string | PgQueryConfig, values: unknown[], callback: PgQueryCallback<R>): void;
  release?: (err?: Error | boolean) => void;
  end?: () => Promise<void>;
  on?: (event: string, listener: (...args: unknown[]) => void) => unknown;
  once?: (event: string, listener: (...args: unknown[]) => void) => unknown;
  removeListener?: (event: string, listener: (...args: unknown[]) => void) => unknown;
}

export interface PgPoolLike extends PgClientLike {
  connect(): Promise<PgClientLike>;
  end(): Promise<void>;
}

export interface WrappedPgClient<T extends PgClientLike = PgClientLike> extends PgClientLike {
  raw(): T | null;
}

export interface WrappedPgPool<T extends PgPoolLike = PgPoolLike> extends PgPoolLike {
  raw(): T | null;
  connect(): Promise<WrappedPgClient>;
}

function normalizeQueryArgs(
  textOrConfig: string | PgQueryConfig,
  valuesOrCb?: unknown[] | PgQueryCallback,
  maybeCb?: PgQueryCallback,
): { sql: string; values: unknown[]; rowMode?: string; callback?: PgQueryCallback } {
  let sql: string;
  let values: unknown[] = [];
  let rowMode: string | undefined;
  let callback: PgQueryCallback | undefined;

  if (typeof textOrConfig === "string") {
    sql = textOrConfig;
    if (typeof valuesOrCb === "function") {
      callback = valuesOrCb;
    } else {
      if (Array.isArray(valuesOrCb)) values = valuesOrCb;
      if (typeof maybeCb === "function") callback = maybeCb;
    }
  } else {
    sql = textOrConfig.text;
    values = textOrConfig.values ?? [];
    rowMode = textOrConfig.rowMode;
    if (typeof valuesOrCb === "function") callback = valuesOrCb;
    else if (typeof maybeCb === "function") callback = maybeCb;
  }

  return { sql, values, rowMode, callback };
}

async function executeQuery<R>(
  kv: Kavach,
  rawClient: PgClientLike | null,
  textOrConfig: string | PgQueryConfig,
  valuesOrCb?: unknown[] | PgQueryCallback,
): Promise<PgQueryResult<R>> {
  const { sql, values, rowMode } = normalizeQueryArgs(textOrConfig, valuesOrCb);

  if (kv.isOff()) {
    if (!rawClient) throw new Error("kavach-pg: nil underlying pg client/pool");
    return typeof textOrConfig === "string"
      ? rawClient.query<R>(textOrConfig, values)
      : rawClient.query<R>(textOrConfig);
  }

  const argStrs = formatPgArgs(values);
  const { call, normSQL, isWrite, hasReturning, isTxControl } = buildPgGatewayCall(sql, argStrs);

  // Transaction control (BEGIN/COMMIT/ROLLBACK/SAVEPOINT): no-op in Replay, pass-through in Record.
  if (isTxControl) {
    if (kv.isReplay()) {
      return {
        command: defaultCommand(sql),
        rowCount: null,
        oid: null,
        rows: [],
        fields: [],
      };
    }
    if (!rawClient) throw new Error("kavach-pg: nil underlying pg client/pool");
    return typeof textOrConfig === "string"
      ? rawClient.query<R>(textOrConfig, values)
      : rawClient.query<R>(textOrConfig);
  }

  // Write without RETURNING -> Output ("pg:exec")
  if (isWrite && !hasReturning) {
    if (kv.isReplay()) {
      const tag = defaultCommandTag(sql, 1);
      if (kv.activeStep()) {
        emitExecOutput(kv, sql, normSQL, argStrs, tag);
      }
      return {
        command: defaultCommand(sql),
        rowCount: 1,
        oid: null,
        rows: [],
        fields: [],
      };
    }
    if (!rawClient) throw new Error("kavach-pg: nil underlying pg client/pool");
    try {
      const res =
        typeof textOrConfig === "string"
          ? await rawClient.query<R>(textOrConfig, values)
          : await rawClient.query<R>(textOrConfig);
      emitExecOutput(kv, sql, normSQL, argStrs, defaultCommandTag(sql, res.rowCount ?? 0));
      return res;
    } catch (e) {
      emitExecOutput(kv, sql, normSQL, argStrs, "", e);
      throw e;
    }
  }

  // SELECT or INSERT/UPDATE/DELETE ... RETURNING -> Gateway (plus Output if isWrite && hasReturning)
  if (kv.isReplay()) {
    if (!kv.activeStep()) {
      // Startup healthcheck (e.g. SELECT 1) outside a step: succeed offline.
      return {
        command: defaultCommand(sql),
        rowCount: 0,
        oid: null,
        rows: [],
        fields: [],
      };
    }
    let respBytes: Uint8Array;
    try {
      respBytes = await kv.replayGateway(call);
    } catch (e) {
      throw reconstructPgError(e);
    }
    const rs = unmarshalResultSet(respBytes);
    if (rs.error) {
      throw reconstructPgError(new Error(rs.error));
    }
    if (isWrite && hasReturning) {
      emitExecOutput(kv, sql, normSQL, argStrs, rs.command_tag || defaultCommandTag(sql, rs.rows.length));
    }
    return buildQueryResult<R>(sql, rs, rowMode);
  }

  // Record mode: execute real Postgres query and capture typed rows.
  if (!rawClient) throw new Error("kavach-pg: nil underlying pg client/pool");
  let res: PgQueryResult<R>;
  try {
    res =
      typeof textOrConfig === "string"
        ? await rawClient.query<R>(textOrConfig, values)
        : await rawClient.query<R>(textOrConfig);
  } catch (e) {
    kv.recordGateway({ ...call, error: e instanceof Error ? e.message : String(e) });
    if (isWrite && hasReturning) {
      emitExecOutput(kv, sql, normSQL, argStrs, "", e);
    }
    throw e;
  }

  const rs = materializeQueryResult(sql, res, rowMode);
  kv.recordGateway({
    ...call,
    response: new Uint8Array(Buffer.from(JSON.stringify(rs), "utf8")),
  });
  if (isWrite && hasReturning) {
    emitExecOutput(kv, sql, normSQL, argStrs, rs.command_tag || defaultCommandTag(sql, rs.rows.length));
  }
  return res;
}

function emitExecOutput(
  kv: Kavach,
  sql: string,
  normSQL: string,
  args: string[],
  commandTag: string,
  err?: unknown,
): void {
  const rec: RecordedExec = {
    sql,
    normalized_sql: normSQL,
  };
  if (args.length > 0) rec.args = args;
  if (commandTag) rec.command_tag = commandTag;
  if (err) rec.error = err instanceof Error ? err.message : String(err);
  kv.recordOutput({
    sink: "pg:exec",
    key: normSQL,
    data: new Uint8Array(Buffer.from(JSON.stringify(rec), "utf8")),
  });
}

function materializeQueryResult<R>(sql: string, res: PgQueryResult<R>, rowMode?: string): RecordedResultSet {
  let columns: string[] = [];
  if (Array.isArray(res.fields) && res.fields.length > 0) {
    columns = res.fields.map((f) => f.name);
  } else if (res.rows.length > 0 && res.rows[0] && typeof res.rows[0] === "object" && !Array.isArray(res.rows[0])) {
    columns = Object.keys(res.rows[0] as Record<string, unknown>);
  }

  const rows = res.rows.map((row) => {
    if (rowMode === "array" || Array.isArray(row)) {
      return (row as unknown[]).map((v) => encodeCell(v));
    }
    const obj = row as Record<string, unknown>;
    return columns.map((col) => encodeCell(obj[col]));
  });

  const tag = defaultCommandTag(sql, res.rowCount ?? rows.length);
  return {
    columns,
    rows,
    command_tag: tag,
    rows_affected: res.rowCount ?? rows.length,
  };
}

function unmarshalResultSet(data: Uint8Array): RecordedResultSet {
  if (data.length === 0) return { columns: [], rows: [] };
  const parsed = JSON.parse(Buffer.from(data).toString("utf8")) as Partial<RecordedResultSet>;
  return {
    columns: Array.isArray(parsed.columns) ? parsed.columns : [],
    rows: Array.isArray(parsed.rows) ? parsed.rows : [],
    command_tag: parsed.command_tag,
    rows_affected: parsed.rows_affected,
    error: parsed.error,
  };
}

function buildQueryResult<R>(sql: string, rs: RecordedResultSet, rowMode?: string): PgQueryResult<R> {
  const fields: PgFieldDef[] = rs.columns.map((name) => ({ name }));
  const rows = rs.rows.map((cells) => {
    if (rowMode === "array") {
      return cells.map((c) => decodeCell(c)) as unknown as R;
    }
    const obj: Record<string, unknown> = {};
    for (let i = 0; i < rs.columns.length; i++) {
      obj[rs.columns[i]] = i < cells.length ? decodeCell(cells[i]) : null;
    }
    return obj as unknown as R;
  });
  return {
    command: defaultCommand(sql),
    rowCount: rs.rows_affected ?? rows.length,
    oid: null,
    rows,
    fields,
  };
}

/**
 * Wraps a `pg` `Client` (or `null` in offline Replay mode) with Kavach capture/replay.
 */
export function wrapClient<T extends PgClientLike>(kv: Kavach, client: T | null): WrappedPgClient<T> {
  const wrapped: WrappedPgClient<T> = {
    raw: () => client,
    query(textOrConfig: string | PgQueryConfig, valuesOrCb?: unknown[] | PgQueryCallback, maybeCb?: PgQueryCallback): any {
      const { callback } = normalizeQueryArgs(textOrConfig, valuesOrCb, maybeCb);
      const promise = executeQuery<Record<string, unknown>>(
        kv,
        client,
        textOrConfig,
        typeof valuesOrCb === "function" ? undefined : valuesOrCb,
      );
      if (callback) {
        promise.then(
          (res) => callback(null, res),
          (err) => callback(err instanceof Error ? err : new Error(String(err))),
        );
        return;
      }
      return promise;
    },
    release(err?: Error | boolean): void {
      if (!kv.isReplay() && client?.release) {
        client.release(err);
      }
    },
    async end(): Promise<void> {
      if (!kv.isReplay() && client?.end) {
        await client.end();
      }
    },
    on(event: string, listener: (...args: unknown[]) => void): unknown {
      if (client?.on) return client.on(event, listener);
      return wrapped;
    },
    once(event: string, listener: (...args: unknown[]) => void): unknown {
      if (client?.once) return client.once(event, listener);
      return wrapped;
    },
    removeListener(event: string, listener: (...args: unknown[]) => void): unknown {
      if (client?.removeListener) return client.removeListener(event, listener);
      return wrapped;
    },
  };
  return wrapped;
}

/**
 * Wraps a `pg` `Pool` (works directly under `node-postgres`, Drizzle `drizzle(pool)`,
 * and Prisma `@prisma/adapter-pg`). In Replay mode, `pool` may be `null` and NO
 * Postgres socket is ever opened.
 */
export function wrapPool<T extends PgPoolLike>(kv: Kavach, pool: T | null): WrappedPgPool<T> {
  const base = wrapClient(kv, pool);
  const wrapped: WrappedPgPool<T> = {
    ...base,
    raw: () => pool,
    async connect(): Promise<WrappedPgClient> {
      if (kv.isReplay()) {
        return wrapClient(kv, null);
      }
      if (!pool) throw new Error("kavach-pg: nil underlying pg Pool");
      const rawClient = await pool.connect();
      return wrapClient(kv, rawClient);
    },
    async end(): Promise<void> {
      if (!kv.isReplay() && pool?.end) {
        await pool.end();
      }
    },
  };
  return wrapped;
}

/** Creates an offline Replay `pg` Pool that never opens a TCP socket to Postgres. */
export function createReplayPool(kv: Kavach): WrappedPgPool<PgPoolLike> {
  return wrapPool<PgPoolLike>(kv, null);
}

/** Creates an offline Replay `pg` Client that never opens a TCP socket to Postgres. */
export function createReplayClient(kv: Kavach): WrappedPgClient<PgClientLike> {
  return wrapClient<PgClientLike>(kv, null);
}
