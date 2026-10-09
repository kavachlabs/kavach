import { HandlerError, Panic } from "./errors.js";
import type { Failure, Handler } from "./types.js";

function safeString(x: unknown): string {
  try {
    return String(x);
  } catch {
    return Object.prototype.toString.call(x);
  }
}

/**
 * The §4.5 mapping, used both when recording and when hosting a replay so a
 * recorded failure and its replay compare equal:
 *
 * - `new HandlerError(m)`: `error`, message exactly `m`.
 * - `new Panic(m)`: `panic`, message exactly `m`, the stack as detail.
 * - any other `Error`: `panic`, message `${name}: ${message}`, the stack as detail.
 * - anything else thrown: `panic`, message `String(x)`.
 */
export function mapFailure(e: unknown): Failure {
  if (e instanceof HandlerError) return { kind: "error", message: e.message, detail: "" };
  if (e instanceof Panic) return { kind: "panic", message: e.message, detail: e.stack ?? "" };
  if (e instanceof Error) {
    return { kind: "panic", message: `${e.name}: ${e.message}`, detail: e.stack ?? "" };
  }
  return { kind: "panic", message: safeString(e), detail: "" };
}

/** Checks the handler's invariants in order; returns the first failure. */
export function checkInvariants(handler: Handler): Failure | undefined {
  const invs = handler.invariants?.() ?? [];
  for (const inv of invs) {
    try {
      inv.check();
    } catch (e) {
      const detail = e instanceof Error ? `${e.name}: ${e.message}` : safeString(e);
      return { kind: "invariant", message: inv.name, detail };
    }
  }
  return undefined;
}

export function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : safeString(e);
}
