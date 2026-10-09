// The conformance handler (SPEC §9.6).

import { GatewayError, HandlerError, Panic } from "../errors.js";
import type { Env, Handler, Input, Invariant } from "../types.js";

const enc = new TextEncoder();
const dec = new TextDecoder();
const utf8 = (s: string) => enc.encode(s);

/** SPEC §9.6: escape `"`, `\\`, \n, \r, \t and other C0 controls (lowercase hex); nothing else. */
function jsonString(s: string): string {
  let out = '"';
  for (const ch of s) {
    const c = ch.codePointAt(0)!;
    if (ch === '"') out += '\\"';
    else if (ch === "\\") out += "\\\\";
    else if (ch === "\n") out += "\\n";
    else if (ch === "\r") out += "\\r";
    else if (ch === "\t") out += "\\t";
    else if (c < 0x20) out += "\\u" + c.toString(16).padStart(4, "0");
    else out += ch;
  }
  return out + '"';
}

interface Op {
  op: string;
  [k: string]: unknown;
}

export class ConformanceHandler implements Handler {
  count = 0;

  async handle(env: Env, input: Input): Promise<void> {
    const ops = JSON.parse(dec.decode(input.data)) as Op[];
    for (const op of ops) {
      switch (op.op) {
        case "clock":
          env.emit("trace", utf8(`{"clock":"${env.nowNanos()}"}`));
          break;
        case "rand":
          env.emit("trace", env.random(Number(op.n)));
          break;
        case "gateway":
          try {
            env.emit("trace", await env.query(String(op.gateway), utf8(String(op.request))));
          } catch (e) {
            if (!(e instanceof GatewayError)) throw e;
            env.emit("trace", utf8(`{"error":${jsonString(e.error)}}`));
          }
          break;
        case "config": {
          const v = env.config(String(op.key));
          env.emit("trace", v ?? utf8('{"unset":true}'));
          break;
        }
        case "getenv": {
          const v = process.env[String(op.name)];
          env.emit("trace", v === undefined ? utf8('{"unset":true}') : utf8(v));
          break;
        }
        case "emit":
          env.emit(String(op.sink), utf8(String(op.data)));
          break;
        case "panic":
          throw new Panic(String(op.message));
        case "error":
          throw new HandlerError(String(op.message));
        case "print":
          console.log(String(op.text));
          break;
        case "count":
          this.count += Number(op.n); // takes effect at once, even if the step later fails
          break;
        default:
          throw new HandlerError(`unknown op ${String(op.op)}`);
      }
    }
    this.count += 1; // only this 1 depends on the step not failing
  }

  snapshot(): Uint8Array {
    return utf8(String(this.count));
  }

  restore(data: Uint8Array): void {
    this.count = Number.parseInt(dec.decode(data), 10);
  }

  invariants(): Invariant[] {
    return [
      {
        name: "below_limit",
        check: () => {
          if (this.count >= 1000) throw new Error(`count is ${this.count}`);
        },
      },
    ];
  }
}
