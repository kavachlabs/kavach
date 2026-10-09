// A tiny recorder for tests: node test-recorder.js <out.jsonl> <mode>
// Appends each frame it reads to <out.jsonl> as one decoded JSON line.
// Modes: normal (answers ready/durable/closed), silent (never answers durable),
//        die (exits after the first frame), fatal (reports a fatal error after open).

import { appendFileSync } from "node:fs";
import { decode, parseFrames } from "./frames.js";

const out = process.argv[2]!;
const mode = process.argv[3] ?? "normal";
const say = (m: object) => process.stdout.write(JSON.stringify(m) + "\n");

let buf = Buffer.alloc(0);
process.stdin.on("data", (chunk: Buffer) => {
  buf = Buffer.concat([buf, chunk]);
  // Consume whole frames only.
  let end = 0;
  for (let i = 0; i < buf.length; ) {
    let len = 0;
    let shift = 0;
    let j = i;
    let ok = false;
    while (j < buf.length) {
      const c = buf[j++]!;
      len |= (c & 0x7f) << shift;
      shift += 7;
      if (c < 0x80) {
        ok = true;
        break;
      }
    }
    if (!ok || j + len > buf.length) break;
    i = j + len;
    end = i;
  }
  const frames = parseFrames(buf.subarray(0, end));
  buf = buf.subarray(end);
  for (const f of frames) {
    const d = decode(f);
    appendFileSync(out, JSON.stringify(d) + "\n");
    if (mode === "die") process.exit(1);
    if (d.frame === "open") {
      say({ t: "ready", protocol: 1, recorder: "test-recorder", run: "t", file: "/dev/null" });
      if (mode === "fatal") say({ t: "error", message: "disk full", fatal: true });
    } else if (d.frame === "flush" && d.durable && mode !== "silent") say({ t: "durable", seq: "0" });
    else if (d.frame === "close") {
      say({ t: "closed" });
      process.exit(0);
    }
  }
});
process.stdin.on("end", () => process.exit(0));
