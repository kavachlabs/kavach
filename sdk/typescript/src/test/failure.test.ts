import assert from "node:assert/strict";
import { test } from "node:test";
import { HandlerError, Panic, mapFailure } from "../index.js";
import { checkInvariants } from "../failure.js";

test("a thrown Error is a panic named Name: message with the stack as detail", () => {
  const f = mapFailure(new TypeError("x is null"));
  assert.equal(f.kind, "panic");
  assert.equal(f.message, "TypeError: x is null");
  assert.match(f.detail, /TypeError: x is null/);
});

test("a thrown non-Error is a panic with String(x)", () => {
  assert.deepEqual(mapFailure("boom"), { kind: "panic", message: "boom", detail: "" });
  assert.equal(mapFailure(42).message, "42");
  assert.equal(mapFailure(null).message, "null");
  assert.equal(mapFailure({ toString: () => "custom" }).message, "custom");
  assert.equal(
    mapFailure({
      toString() {
        throw new Error("no");
      },
    }).kind,
    "panic",
  );
});

test("Panic carries its message exactly", () => {
  const f = mapFailure(new Panic("exactly this"));
  assert.equal(f.kind, "panic");
  assert.equal(f.message, "exactly this");
});

test("HandlerError is an error with its message exactly", () => {
  assert.deepEqual(mapFailure(new HandlerError("no funds")), { kind: "error", message: "no funds", detail: "" });
});

test("invariants are checked in order and the first failure is reported", () => {
  const calls: string[] = [];
  const h = {
    handle() {},
    invariants: () => [
      { name: "a", check: () => void calls.push("a") },
      {
        name: "b",
        check: () => {
          calls.push("b");
          throw new Error("broken");
        },
      },
      { name: "c", check: () => void calls.push("c") },
    ],
  };
  const f = checkInvariants(h);
  assert.equal(f?.kind, "invariant");
  assert.equal(f?.message, "b");
  assert.match(f!.detail, /broken/);
  assert.deepEqual(calls, ["a", "b"]);
});
