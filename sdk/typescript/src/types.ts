/** One event consumed by a handler. */
export interface Input {
  /** Where the event came from, e.g. "kafka:wallet-events". */
  source: string;
  /** Its position in that source, e.g. "3:1042". Opaque to Kavach. */
  position: string;
  /** The event exactly as the handler received it. */
  data: Uint8Array;
}

/** Whether a resource is on another host (remote) or on the host itself (local). SPEC §4.4. */
export type Scope = "remote" | "local";

/** One effect a handler requested. */
export interface Output {
  sink: string;
  data: Uint8Array;
  scope: Scope;
}

export interface EmitOptions {
  /** The effect targets a resource of the host the process runs on (SPEC §4.4, §6.3). Default false. */
  local?: boolean;
}

/**
 * Passed to a handler for each input. Everything nondeterministic a handler
 * does must go through it: reading time, randomness, config and external
 * systems, and producing effects.
 */
export interface Env {
  /** Reads the injected clock (processing time). */
  now(): Date;
  /** Reads the injected clock in nanoseconds since the Unix epoch. */
  nowNanos(): bigint;
  /** Reads exactly `n` (at least 1) random bytes from the injected source. */
  random(n: number): Uint8Array;
  /**
   * Queries an external system through a registered gateway. Rejects with
   * {@link GatewayError} when the connection reports a failure.
   *
   * The query's position in the journal is fixed when `query` is called, not
   * when it resolves.
   */
  query(gateway: string, request: Uint8Array): Promise<Uint8Array>;
  /** Reads a config value (feature flag, limit). `undefined` when not set. */
  config(key: string): Uint8Array | undefined;
  /** Requests an effect. Delivered only after the step succeeds; never during replay. */
  emit(sink: string, data: Uint8Array, options?: EmitOptions): void;
}

/** A named property of handler state that must hold after every step. `check` throws on failure. */
export interface Invariant {
  name: string;
  check(): void;
}

/**
 * Folds inputs into state. It must be deterministic given the Env. Steps run
 * one at a time.
 */
export interface Handler {
  handle(env: Env, input: Input): void | Promise<void>;
  /** The handler's state in its own encoding. Makes the handler segmentable. */
  snapshot?(): Uint8Array;
  /** Restores a state taken by `snapshot`. */
  restore?(data: Uint8Array): void;
  /** Invariants checked, in order, after every step that ended without failure. */
  invariants?(): Invariant[];
}

/** A connection to an external system, registered by name on the recorder or host. */
export interface Gateway {
  /** Makes the call. Reject (or throw) to report a failure; the message is recorded as the error. */
  call(request: Uint8Array): Promise<Uint8Array | string>;
  /** Default "remote". It must not change between builds (SPEC §4.7). */
  scope?: Scope;
}

export type FailureKind = "panic" | "error" | "invariant";

export interface Failure {
  kind: FailureKind;
  message: string;
  detail: string;
}
