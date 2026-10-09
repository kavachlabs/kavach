// Runs the shared conformance suites (SPEC §9.6, §10.6) against this SDK.
// They are Python 3 scripts; the tests are skipped if python3 or the spec directory is missing.

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { hostSpecDir, recorderSdkSpecDir } from "../specdir.js";

const python = spawnSync("python3", ["--version"]).status === 0;
const skip = !python ? "python3 not found" : !existsSync(hostSpecDir()) ? `no spec at ${hostSpecDir()} (set KAVACH_SPEC_DIR)` : false;

const dist = fileURLToPath(new URL("../", import.meta.url));

test("host transcripts (spec/host)", { skip }, () => {
  const r = spawnSync("python3", [join(hostSpecDir(), "run.py"), "--host", `${process.execPath} ${join(dist, "conformance/host.js")}`], {
    encoding: "utf8",
  });
  assert.equal(r.status, 0, r.stdout + r.stderr);
  assert.match(r.stdout, /(\d+)\/\1 transcripts passed/);
});

const cases = skip ? [] : readdirSync(recorderSdkSpecDir()).filter((f) => f.endsWith(".json"));
for (const name of cases) {
  test(`recorder case ${name}`, { skip }, () => {
    const r = spawnSync(process.execPath, [join(dist, "conformance/recorder-case.js"), join(recorderSdkSpecDir(), name)], {
      encoding: "utf8",
    });
    assert.equal(r.status, 0, r.stdout + r.stderr);
  });
}
