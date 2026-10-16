// The console's UI session (session.mjs) as a Kavach service.
//
//   node main.mjs --in sessions/panes.jsonl [--fixtures DIR] [--facts FILE]
//
// feeds a session, one input per line, through the flight recorder
// (kavach-recorder, found through $KAVACH_RECORDER or PATH; --facts passes
// it --test-facts). An input the console gets wrong breaks an invariant, and
// the recorder writes a fixture. As `main.mjs kavach-host` it serves the
// kavach CLI instead:
//
//   kavach replay ../testdata/kavach/panes-reload.kavach --bin "node main.mjs"

import { readFileSync } from "node:fs";
import { basename } from "node:path";
import { Recorder, maybeHost } from "../../../../../sdk/typescript/dist/index.js";
import { Session } from "./session.mjs";

await maybeHost(() => new Session());

const args = process.argv.slice(2);
const value = (name) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : undefined;
};
const file = value("--in");
if (!file) {
  console.error("usage: main.mjs --in FILE [--fixtures DIR] [--facts FILE]");
  process.exit(2);
}
const facts = value("--facts");

const recorder = await Recorder.start({
  service: "console-ui",
  handler: new Session(),
  dir: value("--fixtures") ?? "kavach",
  required: true,
  recorderCommand: facts ? [process.env.KAVACH_RECORDER || "kavach-recorder", "--test-facts", facts] : undefined,
  log: (m) => console.error(m),
  deliver: (outputs) => {
    for (const o of outputs) console.log(`${o.sink.padEnd(13)} ${new TextDecoder().decode(o.data)}`);
  },
});

let status = 0;
for (const [i, line] of readFileSync(file, "utf8").split("\n").entries()) {
  if (!line.trim()) continue;
  console.log(`> ${line}`);
  const r = await recorder.step({ source: `session:${basename(file)}`, position: String(i + 1), data: new TextEncoder().encode(line) });
  if (r.outcome !== "ok") {
    console.error(`line ${i + 1}: ${r.outcome}: ${r.message}`);
    status = 1;
    break;
  }
}
await recorder.close();
process.exit(status);
