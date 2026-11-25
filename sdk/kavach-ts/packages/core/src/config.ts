import fs from "node:fs";
import path from "node:path";
import type { Readable, Writable } from "node:stream";

export const VERSION = "0.2.1";
export const HOST_COMMAND = "kavach-host";
export const DEFAULT_INVARIANTS_PATH = ".kavach/invariants.yaml";

export type ConfigMode = "auto" | "off" | "record" | "replay";
export type Mode = "off" | "record" | "replay";
export type ScrubMode = "off" | "standard" | "strict";

export interface KavachConfig {
  /** Service name recorded in KVRING02 Open frames. Defaults to KAVACH_SERVICE or script basename. */
  service?: string;
  /** Path to KVRING02 shared-memory ring file (or stream fallback). Also honors KAVACH_RING_PATH. */
  ringPath?: string;
  /** Circular ring capacity in bytes (power of two >= 65536). Defaults to 8 MiB. */
  ringCapacity?: number;
  /** Deterministic PII scrubbing mode: 'off' | 'standard' | 'strict'. Also honors KAVACH_PII. */
  pii?: ScrubMode;
  /** HMAC-SHA256 key bytes/string for deterministic PII tokenization. Also honors KAVACH_PII_KEY. */
  piiKey?: Uint8Array | string;
  /** Path to declarative invariants YAML. Defaults to .kavach/invariants.yaml if present. */
  invariantsPath?: string;
  /** Runtime mode override: 'auto' | 'off' | 'record' | 'replay'. */
  mode?: ConfigMode;
  /** Listen address/port for serveOrReplay when serving an HTTP handler outside Replay mode. */
  listenAddr?: string;
  /** Custom stdin stream for kavach-host protocol loop (tests). */
  stdin?: Readable;
  /** Custom stdout stream for kavach-host protocol loop (tests). */
  stdout?: Writable;
  /** Callback invoked when replay driver sends a 'reset' command between runs. */
  onReset?: () => void | Promise<void>;
  /** Override process.argv for mode detection (tests). */
  argv?: string[];
  /** Override process.env for config resolution (tests). */
  env?: Record<string, string | undefined>;
}

export interface ResolvedConfig {
  service: string;
  ringPath: string;
  ringCapacity: number;
  pii: ScrubMode;
  piiKey: Uint8Array;
  invariantsPath: string;
  mode: Mode;
  listenAddr: string;
  stdin?: Readable;
  stdout?: Writable;
  onReset?: () => void | Promise<void>;
}

export function parseScrubMode(s: string | undefined): { mode: ScrubMode; ok: boolean } {
  const norm = (s ?? "").trim().toLowerCase();
  switch (norm) {
    case "":
    case "off":
    case "none":
    case "false":
      return { mode: "off", ok: true };
    case "standard":
    case "default":
    case "true":
      return { mode: "standard", ok: true };
    case "strict":
      return { mode: "strict", ok: true };
    default:
      return { mode: "off", ok: false };
  }
}

export function detectReplayMode(
  argv: string[] = process.argv,
  env: Record<string, string | undefined> = process.env,
): boolean {
  const hostEnv = (env.KAVACH_HOST ?? "").trim().toLowerCase();
  if (hostEnv === "1" || hostEnv === "true") {
    return true;
  }
  if (argv.length > 1 && argv[argv.length - 1] === HOST_COMMAND) {
    return true;
  }
  return false;
}

export function resolveConfig(cfg: KavachConfig = {}): ResolvedConfig {
  const argv = cfg.argv ?? process.argv;
  const env = cfg.env ?? process.env;

  let service = (cfg.service ?? "").trim();
  if (!service) {
    service = (env.KAVACH_SERVICE ?? "").trim();
  }
  if (!service) {
    const entry = argv[1] || argv[0] || "service";
    service = path.basename(entry, path.extname(entry)) || "service";
  }

  let ringPath = (cfg.ringPath ?? "").trim();
  if (!ringPath) {
    ringPath = (env.KAVACH_RING_PATH ?? "").trim();
  }

  let pii: ScrubMode = cfg.pii ?? "off";
  if (cfg.pii === undefined && env.KAVACH_PII) {
    const parsed = parseScrubMode(env.KAVACH_PII);
    if (parsed.ok) {
      pii = parsed.mode;
    }
  }

  let piiKey: Uint8Array;
  if (cfg.piiKey !== undefined && cfg.piiKey.length > 0) {
    piiKey = typeof cfg.piiKey === "string" ? Buffer.from(cfg.piiKey, "utf8") : new Uint8Array(cfg.piiKey);
  } else if (env.KAVACH_PII_KEY) {
    piiKey = Buffer.from(env.KAVACH_PII_KEY, "utf8");
  } else {
    piiKey = Buffer.from(`kavach-pii-v1:${service}`, "utf8");
  }

  let invariantsPath = (cfg.invariantsPath ?? "").trim();
  if (!invariantsPath && env.KAVACH_INVARIANTS) {
    invariantsPath = env.KAVACH_INVARIANTS.trim();
  }
  if (!invariantsPath) {
    try {
      if (fs.statSync(DEFAULT_INVARIANTS_PATH).isFile()) {
        invariantsPath = DEFAULT_INVARIANTS_PATH;
      }
    } catch {
      // default invariants file does not exist
    }
  }

  const listenAddr = (cfg.listenAddr ?? "").trim() || (env.KAVACH_ADDR ?? "").trim() || ":8080";

  const requestedMode: ConfigMode = cfg.mode ?? "auto";
  let mode: Mode;
  if (requestedMode === "auto") {
    if (detectReplayMode(argv, env)) {
      mode = "replay";
    } else {
      const envMode = (env.KAVACH_MODE ?? "").trim().toLowerCase();
      if (envMode === "replay") {
        mode = "replay";
      } else if (envMode === "off") {
        mode = "off";
      } else if (envMode === "record" || ringPath !== "") {
        mode = "record";
      } else {
        mode = "off";
      }
    }
  } else {
    mode = requestedMode;
  }

  return {
    service,
    ringPath,
    ringCapacity: cfg.ringCapacity ?? 0,
    pii,
    piiKey,
    invariantsPath,
    mode,
    listenAddr,
    stdin: cfg.stdin,
    stdout: cfg.stdout,
    onReset: cfg.onReset,
  };
}
