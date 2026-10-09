// Conformance host entry point: `node dist/conformance/host.js kavach-host`.

import { maybeHost } from "../host.js";
import { ConformanceHandler } from "./handler.js";

await maybeHost(() => new ConformanceHandler());
console.error("kavach conformance host: run with kavach-host as the last argument");
process.exit(2);
