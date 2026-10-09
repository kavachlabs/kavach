import { join } from "node:path";

/**
 * Where the conformance contract lives (`host/`, `recorder/sdk/`). Until it is
 * merged into the repo's own `spec/`, it is read from the main checkout.
 * Override with KAVACH_SPEC_DIR.
 */
export const SPEC_DIR = process.env.KAVACH_SPEC_DIR ?? "/Users/koustav/code/kavach-labs/kavach/spec";

export const hostSpecDir = (): string => join(SPEC_DIR, "host");
export const recorderSdkSpecDir = (): string => join(SPEC_DIR, "recorder", "sdk");
