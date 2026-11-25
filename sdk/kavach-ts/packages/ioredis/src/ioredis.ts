import { ErrNoActiveStep, type GatewayCall, type Kavach } from "@kavach/core";

export interface IORedisPipelineLike {
  exec(): Promise<Array<[Error | null, unknown]>>;
  [cmd: string]: (...args: unknown[]) => unknown;
}

export interface IORedisLike {
  status?: string;
  ping(msg?: string): Promise<string>;
  get(key: string): Promise<string | null>;
  set(key: string, value: string | number | Buffer, ...args: unknown[]): Promise<string | null>;
  setex?(key: string, seconds: number, value: string | number | Buffer): Promise<string>;
  del(...keys: string[]): Promise<number>;
  exists(...keys: string[]): Promise<number>;
  expire?(key: string, seconds: number): Promise<number>;
  incr?(key: string): Promise<number>;
  incrby?(key: string, increment: number): Promise<number>;
  decr?(key: string): Promise<number>;
  hget?(key: string, field: string): Promise<string | null>;
  hset?(key: string, ...args: unknown[]): Promise<number>;
  hgetall?(key: string): Promise<Record<string, string>>;
  zadd(key: string, ...args: unknown[]): Promise<number | string>;
  zrem?(key: string, ...members: unknown[]): Promise<number>;
  zrange?(key: string, start: number | string, stop: number | string, ...args: unknown[]): Promise<string[]>;
  zscore?(key: string, member: string): Promise<string | null>;
  mget?(...keys: string[]): Promise<Array<string | null>>;
  call?(command: string, ...args: unknown[]): Promise<unknown>;
  pipeline?(): IORedisPipelineLike;
  multi?(): IORedisPipelineLike;
  quit?(): Promise<string>;
  disconnect?(): void;
  on?(event: string, listener: (...args: unknown[]) => void): unknown;
  once?(event: string, listener: (...args: unknown[]) => void): unknown;
  [key: string]: unknown;
}

interface RecordedRedisReply {
  kind: string;
  is_nil?: boolean;
  val?: unknown;
  str?: string;
  int?: number;
  bool?: boolean;
  str_slice?: string[];
  map_ss?: Record<string, string>;
}

const HANDSHAKE_COMMANDS = new Set([
  "ping",
  "hello",
  "auth",
  "select",
  "client",
  "readonly",
  "readwrite",
  "echo",
]);

const NON_COMMAND_PROPS = new Set([
  "then",
  "catch",
  "finally",
  "toJSON",
  "inspect",
  "constructor",
  "status",
  "options",
  "on",
  "once",
  "off",
  "emit",
  "addListener",
  "removeListener",
  "removeAllListeners",
  "listeners",
  "connect",
  "disconnect",
  "quit",
  "duplicate",
  "pipeline",
  "multi",
  "raw",
]);

/**
 * Extracts ONLY stable key arguments from a Redis command, deliberately omitting
 * volatile values, TTLs, scores, and time-bucketed members (such as `ZADD key score member`),
 * matching Go `kavachredis.extractKeyArgs` byte-for-byte.
 */
export function extractRedisKeyArgs(cmdName: string, args: readonly unknown[]): string[] {
  const name = cmdName.trim().toLowerCase();
  if (args.length === 0) return [];
  switch (name) {
    case "ping":
    case "echo":
    case "auth":
    case "hello":
    case "select":
    case "client":
    case "info":
    case "dbsize":
    case "multi":
    case "exec":
    case "discard":
    case "quit":
      return [];
    case "hget":
    case "hexists":
    case "hincrby":
    case "hincrbyfloat":
      return args.length >= 2 ? [argToString(args[0]), argToString(args[1])] : [argToString(args[0])];
    case "mget":
    case "del":
    case "unlink":
    case "exists":
      return flattenArgs(args).map(argToString);
    case "mset":
    case "msetnx": {
      const flat = flattenArgs(args);
      const keys: string[] = [];
      for (let i = 0; i < flat.length; i += 2) {
        keys.push(argToString(flat[i]));
      }
      return keys;
    }
    case "eval":
    case "evalsha":
    case "eval_ro":
    case "evalsha_ro": {
      const out = [argToString(args[0])];
      if (args.length >= 2) {
        const numKeys = Number(argToString(args[1])) || 0;
        for (let i = 0; i < numKeys && 2 + i < args.length; i++) {
          out.push(argToString(args[2 + i]));
        }
      }
      return out;
    }
    default:
      // Standard single-key Redis commands: GET, SET, SETEX, EXPIRE, ZADD, ZREM,
      // ZRANGE, HSET, HGETALL, LPUSH, RPUSH, SADD, INCR, DECR, etc.
      return [argToString(args[0])];
  }
}

function flattenArgs(args: readonly unknown[]): unknown[] {
  const out: unknown[] = [];
  for (const a of args) {
    if (Array.isArray(a)) out.push(...a);
    else out.push(a);
  }
  return out;
}

function argToString(v: unknown): string {
  if (v === null || v === undefined) return "";
  if (typeof v === "string") return v;
  if (Buffer.isBuffer(v)) return v.toString("utf8");
  if (v instanceof Uint8Array) return Buffer.from(v).toString("utf8");
  return String(v);
}

function buildRedisGatewayCall(cmdName: string, args: readonly unknown[]): GatewayCall {
  const lower = cmdName.trim().toLowerCase();
  const upper = lower.toUpperCase();
  return {
    driver: "redis",
    gateway: `redis:${lower}`,
    op: upper,
    normalizedOp: upper,
    keyArgs: extractRedisKeyArgs(lower, args),
  };
}

function encodeRedisReply(val: unknown): Uint8Array {
  let rec: RecordedRedisReply;
  if (val === null || val === undefined) {
    rec = { kind: "nil", is_nil: true };
  } else if (typeof val === "string") {
    rec = { kind: "string", str: val, val };
  } else if (typeof val === "number") {
    rec = { kind: "int", int: val, val };
  } else if (typeof val === "boolean") {
    rec = { kind: "bool", bool: val, val };
  } else if (Array.isArray(val)) {
    rec = { kind: "slice", val };
  } else if (typeof val === "object") {
    rec = { kind: "object", val };
  } else {
    rec = { kind: "cmd", val };
  }
  return new Uint8Array(Buffer.from(JSON.stringify(rec), "utf8"));
}

function decodeRedisReply(data: Uint8Array): unknown {
  if (data.length === 0) return null;
  try {
    const rec = JSON.parse(Buffer.from(data).toString("utf8")) as RecordedRedisReply;
    if (rec && typeof rec === "object" && !Array.isArray(rec)) {
      if (rec.is_nil || rec.kind === "nil") return null;
      if (rec.val !== undefined) return rec.val;
      switch (rec.kind) {
        case "string":
        case "status":
          return rec.str ?? "";
        case "int":
          return rec.int ?? 0;
        case "bool":
          return rec.bool ?? false;
        case "string_slice":
          return rec.str_slice ?? [];
        case "map_ss":
          return rec.map_ss ?? {};
      }
    }
    return rec;
  } catch {
    return Buffer.from(data).toString("utf8");
  }
}

function reconstructRedisError(err: unknown): Error {
  const msg = err instanceof Error ? err.message : String(err);
  const replyErr = new Error(msg);
  if (
    msg.startsWith("LOADING ") ||
    msg.startsWith("READONLY ") ||
    msg.startsWith("CLUSTERDOWN ") ||
    msg.startsWith("ERR ") ||
    msg.startsWith("WRONGTYPE ")
  ) {
    replyErr.name = "ReplyError";
  }
  return replyErr;
}

async function execRedisCommand(
  kv: Kavach,
  raw: IORedisLike | null,
  cmdName: string,
  args: unknown[],
): Promise<unknown> {
  const lower = cmdName.trim().toLowerCase();

  if (kv.isOff()) {
    if (!raw) throw new Error(`kavach-ioredis: nil underlying redis client for ${lower}`);
    return invokeLiveRedis(raw, cmdName, args);
  }

  if (kv.isReplay()) {
    if (!kv.activeStep()) {
      if (lower === "ping") return args.length > 0 ? argToString(args[0]) : "PONG";
      if (HANDSHAKE_COMMANDS.has(lower)) return "OK";
      throw ErrNoActiveStep;
    }
    const call = buildRedisGatewayCall(lower, args);
    try {
      const respBytes = await kv.replayGateway(call);
      return decodeRedisReply(respBytes);
    } catch (e) {
      throw reconstructRedisError(e);
    }
  }

  // Record mode: execute real Redis command and capture Gateway if inside an active step.
  if (!raw) throw new Error(`kavach-ioredis: nil underlying redis client for ${lower}`);
  const call = buildRedisGatewayCall(lower, args);
  try {
    const val = await invokeLiveRedis(raw, cmdName, args);
    if (kv.activeStep()) {
      kv.recordGateway({
        ...call,
        response: encodeRedisReply(val),
      });
    }
    return val;
  } catch (e) {
    if (kv.activeStep()) {
      kv.recordGateway({
        ...call,
        error: e instanceof Error ? e.message : String(e),
      });
    }
    throw e;
  }
}

async function invokeLiveRedis(raw: IORedisLike, cmdName: string, args: unknown[]): Promise<unknown> {
  const fn = raw[cmdName] ?? raw[cmdName.toLowerCase()];
  if (typeof fn === "function") {
    return (fn as (...a: unknown[]) => unknown).apply(raw, args);
  }
  if (typeof raw.call === "function") {
    return raw.call(cmdName, ...args);
  }
  throw new Error(`kavach-ioredis: underlying client has no method ${JSON.stringify(cmdName)}`);
}

function createWrappedPipeline(kv: Kavach, raw: IORedisLike | null, isMulti: boolean): IORedisPipelineLike {
  const queued: Array<{ cmd: string; args: unknown[] }> = [];
  const rawPipe = !kv.isReplay() && raw ? (isMulti ? raw.multi?.() : raw.pipeline?.()) : undefined;

  const target: Record<string, unknown> = {
    async exec(): Promise<Array<[Error | null, unknown]>> {
      if (kv.isReplay()) {
        const results: Array<[Error | null, unknown]> = [];
        for (const q of queued) {
          try {
            const val = await execRedisCommand(kv, null, q.cmd, q.args);
            results.push([null, val]);
          } catch (e) {
            results.push([e instanceof Error ? e : new Error(String(e)), null]);
          }
        }
        return results;
      }
      if (rawPipe) {
        const res = await rawPipe.exec();
        if (kv.isRecord() && kv.activeStep()) {
          for (let i = 0; i < queued.length; i++) {
            const q = queued[i];
            const [err, val] = res[i] ?? [null, null];
            const call = buildRedisGatewayCall(q.cmd, q.args);
            if (err) {
              kv.recordGateway({ ...call, error: err.message });
            } else {
              kv.recordGateway({ ...call, response: encodeRedisReply(val) });
            }
          }
        }
        return res;
      }
      // Fallback if raw has no pipeline()
      const results: Array<[Error | null, unknown]> = [];
      for (const q of queued) {
        try {
          const val = await execRedisCommand(kv, raw, q.cmd, q.args);
          results.push([null, val]);
        } catch (e) {
          results.push([e instanceof Error ? e : new Error(String(e)), null]);
        }
      }
      return results;
    },
  };

  const proxy: IORedisPipelineLike = new Proxy(target, {
    get(obj, prop, receiver) {
      if (typeof prop === "symbol" || prop === "exec" || prop === "then") {
        return Reflect.get(obj, prop, receiver);
      }
      return (...args: unknown[]) => {
        queued.push({ cmd: prop, args });
        if (rawPipe && typeof rawPipe[prop] === "function") {
          rawPipe[prop](...args);
        }
        return proxy;
      };
    },
  }) as unknown as IORedisPipelineLike;

  return proxy;
}

/**
 * Wraps an `ioredis` client (or `null` in Replay mode) with Kavach record and offline replay.
 * Intercepts all Redis commands (including `zadd`, `get`, `set`, `hget`, `pipeline`, `multi`, `call`),
 * answers startup `ping()` with `'PONG'` offline, uses timestamp-independent `(cmd, key)`
 * fingerprinting, and injects realistic `LOADING` / `READONLY` / `CLUSTERDOWN` errors in Replay.
 */
export function wrapRedis<T extends IORedisLike>(kv: Kavach, redis: T | null): T {
  const baseTarget: Record<string, unknown> = {
    status: kv.isReplay() ? "ready" : (redis?.status ?? "ready"),
    raw: () => redis,
    async quit(): Promise<string> {
      if (!kv.isReplay() && redis?.quit) return redis.quit();
      return "OK";
    },
    disconnect(): void {
      if (!kv.isReplay() && redis?.disconnect) redis.disconnect();
    },
    async connect(): Promise<void> {
      if (!kv.isReplay() && typeof redis?.connect === "function") {
        await (redis.connect as () => Promise<void>)();
      }
    },
    pipeline(): IORedisPipelineLike {
      return createWrappedPipeline(kv, redis, false);
    },
    multi(): IORedisPipelineLike {
      return createWrappedPipeline(kv, redis, true);
    },
    call(command: string, ...args: unknown[]): Promise<unknown> {
      return execRedisCommand(kv, redis, command, args);
    },
    on(event: string, listener: (...args: unknown[]) => void): unknown {
      if (!kv.isReplay() && redis?.on) return redis.on(event, listener);
      return proxy;
    },
    once(event: string, listener: (...args: unknown[]) => void): unknown {
      if (!kv.isReplay() && redis?.once) return redis.once(event, listener);
      return proxy;
    },
  };

  const proxy = new Proxy(baseTarget, {
    get(obj, prop, receiver) {
      if (typeof prop === "symbol") {
        if (redis) return Reflect.get(redis, prop);
        return Reflect.get(obj, prop, receiver);
      }
      if (prop in obj) {
        return Reflect.get(obj, prop, receiver);
      }
      if (NON_COMMAND_PROPS.has(prop)) {
        if (redis && prop in redis) {
          const val = (redis as Record<string, unknown>)[prop];
          return typeof val === "function" ? val.bind(redis) : val;
        }
        return undefined;
      }
      return (...args: unknown[]) => execRedisCommand(kv, redis, prop, args);
    },
  });

  return proxy as unknown as T;
}

/** Creates an offline Replay `ioredis` client that never opens any TCP socket. */
export function createReplayRedis(kv: Kavach): IORedisLike {
  return wrapRedis<IORedisLike>(kv, null);
}
