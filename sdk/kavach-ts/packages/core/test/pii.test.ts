import assert from "node:assert/strict";
import crypto from "node:crypto";
import test from "node:test";
import { scrubBytes, scrubHeaders, scrubString } from "@kavachlabs/core";

test("PII scrubber preserves exact JSON key order, numbers, and matches Go HMAC-SHA256 tokens", () => {
  const key = Buffer.from("kavach-pii-v1:events-processor", "utf8");
  const raw = Buffer.from(
    '{"z_first":123,"transaction_id":"tx_1001","customer_email":"alice@vertex-ai.example","amount_cents":500,"phone":"+14155550199","ssn":"123-45-6789","api_key":"sk_live_secret99999999"}',
    "utf8",
  );
  const out = scrubBytes("standard", key, raw);
  const outStr = Buffer.from(out).toString("utf8");

  // Raw PII must be completely absent
  assert.equal(outStr.includes("alice@vertex-ai.example"), false);
  assert.equal(outStr.includes("+14155550199"), false);
  assert.equal(outStr.includes("123-45-6789"), false);
  assert.equal(outStr.includes("sk_live_secret99999999"), false);

  // Allowlisted keys & numbers preserved verbatim
  assert.equal(outStr.includes('"transaction_id":"tx_1001"'), true);
  assert.equal(outStr.includes('"z_first":123'), true);
  assert.equal(outStr.includes('"amount_cents":500'), true);

  // Exact key order preserved!
  const keys = Object.keys(JSON.parse(outStr));
  assert.deepEqual(keys, [
    "z_first",
    "transaction_id",
    "customer_email",
    "amount_cents",
    "phone",
    "ssn",
    "api_key",
  ]);

  // Verify byte-for-byte parity with Go HMAC-SHA256 formula
  const expectedEmailHex = crypto
    .createHmac("sha256", key)
    .update("alice@vertex-ai.example")
    .digest()
    .subarray(0, 8)
    .toString("hex");
  const parsed = JSON.parse(outStr) as Record<string, unknown>;
  assert.equal(parsed.customer_email, `u_${expectedEmailHex}@redacted.invalid`);
  assert.match(String(parsed.phone), /^\+155501\d{5}$/);
  assert.match(String(parsed.ssn), /^900-00-\d{4}$/);
  assert.match(String(parsed.api_key), /^tok_[0-9a-f]{16}$/);

  // Deterministic across invocations
  assert.deepEqual(scrubBytes("standard", key, raw), out);
});

test("PII scrubber handles inline text, headers, and strict mode", () => {
  const key = Buffer.from("test-key", "utf8");
  const inline = scrubString(
    "standard",
    key,
    "note",
    "Contact bob@acme.org with Bearer supersecrettoken123",
  );
  assert.equal(inline.includes("bob@acme.org"), false);
  assert.equal(inline.includes("supersecrettoken123"), false);
  assert.match(inline, /u_[0-9a-f]{16}@redacted\.invalid/);
  assert.match(inline, /Bearer tok_[0-9a-f]{16}/);

  const hdrs = scrubHeaders("standard", key, {
    Authorization: "Bearer mytoken987654321",
    "Content-Type": "application/json",
  });
  assert.equal(hdrs["Content-Type"], "application/json");
  assert.equal(hdrs.Authorization.includes("mytoken987654321"), false);

  const strict = Buffer.from(
    scrubBytes("strict", key, Buffer.from('{"status":"active","custom_note":"hello world"}')),
  ).toString("utf8");
  const sp = JSON.parse(strict) as Record<string, string>;
  assert.equal(sp.status, "active"); // allowlisted
  assert.match(sp.custom_note, /^tok_[0-9a-f]{16}$/);
});
