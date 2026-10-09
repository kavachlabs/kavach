import { join } from "node:path";
import { fileURLToPath } from "node:url";

/**
 * Where the conformance contract lives (`host/`, `recorder/sdk/`):
 * KAVACH_SPEC_DIR, else the repository's own `spec/`.
 */
export const SPEC_DIR = process.env.KAVACH_SPEC_DIR ?? fileURLToPath(new URL("../../../spec", import.meta.url));

export const hostSpecDir = (): string => join(SPEC_DIR, "host");
export const recorderSdkSpecDir = (): string => join(SPEC_DIR, "recorder", "sdk");
