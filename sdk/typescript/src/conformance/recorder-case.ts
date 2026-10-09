// Runs one SDK recorder case (spec/recorder/sdk/README.md):
//   node dist/conformance/recorder-case.js <case.json>

import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Recorder } from "../recorder.js";
import { recorderSdkSpecDir } from "../specdir.js";
import type { Gateway, Handler } from "../types.js";
import { ConformanceHandler } from "./handler.js";

interface Answers {
  clock?: string[];
  rand?: string[];
  gateway?: { response?: string; error?: string }[];
  config?: { value?: string; unset?: boolean }[];
}

interface Case {
  description?: string;
  open: { service: string; start: "genesis" | "snapshot"; snapshots: boolean };
  flags?: Record<string, string>;
  snapshot?: string;
  actions: {
    step?: { source: string; position: string; data: string; ops?: { op: string; gateway?: string }[] };
    answers?: Answers;
    flush?: { durable: boolean };
  }[];
}

const casePath = process.argv[2];
if (!casePath) {
  console.error("usage: recorder-case.js <case.json>");
  process.exit(2);
}
const c = JSON.parse(readFileSync(casePath, "utf8")) as Case;
const dir = mkdtempSync(join(tmpdir(), "kavach-case-"));
const resultPath = join(dir, "result.json");

// The answers of the step being run, served in order per kind.
let answers: Answers = {};
const next = <T>(kind: keyof Answers): T => {
  const q = answers[kind] as unknown[] | undefined;
  if (!q || q.length === 0) throw new Error(`case has no more ${kind} answers`);
  return q.shift() as T;
};

const gateways: Record<string, Gateway> = {};
for (const a of c.actions) {
  for (const op of a.step?.ops ?? []) {
    if (op.op === "gateway" && op.gateway) {
      gateways[op.gateway] = {
        async call() {
          const g = next<{ response?: string; error?: string }>("gateway");
          if (g.error !== undefined) throw new Error(g.error);
          return new Uint8Array(Buffer.from(g.response ?? "", "base64"));
        },
      };
    }
  }
}

const full = new ConformanceHandler();
// The case says whether the handler is a snapshotter; a snapshot start still needs restore().
const handler: Handler = c.open.snapshots
  ? full
  : {
      handle: (env, input) => full.handle(env, input),
      restore: (d) => full.restore(d),
      invariants: () => full.invariants(),
    };
const rec = await Recorder.start({
  service: c.open.service,
  handler,
  snapshot: c.open.start === "snapshot" ? new Uint8Array(Buffer.from(c.snapshot ?? "", "utf8")) : undefined,
  recorderCommand: ["python3", `${recorderSdkSpecDir()}/fake_recorder.py`, casePath, resultPath],
  required: true,
  gateways,
  clock: () => BigInt(next<string>("clock")),
  random: (n) => {
    const b = new Uint8Array(Buffer.from(next<string>("rand"), "base64"));
    if (b.length !== n) throw new Error(`case rand answer has ${b.length} bytes, handler asked for ${n}`);
    return b;
  },
  config: (key) => {
    const v = next<{ value?: string; unset?: boolean }>("config");
    void key;
    return v.unset ? undefined : { value: new Uint8Array(Buffer.from(v.value ?? "", "base64")), source: "test" };
  },
  flags: c.flags ? () => c.flags! : undefined,
});

for (const a of c.actions) {
  if (a.step) {
    answers = structuredClone(a.answers ?? {});
    await rec.step({
      source: a.step.source,
      position: a.step.position,
      data: new Uint8Array(Buffer.from(a.step.data, "base64")),
    });
  } else if (a.flush) {
    await rec.flush({ durable: a.flush.durable });
  }
}
await rec.close();

// The fake writes its verdict when its input closes; give it a moment.
let result: { pass: boolean; error?: string } | undefined;
for (let i = 0; i < 100 && !result; i++) {
  try {
    result = JSON.parse(readFileSync(resultPath, "utf8"));
  } catch {
    await new Promise((r) => setTimeout(r, 50));
  }
}
rmSync(dir, { recursive: true, force: true });
if (!result) {
  console.error(`${casePath}: the fake recorder wrote no result`);
  process.exit(1);
}
if (!result.pass) {
  console.error(`${casePath}: FAIL\n${result.error}`);
  process.exit(1);
}
console.log(`${casePath}: pass`);
process.exit(0);
