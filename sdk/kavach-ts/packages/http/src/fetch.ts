import crypto from "node:crypto";
import type { GatewayCall, Kavach } from "@kavachlabs/core";

export interface WrapFetchOptions {
  /** Max response body bytes captured in Record mode (default: 4 MiB). */
  maxBodyBytes?: number;
}

interface RecordedFetchResponse {
  status_code: number;
  status?: string;
  headers?: Record<string, string[]>;
  body_json?: unknown;
  body?: string; // base64
}

/**
 * Wraps the standard Fetch API (`globalThis.fetch` or a custom `fetchFn`) to capture
 * outbound HTTP requests as Kavach `Gateway` calls (`http:<METHOD> <host><path>`)
 * in Record mode and replay them 100% offline with zero network I/O in Replay mode.
 */
export function wrapFetch(
  kv: Kavach,
  fetchFn: typeof fetch = globalThis.fetch,
  opts: WrapFetchOptions = {},
): typeof fetch {
  const maxBodyBytes = opts.maxBodyBytes ?? (4 << 20);

  return async function kavachFetch(
    input: Parameters<typeof fetch>[0],
    init?: Parameters<typeof fetch>[1],
  ): Promise<Response> {
    if (kv.isOff()) {
      return fetchFn(input, init);
    }

    const req = input instanceof Request ? input : new Request(input, init);
    const method = (init?.method ?? req.method ?? "GET").toUpperCase();
    const url = new URL(req.url);
    const hostPath = `${url.host}${url.pathname}`;
    const op = `${method} ${hostPath}`;

    const keyArgs: string[] = [hostPath];
    const rawQuery = url.search ? url.search.slice(1) : "";
    if (rawQuery) {
      keyArgs.push(rawQuery);
    }

    // Disambiguate POST/PUT/PATCH calls to the same URL by hashing non-empty request body.
    if (method !== "GET" && method !== "HEAD" && (init?.body !== undefined || req.body !== null)) {
      const reqBody = new Uint8Array(await req.clone().arrayBuffer());
      if (reqBody.length > 0) {
        const hash16 = crypto.createHash("sha256").update(reqBody).digest("hex").slice(0, 16);
        keyArgs.push(`body:${hash16}`);
      }
    }

    const call: GatewayCall = {
      driver: "http",
      gateway: `http:${op}`,
      op,
      normalizedOp: op,
      keyArgs,
    };

    if (kv.isReplay()) {
      const respBytes = await kv.replayGateway(call);
      return decodeFetchResponse(respBytes);
    }

    // Record mode: execute real outbound fetch, record response/error, never block.
    let resp: Response;
    try {
      resp = await fetchFn(input, init);
    } catch (e) {
      kv.recordGateway({
        ...call,
        error: e instanceof Error ? e.message : String(e),
      });
      throw e;
    }

    try {
      const raw = new Uint8Array(await resp.clone().arrayBuffer());
      const body = raw.length > maxBodyBytes ? raw.subarray(0, maxBodyBytes) : raw;
      const headers: Record<string, string[]> = {};
      resp.headers.forEach((v, k) => {
        headers[k] = [v];
      });
      const rec: RecordedFetchResponse = {
        status_code: resp.status,
        status: `${resp.status} ${resp.statusText}`.trim(),
        headers,
      };
      if (body.length > 0) {
        const text = Buffer.from(body).toString("utf8").trim();
        if (text.length > 0 && (text[0] === "{" || text[0] === "[")) {
          try {
            rec.body_json = JSON.parse(text);
          } catch {
            rec.body = Buffer.from(body).toString("base64");
          }
        } else {
          rec.body = Buffer.from(body).toString("base64");
        }
      }
      kv.recordGateway({
        ...call,
        response: new Uint8Array(Buffer.from(JSON.stringify(rec), "utf8")),
      });
    } catch {
      // Best-effort capture; never interfere with live response.
    }

    return resp;
  };
}

function decodeFetchResponse(data: Uint8Array): Response {
  if (data.length === 0) {
    return new Response(null, { status: 200, statusText: "OK" });
  }
  try {
    const rec = JSON.parse(Buffer.from(data).toString("utf8")) as RecordedFetchResponse;
    const status = rec.status_code || 200;
    const headers = new Headers();
    if (rec.headers) {
      for (const [k, vals] of Object.entries(rec.headers)) {
        if (Array.isArray(vals)) {
          for (const v of vals) headers.append(k, v);
        } else if (typeof vals === "string") {
          headers.set(k, vals);
        }
      }
    }
    let body: string | Buffer | null = null;
    if (rec.body_json !== undefined) {
      body = JSON.stringify(rec.body_json);
    } else if (typeof rec.body === "string" && rec.body.length > 0) {
      body = Buffer.from(rec.body, "base64");
    }
    return new Response(body, { status, headers });
  } catch {
    return new Response(Buffer.from(data), { status: 200 });
  }
}
