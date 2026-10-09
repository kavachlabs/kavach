// The ledger demo service.
//
//   node dist/examples/ledger/main.js --in events.jsonl [--fixed] [--fixtures dir]
//
// With the "amount": null event in the input the buggy handler throws; the
// flight recorder writes a fixture before the process stops. `--fixed` runs the
// handler with the fix. Behaviour is chosen by command-line flags, never by
// environment variables: environment variables are served from the journal on
// replay, so the host command a replay uses (`node main.js [--fixed]`) carries
// the flag itself.
//
// As `main.js [--fixed] kavach-host` it serves the host protocol instead.

import { readFileSync } from "node:fs";
import { basename } from "node:path";
import { Recorder, maybeHost } from "@kavachlabs/kavach";
import { Ledger } from "./ledger.js";

const args = process.argv.slice(2);
const flag = (name: string) => args.includes(name);
const value = (name: string) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : undefined;
};
const fixNullAmount = flag("--fixed");

// Lets the kavach CLI use this program as a replay host.
await maybeHost(() => new Ledger({ fixNullAmount }));

const file = value("--in");
if (!file) {
  console.error("usage: main.js --in FILE [--fixed] [--fixtures DIR]");
  process.exit(2);
}

const recorder = await Recorder.start({
  service: "ledger",
  handler: new Ledger({ fixNullAmount }),
  dir: value("--fixtures") ?? "fixtures",
  deliver: (outputs) => {
    for (const o of outputs) console.log(`${o.sink.padEnd(18)} ${Buffer.from(o.data).toString()}`);
  },
});

let status = 0;
const lines = readFileSync(file, "utf8").split("\n");
for (const [i, line] of lines.entries()) {
  if (!line) continue;
  const result = await recorder.step({
    source: `file:${basename(file)}`,
    position: String(i + 1),
    data: new TextEncoder().encode(line),
  });
  if (result.outcome === "panic") {
    // A panic stops the service (the fixture is on its way by then).
    console.error(`ledger: ${result.message}`);
    status = 1;
    break;
  }
  if (result.outcome !== "ok") console.error(`ledger: line ${i + 1}: ${result.outcome}: ${result.message}`);
}
await recorder.close();
process.exit(status);
