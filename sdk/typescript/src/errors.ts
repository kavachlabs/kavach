/**
 * Throw `new Panic(message)` to fail a step as a `panic` whose marker message
 * is exactly `message` (SPEC §4.5).
 */
export class Panic extends Error {
  constructor(message: string) {
    super(message);
    this.name = "Panic";
  }
}

/**
 * Throw `new HandlerError(message)` to fail a step as an `error` whose marker
 * message is exactly `message`: the equivalent of returning an error.
 */
export class HandlerError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "HandlerError";
  }
}

/** Rejection of `env.query` when the gateway's connection reported a failure. */
export class GatewayError extends Error {
  /** The failure the connection reported, e.g. "timeout". Recorded verbatim. */
  readonly error: string;
  constructor(error: string) {
    super(error);
    this.name = "GatewayError";
    this.error = error;
  }
}

/**
 * Thrown inside a handler when the driver aborted the step (SPEC §9.4).
 * Handler code must not swallow it. If it does, the host still reports the
 * step as `aborted`.
 */
export class KavachAbort extends Error {
  readonly detail: string;
  constructor(detail = "") {
    super(`kavach: replay aborted${detail ? `: ${detail}` : ""}`);
    this.name = "KavachAbort";
    this.detail = detail;
  }
}
