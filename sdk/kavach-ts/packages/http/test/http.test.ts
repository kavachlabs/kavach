import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { FrameDecoder, RecordType, Ring, StepTable, decodeRecord, init } from "@kavach/core";
import { wrapFetch, wrapHandler } from "@kavach/http";

test("@kavach/http wrapHandler records Node http.RequestListener and replays 100% offline with zero TCP socket", async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "kavach-ts-http-"));
  const ringPath = path.join(dir, "http.shm");
  try {
    const kvRec = init({ service: "http-svc", ringPath, pii: "standard", mode: "record" });
    const handler = (req: http.IncomingMessage, res: http.ServerResponse) => {
      const chunks: Buffer[] = [];
      req.on("data", (c) => chunks.push(Buffer.from(c)));
      req.on("end", () => {
        const body = JSON.parse(Buffer.concat(chunks).toString("utf8")) as { email: string; amount: number };
        res.writeHead(201, { "content-type": "application/json" });
        res.end(JSON.stringify({ ok: true, email: body.email, total: body.amount * 2 }));
      });
    };

    const wrapped = wrapHandler(kvRec, handler);
    const srv = http.createServer(wrapped);
    await new Promise<void>((resolve) => srv.listen(0, "127.0.0.1", resolve));
    const addr = srv.address();
    assert.ok(addr && typeof addr === "object");

    const liveResp = await fetch(`http://127.0.0.1:${addr.port}/orders`, {
      method: "POST",
      headers: { "content-type": "application/json", authorization: "Bearer secret_token_12345" },
      body: JSON.stringify({ email: "buyer@acme.example", amount: 25 }),
    });
    assert.equal(liveResp.status, 201);
    assert.deepEqual(await liveResp.json(), { ok: true, email: "buyer@acme.example", total: 50 });

    await new Promise<void>((resolve) => srv.close(() => resolve()));
    await kvRec.close();

    // Inspect recorded ring & replay completely offline (no server started!)
    const reader = Ring.open(ringPath);
    const bytes = reader.pollOnce();
    assert.ok(bytes);
    const dec = new FrameDecoder();
    dec.feed(bytes);
    const drained = dec.finishEOF();
    const inputRec = decodeRecord(drained.frames[1].payload);
    assert.equal(inputRec.type, RecordType.Input);
    assert.equal(inputRec.source, "http:POST /orders");
    const recText = Buffer.from(inputRec.data!).toString("utf8");
    assert.equal(recText.includes("buyer@acme.example"), false);
    assert.equal(recText.includes("secret_token_12345"), false);
    reader.close();

    const kvRep = init({ service: "http-svc", mode: "replay" });
    const wrappedRep = wrapHandler(kvRep, handler);
    const table = new StepTable(1n);
    const { error } = await kvRep.runStepWithTable(
      { source: inputRec.source!, position: inputRec.position, data: inputRec.data! },
      table,
      () => wrappedRep.runStep({ source: inputRec.source!, position: inputRec.position, data: inputRec.data! }),
    );
    assert.equal(error, undefined);
    const outs = table.emittedOutputs();
    assert.equal(outs.length, 1);
    assert.equal(outs[0].sink, "http:response");
    const respEnv = JSON.parse(Buffer.from(outs[0].data).toString("utf8")) as {
      status: number;
      body_json: { ok: boolean; total: number };
    };
    assert.equal(respEnv.status, 201);
    assert.equal(respEnv.body_json.ok, true);
    assert.equal(respEnv.body_json.total, 50);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("@kavach/http wrapHandler supports Hono/Fetch apps and Fastify-style inject offline", async () => {
  const kv = init({ service: "fetch-app", mode: "replay" });

  // Hono / Fetch-standard app
  const honoLike = {
    async fetch(req: Request): Promise<Response> {
      const data = (await req.json()) as { name: string };
      return new Response(JSON.stringify({ hello: data.name }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    },
  };
  const wrappedHono = wrapHandler(kv, honoLike);
  const t1 = new StepTable(1n);
  await kv.runStepWithTable(
    {
      source: "http:POST /greet",
      data: Buffer.from(JSON.stringify({ method: "POST", path: "/greet", body_json: { name: "kavach" } })),
    },
    t1,
    (sc) => wrappedHono.runStep(sc.input()),
  );
  const out1 = JSON.parse(Buffer.from(t1.emittedOutputs()[0].data).toString("utf8")) as {
    status: number;
    body_json: { hello: string };
  };
  assert.equal(out1.status, 200);
  assert.equal(out1.body_json.hello, "kavach");

  // Fastify-style app with .inject()
  const fastifyLike = {
    async inject(opts: { method: string; url: string }) {
      return {
        statusCode: 200,
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ fastify: `${opts.method} ${opts.url}` }),
      };
    },
  };
  const wrappedFastify = wrapHandler(kv, fastifyLike);
  const t2 = new StepTable(2n);
  await kv.runStepWithTable(
    { source: "http:GET /health", data: Buffer.from("{}") },
    t2,
    (sc) => wrappedFastify.runStep(sc.input()),
  );
  const out2 = JSON.parse(Buffer.from(t2.emittedOutputs()[0].data).toString("utf8")) as {
    body_json: { fastify: string };
  };
  assert.equal(out2.body_json.fastify, "GET /health");
});

test("@kavach/http wrapFetch captures outbound HTTP Gateway and replays offline", async () => {
  const kvRep = init({ service: "client-svc", mode: "replay" });
  const recordedBody = JSON.stringify({
    status_code: 200,
    headers: { "content-type": ["application/json"] },
    body_json: { rate: 1.25 },
  });
  const table = new StepTable(1n, [
    {
      driver: "http",
      gateway: "http:GET fx.internal/rates",
      op: "GET fx.internal/rates",
      normalizedOp: "GET fx.internal/rates",
      keyArgs: ["fx.internal/rates", "pair=USD_EUR"],
      response: Buffer.from(recordedBody),
    },
  ]);

  const safeFetch = wrapFetch(kvRep, () => {
    throw new Error("live fetch must never be called in replay!");
  });

  const { result, error } = await kvRep.runStepWithTable(
    { source: "test", data: Buffer.from("{}") },
    table,
    async () => {
      const resp = await safeFetch("https://fx.internal/rates?pair=USD_EUR");
      assert.equal(resp.status, 200);
      return resp.json();
    },
  );
  assert.equal(error, undefined);
  assert.deepEqual(result, { rate: 1.25 });
});
