import { EventEmitter } from "node:events";
import http from "node:http";
import { Readable } from "node:stream";
import type { Input, Kavach, StepRunner } from "@kavachlabs/core";

const DEFAULT_MAX_BODY_BYTES = 4 << 20; // 4 MiB

export interface WrapHandlerOptions {
  /** Override Input.source (default: "http:<METHOD> <PATH>"). */
  sourceFunc?: (method: string, path: string, headers: Record<string, string>) => string;
  /** Minimum HTTP status code treated as a step error (default: 500; <= 0 disables). */
  errorStatus?: number;
  /** Custom error classifier overriding errorStatus. */
  errorClassifier?: (status: number, body: Uint8Array) => Error | undefined;
  /** Max buffered body bytes for recording (default: 4 MiB). */
  maxBodyBytes?: number;
}

export interface RecordedHttpRequest {
  method: string;
  path: string;
  raw_query?: string;
  host?: string;
  headers?: Record<string, string>;
  body_json?: unknown;
  body?: string; // base64
  has_clock?: boolean;
}

export interface RecordedHttpResponse {
  status: number;
  headers?: Record<string, string>;
  body_json?: unknown;
  body?: string; // base64
}

/** Fastify-like instance supporting offline `.inject()`. */
export interface FastifyLike {
  inject(opts: {
    method: string;
    url: string;
    headers?: Record<string, string>;
    payload?: Buffer | string;
  }): Promise<{
    statusCode: number;
    headers: Record<string, string | string[] | undefined>;
    rawPayload?: Buffer;
    body?: string;
  }>;
  listen?: (opts: { port: number; host?: string }) => Promise<unknown>;
  server?: http.Server;
  routing?: http.RequestListener;
}

/** Hono / Fetch-standard app supporting `.fetch(Request) => Promise<Response>`. */
export interface FetchAppLike {
  fetch(request: Request): Response | Promise<Response>;
}

export type HttpHandlerTarget = http.RequestListener | FastifyLike | FetchAppLike;

export interface WrappedHttpHandler extends StepRunner {
  (req: http.IncomingMessage, res: http.ServerResponse): void;
  runStep(input: Input): Promise<void>;
  serve(): Promise<void>;
  fetch?: (request: Request) => Promise<Response>;
}

function isFetchApp(x: unknown): x is FetchAppLike {
  return Boolean(x && typeof x === "object" && typeof (x as FetchAppLike).fetch === "function");
}

function isFastifyApp(x: unknown): x is FastifyLike {
  return Boolean(x && typeof x === "object" && typeof (x as FastifyLike).inject === "function");
}

/**
 * Wraps a Node `http.RequestListener`, Express `app`, Fastify instance, or Hono/Fetch app
 * for deterministic Kavach record & offline socket-free Replay.
 *
 * The returned handler is both a callable Node `http.RequestListener` and a `StepRunner`
 * compatible with `kv.serveOrReplay(wrapped)`.
 */
export function wrapHandler(
  kv: Kavach,
  handler: HttpHandlerTarget,
  opts: WrapHandlerOptions = {},
): WrappedHttpHandler {
  const errorStatus = opts.errorStatus ?? 500;
  const maxBodyBytes = opts.maxBodyBytes ?? DEFAULT_MAX_BODY_BYTES;

  const classifyError = (status: number, body: Uint8Array): Error | undefined => {
    if (opts.errorClassifier) return opts.errorClassifier(status, body);
    if (errorStatus > 0 && status >= errorStatus) {
      const text = Buffer.from(body).toString("utf8").trim();
      return new Error(`http ${status}: ${text}`);
    }
    return undefined;
  };

  const sourceFor = (method: string, pathname: string, headers: Record<string, string>): string => {
    if (opts.sourceFunc) {
      const custom = opts.sourceFunc(method, pathname, headers);
      if (custom) return custom;
    }
    return `http:${method.toUpperCase()} ${pathname}`;
  };

  const handleFetchApp = async (app: FetchAppLike, req: Request): Promise<Response> => {
    if (kv.isOff() || (kv.isReplay() && kv.activeStep())) {
      return app.fetch(req);
    }
    const url = new URL(req.url);
    const flatHdrs: Record<string, string> = {};
    req.headers.forEach((v, k) => {
      flatHdrs[k] = v;
    });
    const rawBody = req.body ? new Uint8Array(await req.clone().arrayBuffer()) : new Uint8Array(0);
    const recReq: RecordedHttpRequest = {
      method: req.method.toUpperCase(),
      path: url.pathname,
      raw_query: url.search ? url.search.slice(1) : undefined,
      host: url.host || undefined,
      headers: kv.scrubHeaders(flatHdrs),
      has_clock: true,
    };
    attachRecordedBody(recReq, rawBody);
    const input: Input = {
      source: sourceFor(req.method, url.pathname, flatHdrs),
      position: flatHdrs["x-request-id"] ?? "",
      headers: flatHdrs,
      data: new Uint8Array(Buffer.from(JSON.stringify(recReq), "utf8")),
    };

    let response!: Response;
    await kv.runStep(input, async (sc) => {
      kv.now(); // capture deterministic step-start clock
      response = await app.fetch(req);
      const respBody = new Uint8Array(await response.clone().arrayBuffer());
      const respHdrs: Record<string, string> = {};
      response.headers.forEach((v, k) => {
        respHdrs[k] = v;
      });
      sc.recordOutput({
        sink: "http:response",
        data: encodeRecordedResponse(response.status, respHdrs, respBody),
      });
      const stepErr = classifyError(response.status, respBody);
      if (stepErr) {
        // End step with error without breaking live HTTP response return.
        await sc.end(stepErr);
      }
    }).catch(() => {
      // Live HTTP responses are still returned even on 5xx step errors.
    });
    return response;
  };

  const nodeListener: http.RequestListener = (req, res) => {
    if (isFetchApp(handler)) {
      void (async () => {
        const body = await readIncomingBody(req, maxBodyBytes);
        const host = req.headers.host || "localhost";
        const fullUrl = `http://${host}${req.url || "/"}`;
        const hdrs = flattenNodeHeaders(req.headers);
        const method = (req.method || "GET").toUpperCase();
        const webReq = new Request(fullUrl, {
          method,
          headers: hdrs,
          body: method !== "GET" && method !== "HEAD" && body.length > 0 ? Buffer.from(body) : undefined,
        });
        const webResp = await handleFetchApp(handler, webReq);
        res.statusCode = webResp.status;
        webResp.headers.forEach((v, k) => res.setHeader(k, v));
        const buf = Buffer.from(await webResp.arrayBuffer());
        res.end(buf);
      })();
      return;
    }

    if (isFastifyApp(handler)) {
      void (async () => {
        const body = await readIncomingBody(req, maxBodyBytes);
        const flatHdrs = flattenNodeHeaders(req.headers);
        const method = (req.method || "GET").toUpperCase();
        const parsed = parseRequestTarget(req.url || "/");
        if (kv.isOff()) {
          const out = await handler.inject({
            method,
            url: req.url || "/",
            headers: flatHdrs,
            payload: body.length > 0 ? Buffer.from(body) : undefined,
          });
          res.statusCode = out.statusCode;
          for (const [k, v] of Object.entries(out.headers)) {
            if (v !== undefined) res.setHeader(k, v);
          }
          res.end(out.rawPayload ?? out.body ?? "");
          return;
        }
        const recReq: RecordedHttpRequest = {
          method,
          path: parsed.pathname,
          raw_query: parsed.rawQuery || undefined,
          host: req.headers.host || undefined,
          headers: kv.scrubHeaders(flatHdrs),
          has_clock: true,
        };
        attachRecordedBody(recReq, body);
        const input: Input = {
          source: sourceFor(method, parsed.pathname, flatHdrs),
          position: flatHdrs["x-request-id"] ?? "",
          headers: flatHdrs,
          data: new Uint8Array(Buffer.from(JSON.stringify(recReq), "utf8")),
        };
        let out!: Awaited<ReturnType<FastifyLike["inject"]>>;
        await kv.runStep(input, async (sc) => {
          kv.now();
          out = await handler.inject({
            method,
            url: req.url || "/",
            headers: flatHdrs,
            payload: body.length > 0 ? Buffer.from(body) : undefined,
          });
          const respBytes = out.rawPayload
            ? new Uint8Array(out.rawPayload)
            : new Uint8Array(Buffer.from(out.body ?? "", "utf8"));
          const respHdrs = flattenNodeHeaders(out.headers);
          sc.recordOutput({
            sink: "http:response",
            data: encodeRecordedResponse(out.statusCode, respHdrs, respBytes),
          });
          const err = classifyError(out.statusCode, respBytes);
          if (err) await sc.end(err);
        }).catch(() => {});
        res.statusCode = out.statusCode;
        for (const [k, v] of Object.entries(out.headers)) {
          if (v !== undefined) res.setHeader(k, v);
        }
        res.end(out.rawPayload ?? out.body ?? "");
      })();
      return;
    }

    // Standard Node http.RequestListener / Express app
    if (kv.isOff() || (kv.isReplay() && kv.activeStep())) {
      handler(req, res);
      return;
    }

    void (async () => {
      const body = await readAndRestoreIncomingBody(req, maxBodyBytes);
      const flatHdrs = flattenNodeHeaders(req.headers);
      const method = (req.method || "GET").toUpperCase();
      const parsed = parseRequestTarget(req.url || "/");
      const recReq: RecordedHttpRequest = {
        method,
        path: parsed.pathname,
        raw_query: parsed.rawQuery || undefined,
        host: req.headers.host || undefined,
        headers: kv.scrubHeaders(flatHdrs),
        has_clock: true,
      };
      attachRecordedBody(recReq, body);

      const input: Input = {
        source: sourceFor(method, parsed.pathname, flatHdrs),
        position: flatHdrs["x-request-id"] ?? "",
        headers: flatHdrs,
        data: new Uint8Array(Buffer.from(JSON.stringify(recReq), "utf8")),
      };

      const sc = kv.beginStep(input);
      kv.now();

      const chunks: Buffer[] = [];
      let totalCaptured = 0;
      const appendCapture = (chunk: unknown, enc?: BufferEncoding) => {
        if (chunk === undefined || chunk === null || totalCaptured >= maxBodyBytes) return;
        const buf = Buffer.isBuffer(chunk)
          ? chunk
          : typeof chunk === "string"
            ? Buffer.from(chunk, enc ?? "utf8")
            : Buffer.from(chunk as Uint8Array);
        const take = Math.min(buf.length, maxBodyBytes - totalCaptured);
        if (take > 0) {
          chunks.push(buf.subarray(0, take));
          totalCaptured += take;
        }
      };

      const origWrite = res.write.bind(res);
      const origEnd = res.end.bind(res);
      let finalized = false;

      const finalize = (err?: unknown) => {
        if (finalized) return;
        finalized = true;
        const respBytes = new Uint8Array(Buffer.concat(chunks));
        const respHdrs = flattenOutgoingHeaders(res.getHeaders());
        const status = res.statusCode || 200;
        sc.recordOutput({
          sink: "http:response",
          data: encodeRecordedResponse(status, respHdrs, respBytes),
        });
        const stepErr = err ?? classifyError(status, respBytes);
        void sc.end(stepErr);
      };

      (res as unknown as { write: (...args: unknown[]) => boolean }).write = (
        chunk: unknown,
        encOrCb?: unknown,
        cb?: unknown,
      ) => {
        const enc = typeof encOrCb === "string" ? (encOrCb as BufferEncoding) : undefined;
        appendCapture(chunk, enc);
        return (origWrite as (...a: unknown[]) => boolean)(chunk, encOrCb, cb);
      };

      (res as unknown as { end: (...args: unknown[]) => http.ServerResponse }).end = (
        chunk?: unknown,
        encOrCb?: unknown,
        cb?: unknown,
      ) => {
        if (typeof chunk !== "function") {
          const enc = typeof encOrCb === "string" ? (encOrCb as BufferEncoding) : undefined;
          appendCapture(chunk, enc);
        }
        finalize();
        return (origEnd as (...a: unknown[]) => http.ServerResponse)(chunk, encOrCb, cb);
      };

      res.on("close", () => finalize());

      try {
        await (handler as (r: http.IncomingMessage, s: http.ServerResponse) => unknown)(req, res);
      } catch (e) {
        finalize(e);
        if (!res.headersSent) {
          res.statusCode = 500;
          res.end(e instanceof Error ? e.message : String(e));
        }
      }
    })();
  };

  const runStep = async (input: Input): Promise<void> => {
    const parsed = decodeReplayRequest(input);
    if (parsed.hasClock) {
      kv.now();
    }

    if (isFetchApp(handler)) {
      const targetUrl = `http://${parsed.host || "localhost"}${parsed.target}`;
      const webReq = new Request(targetUrl, {
        method: parsed.method,
        headers: parsed.headers,
        body:
          parsed.method !== "GET" && parsed.method !== "HEAD" && parsed.body.length > 0
            ? Buffer.from(parsed.body)
            : undefined,
      });
      const resp = await handler.fetch(webReq);
      const respBytes = new Uint8Array(await resp.arrayBuffer());
      const respHdrs: Record<string, string> = {};
      resp.headers.forEach((v, k) => {
        respHdrs[k] = v;
      });
      kv.recordOutput({
        sink: "http:response",
        data: encodeRecordedResponse(resp.status, respHdrs, respBytes),
      });
      const err = classifyError(resp.status, respBytes);
      if (err) throw err;
      return;
    }

    if (isFastifyApp(handler)) {
      const out = await handler.inject({
        method: parsed.method,
        url: parsed.target,
        headers: parsed.headers,
        payload: parsed.body.length > 0 ? Buffer.from(parsed.body) : undefined,
      });
      const respBytes = out.rawPayload
        ? new Uint8Array(out.rawPayload)
        : new Uint8Array(Buffer.from(out.body ?? "", "utf8"));
      const respHdrs = flattenNodeHeaders(out.headers);
      kv.recordOutput({
        sink: "http:response",
        data: encodeRecordedResponse(out.statusCode, respHdrs, respBytes),
      });
      const err = classifyError(out.statusCode, respBytes);
      if (err) throw err;
      return;
    }

    // Zero-socket synthetic Node IncomingMessage + ServerResponse for Node http / Express!
    const synReq = createSyntheticRequest(parsed.method, parsed.target, parsed.headers, parsed.body);
    const synRes = createSyntheticResponse();

    await new Promise<void>((resolve, reject) => {
      synRes.on("finish", resolve);
      try {
        const ret: unknown = (handler as (r: http.IncomingMessage, s: http.ServerResponse) => unknown)(
          synReq as unknown as http.IncomingMessage,
          synRes as unknown as http.ServerResponse,
        );
        if (ret && typeof (ret as Promise<unknown>).then === "function") {
          (ret as Promise<unknown>).then(
            () => {
              if (synRes.writableEnded) resolve();
            },
            reject,
          );
        }
      } catch (e) {
        reject(e);
      }
    });

    const respBytes = synRes.getBodyBytes();
    const respHdrs = synRes.getFlatHeaders();
    kv.recordOutput({
      sink: "http:response",
      data: encodeRecordedResponse(synRes.statusCode, respHdrs, respBytes),
    });
    const err = classifyError(synRes.statusCode, respBytes);
    if (err) throw err;
  };

  const wrapped = Object.assign(nodeListener, {
    runStep,
    async serve(): Promise<void> {
      const addr = kv.listenAddr();
      const colon = addr.lastIndexOf(":");
      const host = colon >= 0 ? addr.slice(0, colon) : "";
      const port = colon >= 0 ? Number(addr.slice(colon + 1)) : Number(addr) || 8080;
      if (isFastifyApp(handler) && handler.listen) {
        await handler.listen({ port, host: host || "0.0.0.0" });
        return;
      }
      await new Promise<void>((resolve, reject) => {
        const srv = http.createServer(nodeListener);
        srv.on("error", reject);
        srv.listen(port, host || undefined, () => {
          srv.on("close", resolve);
        });
      });
    },
    ...(isFetchApp(handler)
      ? {
          fetch(request: Request): Promise<Response> {
            return handleFetchApp(handler, request);
          },
        }
      : {}),
  }) as WrappedHttpHandler;

  return wrapped;
}

/** Express / Connect drop-in middleware capturing inbound HTTP requests as Kavach steps. */
export function createMiddleware(
  kv: Kavach,
  opts: WrapHandlerOptions = {},
): (req: http.IncomingMessage, res: http.ServerResponse, next: (err?: unknown) => void) => void {
  return (req, res, next) => {
    const wrapped = wrapHandler(
      kv,
      (_r, _s) => {
        next();
      },
      opts,
    );
    wrapped(req, res);
  };
}

interface DecodedReplayRequest {
  method: string;
  target: string;
  host: string;
  headers: Record<string, string>;
  body: Uint8Array;
  hasClock: boolean;
}

function decodeReplayRequest(input: Input): DecodedReplayRequest {
  let method = "POST";
  let pathname = "/";
  if (input.source.startsWith("http:")) {
    const rest = input.source.slice("http:".length).trim();
    const sp = rest.indexOf(" ");
    if (sp > 0) {
      method = rest.slice(0, sp).trim().toUpperCase();
      pathname = rest.slice(sp + 1).trim();
    } else if (rest) {
      pathname = rest;
    }
  }

  let rawQuery = "";
  let host = "";
  let headers: Record<string, string> = input.headers ? { ...input.headers } : {};
  let body: Uint8Array = input.data;
  let hasClock = false;

  if (input.data.length > 0 && input.data[0] === 0x7b /* { */) {
    try {
      const env = JSON.parse(Buffer.from(input.data).toString("utf8")) as Record<string, unknown>;
      if (typeof env.method === "string" || typeof env.path === "string" || typeof env.url === "string") {
        if (typeof env.method === "string" && env.method) method = env.method.toUpperCase();
        if (typeof env.path === "string" && env.path) pathname = env.path;
        else if (typeof env.url === "string" && env.url) pathname = env.url;
        if (typeof env.raw_query === "string") rawQuery = env.raw_query;
        if (typeof env.host === "string") host = env.host;
        if (env.headers && typeof env.headers === "object" && !Array.isArray(env.headers)) {
          headers = {};
          for (const [k, v] of Object.entries(env.headers as Record<string, unknown>)) {
            if (v !== undefined && v !== null) headers[k.toLowerCase()] = String(v);
          }
        }
        hasClock = env.has_clock === true;
        if (env.body_json !== undefined) {
          body = new Uint8Array(Buffer.from(JSON.stringify(env.body_json), "utf8"));
        } else if (typeof env.body === "string") {
          body = new Uint8Array(Buffer.from(env.body, "base64"));
        } else {
          body = new Uint8Array(0);
        }
      }
    } catch {
      // treat input.data as raw body
    }
  }

  if (!pathname) pathname = "/";
  if (!pathname.startsWith("/") && !pathname.startsWith("http://") && !pathname.startsWith("https://")) {
    pathname = "/" + pathname;
  }
  let target = pathname;
  if (rawQuery && !target.includes("?")) {
    target += "?" + rawQuery;
  }
  return { method, target, host, headers, body, hasClock };
}

function attachRecordedBody(dst: { body_json?: unknown; body?: string }, body: Uint8Array): void {
  if (body.length === 0) return;
  const text = Buffer.from(body).toString("utf8").trim();
  if (text.length > 0 && (text[0] === "{" || text[0] === "[")) {
    try {
      dst.body_json = JSON.parse(text);
      return;
    } catch {
      // fall through to base64
    }
  }
  dst.body = Buffer.from(body).toString("base64");
}

function encodeRecordedResponse(status: number, headers: Record<string, string>, body: Uint8Array): Uint8Array {
  const rec: RecordedHttpResponse = {
    status: status || 200,
  };
  if (Object.keys(headers).length > 0) rec.headers = headers;
  attachRecordedBody(rec, body);
  return new Uint8Array(Buffer.from(JSON.stringify(rec), "utf8"));
}

function parseRequestTarget(url: string): { pathname: string; rawQuery: string } {
  const q = url.indexOf("?");
  if (q < 0) return { pathname: url || "/", rawQuery: "" };
  return { pathname: url.slice(0, q) || "/", rawQuery: url.slice(q + 1) };
}

function flattenNodeHeaders(h: Record<string, string | string[] | number | undefined>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(h)) {
    if (v === undefined) continue;
    out[k.toLowerCase()] = Array.isArray(v) ? v.join(", ") : String(v);
  }
  return out;
}

function flattenOutgoingHeaders(h: http.OutgoingHttpHeaders): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(h)) {
    if (v === undefined) continue;
    out[k.toLowerCase()] = Array.isArray(v) ? v.join(", ") : String(v);
  }
  return out;
}

async function readIncomingBody(req: http.IncomingMessage, maxBytes: number): Promise<Uint8Array> {
  const chunks: Buffer[] = [];
  let total = 0;
  for await (const chunk of req) {
    const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as Uint8Array);
    if (total < maxBytes) {
      chunks.push(buf.subarray(0, Math.min(buf.length, maxBytes - total)));
      total += buf.length;
    }
  }
  return new Uint8Array(Buffer.concat(chunks));
}

async function readAndRestoreIncomingBody(req: http.IncomingMessage, maxBytes: number): Promise<Uint8Array> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as Uint8Array));
  }
  const full = Buffer.concat(chunks);
  // Restore body stream so downstream Express/Node handlers read the full body.
  const restored = Readable.from(full.length > 0 ? [full] : []);
  (req as unknown as { [Symbol.asyncIterator]: () => AsyncIterableIterator<unknown> })[Symbol.asyncIterator] = () =>
    restored[Symbol.asyncIterator]();
  req.on = restored.on.bind(restored) as typeof req.on;
  req.once = restored.once.bind(restored) as typeof req.once;
  req.read = restored.read.bind(restored) as typeof req.read;
  req.pipe = restored.pipe.bind(restored) as typeof req.pipe;
  return new Uint8Array(full.length > maxBytes ? full.subarray(0, maxBytes) : full);
}

class SyntheticIncomingMessage extends Readable {
  method: string;
  url: string;
  headers: Record<string, string>;
  httpVersion = "1.1";
  httpVersionMajor = 1;
  httpVersionMinor = 1;
  socket: Record<string, unknown> = { remoteAddress: "127.0.0.1", encrypted: false };
  private readonly bodyBuf: Buffer;
  private pushed = false;

  constructor(method: string, url: string, headers: Record<string, string>, body: Uint8Array) {
    super();
    this.method = method;
    this.url = url;
    this.headers = {};
    for (const [k, v] of Object.entries(headers)) {
      this.headers[k.toLowerCase()] = v;
    }
    this.bodyBuf = Buffer.from(body);
  }

  override _read(): void {
    if (this.pushed) return;
    this.pushed = true;
    if (this.bodyBuf.length > 0) this.push(this.bodyBuf);
    this.push(null);
  }
}

class SyntheticServerResponse extends EventEmitter {
  statusCode = 200;
  statusMessage = "OK";
  headersSent = false;
  writableEnded = false;
  private readonly hdrs = new Map<string, string | string[]>();
  private readonly chunks: Buffer[] = [];

  setHeader(name: string, value: string | number | readonly string[]): this {
    const key = name.toLowerCase();
    this.hdrs.set(key, Array.isArray(value) ? [...value] : String(value));
    return this;
  }

  getHeader(name: string): string | string[] | undefined {
    return this.hdrs.get(name.toLowerCase());
  }

  getHeaders(): Record<string, string | string[]> {
    const out: Record<string, string | string[]> = {};
    for (const [k, v] of this.hdrs.entries()) out[k] = v;
    return out;
  }

  hasHeader(name: string): boolean {
    return this.hdrs.has(name.toLowerCase());
  }

  removeHeader(name: string): void {
    this.hdrs.delete(name.toLowerCase());
  }

  writeHead(
    statusCode: number,
    statusMessageOrHeaders?: string | Record<string, string | number | readonly string[]>,
    headers?: Record<string, string | number | readonly string[]>,
  ): this {
    this.statusCode = statusCode;
    let hdrObj: Record<string, string | number | readonly string[]> | undefined;
    if (typeof statusMessageOrHeaders === "string") {
      this.statusMessage = statusMessageOrHeaders;
      hdrObj = headers;
    } else {
      hdrObj = statusMessageOrHeaders;
    }
    if (hdrObj) {
      for (const [k, v] of Object.entries(hdrObj)) {
        this.setHeader(k, v);
      }
    }
    this.headersSent = true;
    return this;
  }

  write(chunk: unknown, encOrCb?: unknown, cb?: unknown): boolean {
    this.headersSent = true;
    if (chunk !== undefined && chunk !== null) {
      const enc = typeof encOrCb === "string" ? (encOrCb as BufferEncoding) : "utf8";
      const buf = Buffer.isBuffer(chunk)
        ? chunk
        : typeof chunk === "string"
          ? Buffer.from(chunk, enc)
          : Buffer.from(chunk as Uint8Array);
      this.chunks.push(buf);
    }
    const callback = typeof encOrCb === "function" ? encOrCb : typeof cb === "function" ? cb : undefined;
    if (callback) (callback as () => void)();
    return true;
  }

  end(chunk?: unknown, encOrCb?: unknown, cb?: unknown): this {
    if (typeof chunk === "function") {
      cb = chunk;
      chunk = undefined;
    } else if (chunk !== undefined && chunk !== null) {
      this.write(chunk, typeof encOrCb === "string" ? encOrCb : undefined);
    }
    this.headersSent = true;
    this.writableEnded = true;
    const callback = typeof encOrCb === "function" ? encOrCb : typeof cb === "function" ? cb : undefined;
    if (callback) (callback as () => void)();
    this.emit("finish");
    return this;
  }

  getBodyBytes(): Uint8Array {
    return new Uint8Array(Buffer.concat(this.chunks));
  }

  getFlatHeaders(): Record<string, string> {
    const out: Record<string, string> = {};
    for (const [k, v] of this.hdrs.entries()) {
      out[k] = Array.isArray(v) ? v.join(", ") : v;
    }
    return out;
  }
}

function createSyntheticRequest(
  method: string,
  url: string,
  headers: Record<string, string>,
  body: Uint8Array,
): SyntheticIncomingMessage {
  return new SyntheticIncomingMessage(method, url, headers, body);
}

function createSyntheticResponse(): SyntheticServerResponse {
  return new SyntheticServerResponse();
}
