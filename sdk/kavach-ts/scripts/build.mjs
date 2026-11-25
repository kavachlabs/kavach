import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const packages = ["core", "http", "pg", "ioredis", "kafkajs"];

// Ensure local workspace symlinks exist under node_modules/@kavach/* so
// Node 22 and tsc resolve @kavach/* cleanly with zero network access.
const scopeDir = path.join(root, "node_modules", "@kavach");
fs.mkdirSync(scopeDir, { recursive: true });
for (const pkg of packages) {
  const linkPath = path.join(scopeDir, pkg);
  const target = path.relative(scopeDir, path.join(root, "packages", pkg));
  try {
    const stat = fs.lstatSync(linkPath);
    if (stat.isSymbolicLink() && fs.readlinkSync(linkPath) === target) continue;
    fs.rmSync(linkPath, { recursive: true, force: true });
  } catch {
    // link does not exist yet
  }
  fs.symlinkSync(target, linkPath, "dir");
}

// Find tsc (local workspace, sibling sdk/typescript, or PATH)
const tscCandidates = [
  path.join(root, "node_modules", ".bin", "tsc"),
  path.resolve(root, "..", "typescript", "node_modules", ".bin", "tsc"),
  "tsc",
];
let tscBin = "";
for (const c of tscCandidates) {
  if (c === "tsc") {
    const chk = spawnSync("which", ["tsc"], { encoding: "utf8" });
    if (chk.status === 0 && chk.stdout.trim()) {
      tscBin = chk.stdout.trim();
      break;
    }
  } else if (fs.existsSync(c)) {
    tscBin = c;
    break;
  }
}

if (!tscBin) {
  console.error("build.mjs: could not locate tsc");
  process.exit(1);
}

// Ensure @types/node is discoverable by tsc if installed in sibling sdk/typescript
const typesNodeLink = path.join(root, "node_modules", "@types", "node");
const siblingTypesNode = path.resolve(root, "..", "typescript", "node_modules", "@types", "node");
const siblingUndiciTypes = path.resolve(root, "..", "typescript", "node_modules", "undici-types");
if (!fs.existsSync(typesNodeLink) && fs.existsSync(siblingTypesNode)) {
  fs.mkdirSync(path.dirname(typesNodeLink), { recursive: true });
  fs.symlinkSync(path.relative(path.dirname(typesNodeLink), siblingTypesNode), typesNodeLink, "dir");
  const undiciLink = path.join(root, "node_modules", "undici-types");
  if (!fs.existsSync(undiciLink) && fs.existsSync(siblingUndiciTypes)) {
    fs.symlinkSync(path.relative(path.dirname(undiciLink), siblingUndiciTypes), undiciLink, "dir");
  }
}

const noEmit = process.argv.includes("--noEmit");
const args = noEmit ? ["-b", "tsconfig.json", "--dry"] : ["-b", "tsconfig.json"];
// For true --noEmit typecheck across all packages & tests:
if (noEmit) {
  for (const pkg of packages) {
    const res = spawnSync(tscBin, ["-p", path.join(root, "packages", pkg, "tsconfig.json"), "--noEmit"], {
      cwd: root,
      stdio: "inherit",
    });
    if (res.status !== 0) process.exit(res.status ?? 1);
  }
  process.exit(0);
}

const res = spawnSync(tscBin, args, { cwd: root, stdio: "inherit" });
process.exit(res.status ?? 1);
