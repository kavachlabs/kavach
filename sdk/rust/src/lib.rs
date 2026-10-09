//! Rust SDK for Kavach: a flight recorder and a replay host for deterministic,
//! single-writer handlers.
//!
//! A service implements [`Handler`] and reaches the world only through the
//! [`Env`] it is given. In production a [`Recorder`] runs the handler step by
//! step and streams every input, read and output to `kavach-recorder` (SPEC.md
//! section 10). To replay, the same binary calls [`maybe_host`] first thing in
//! `main`: when the `kavach` CLI starts it with `kavach-host` as its last
//! argument, it serves the host protocol (SPEC.md section 9) and exits.
//!
//! The library has no dependencies and supports Unix only.
//!
//! ```no_run
//! use kavach::{Env, Handler, HandlerResult, HostOptions, Input, Recorder, Scope};
//!
//! struct Echo;
//!
//! impl Handler for Echo {
//!     fn handle(&mut self, env: &mut dyn Env, input: &Input) -> HandlerResult {
//!         env.emit("echo", &input.data, Scope::Remote);
//!         Ok(())
//!     }
//! }
//!
//! fn main() {
//!     kavach::maybe_host(|| Box::new(Echo), HostOptions::new());
//!     let mut rec = Recorder::builder("echo").build(Box::new(Echo)).unwrap();
//!     rec.step(&Input::new("stdin", "0", b"hi".to_vec())).unwrap();
//!     rec.close();
//! }
//! ```

#![allow(clippy::needless_doctest_main)]

#[cfg(not(unix))]
compile_error!("the kavach crate supports Unix only");

pub mod b64;
mod gateway;
mod host;
pub mod json;
mod panics;
mod recorder;
mod sys;
mod wire;

#[cfg(feature = "conformance")]
pub mod conformance;

use std::error::Error;
use std::fmt;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

pub use gateway::{Connection, Gateways};
pub use host::{maybe_host, HostOptions};
pub use panics::{install_panic_hook, panic};
pub use recorder::{Deliver, Recorder, RecorderBuilder, RecorderError, StepError};

/// Version of this library.
pub const VERSION: &str = env!("CARGO_PKG_VERSION");

/// The SDK's identity in headers and in `ready.sdk`.
pub fn producer() -> String {
    format!("kavach-rust/{VERSION}")
}

/// The language runtime, for `host.runtime` (SPEC.md section 4.8).
pub fn runtime() -> &'static str {
    env!("KAVACH_RUNTIME")
}

/// Where an output or a gateway lives (SPEC.md sections 4.4, 4.7, 6.3).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Scope {
    /// A system on another host.
    Remote,
    /// A resource of the host the process runs on.
    Local,
}

impl Scope {
    pub(crate) fn code(self) -> u8 {
        match self {
            Scope::Remote => 0,
            Scope::Local => 1,
        }
    }

    pub(crate) fn name(self) -> &'static str {
        match self {
            Scope::Remote => "remote",
            Scope::Local => "local",
        }
    }
}

/// One event consumed by a handler.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Input {
    /// Where the event came from, e.g. `"kafka:wallet-events"`.
    pub source: String,
    /// Its position in that source, e.g. `"3:1042"`.
    pub position: String,
    /// The event exactly as received.
    pub data: Vec<u8>,
}

impl Input {
    pub fn new(
        source: impl Into<String>,
        position: impl Into<String>,
        data: impl Into<Vec<u8>>,
    ) -> Input {
        Input {
            source: source.into(),
            position: position.into(),
            data: data.into(),
        }
    }
}

/// One effect a handler requested, as passed to a [`Deliver`] closure.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Output {
    pub sink: String,
    pub data: Vec<u8>,
    pub scope: Scope,
}

/// The failure a gateway connection reported, e.g. `"timeout"` (SPEC.md section 4.7).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GatewayError {
    message: String,
}

impl GatewayError {
    pub fn new(message: impl Into<String>) -> GatewayError {
        GatewayError {
            message: message.into(),
        }
    }

    /// The error string as recorded.
    pub fn message(&self) -> &str {
        &self.message
    }
}

impl fmt::Display for GatewayError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message)
    }
}

impl Error for GatewayError {}

impl From<String> for GatewayError {
    fn from(message: String) -> GatewayError {
        GatewayError { message }
    }
}

impl From<&str> for GatewayError {
    fn from(message: &str) -> GatewayError {
        GatewayError {
            message: message.to_string(),
        }
    }
}

/// Everything nondeterministic a handler does must go through its `Env`: reading
/// time, randomness, external systems and config, and producing effects. When
/// recording, reads are journaled; when replaying, they are served from the
/// journal and outputs are captured instead of executed.
pub trait Env {
    /// Reads the clock, as nanoseconds since the Unix epoch.
    fn now_nanos(&mut self) -> i64;

    /// Reads the clock.
    fn now(&mut self) -> SystemTime {
        let n = self.now_nanos();
        if n >= 0 {
            UNIX_EPOCH + Duration::from_nanos(n as u64)
        } else {
            UNIX_EPOCH - Duration::from_nanos(n.unsigned_abs())
        }
    }

    /// Fills `buf` with random bytes.
    fn fill_random(&mut self, buf: &mut [u8]);

    /// Queries a gateway: an external system the handler reads from. The
    /// response is recorded as the connection returned it.
    fn query(&mut self, gateway: &str, request: &[u8]) -> Result<Vec<u8>, GatewayError>;

    /// Reads a config value that can change what the handler does, such as a
    /// feature flag. `None` means the value is not set.
    fn config(&mut self, key: &str) -> Option<Vec<u8>>;

    /// Requests an effect. It is delivered only after the step succeeds, and
    /// never during replay.
    fn emit(&mut self, sink: &str, data: &[u8], scope: Scope);

    /// [`emit`](Env::emit) with [`Scope::Remote`].
    fn emit_remote(&mut self, sink: &str, data: &[u8]) {
        self.emit(sink, data, Scope::Remote);
    }
}

/// The result of a handler step. Any error type that converts into
/// `Box<dyn Error>` works with `?`; the marker message is `e.to_string()`.
pub type HandlerResult = Result<(), Box<dyn Error>>;

/// Folds inputs into state. It must be deterministic given its `Env`: the same
/// state, input, reads and config must produce the same outputs. Called from one
/// thread at a time.
pub trait Handler {
    /// Handles one input. `Err(e)` is recorded as an `error` marker with message
    /// `e.to_string()`; a panic is recorded as a `panic` marker (see the README's
    /// failure model).
    fn handle(&mut self, env: &mut dyn Env, input: &Input) -> HandlerResult;

    /// Return `Some(self)` if the handler can save and restore its state.
    fn snapshotter(&mut self) -> Option<&mut dyn Snapshotter> {
        None
    }

    /// Return `Some(self)` if the handler declares invariants.
    fn checker(&self) -> Option<&dyn Checker> {
        None
    }
}

/// Implemented by handlers whose state can be saved and restored. It lets the
/// recorder start new journal segments and lets fixtures begin at a snapshot.
pub trait Snapshotter {
    fn snapshot(&mut self) -> Result<Vec<u8>, Box<dyn Error>>;
    fn restore(&mut self, data: &[u8]) -> Result<(), Box<dyn Error>>;
}

/// A named property of handler state that must hold after every step.
pub struct Invariant<'a> {
    pub name: String,
    /// `Err(detail)` when the property does not hold.
    pub check: Box<dyn Fn() -> Result<(), String> + 'a>,
}

impl<'a> Invariant<'a> {
    pub fn new(
        name: impl Into<String>,
        check: impl Fn() -> Result<(), String> + 'a,
    ) -> Invariant<'a> {
        Invariant {
            name: name.into(),
            check: Box::new(check),
        }
    }
}

/// Implemented by handlers that declare invariants. They are checked, in order,
/// after every step that ended `ok`.
pub trait Checker {
    fn invariants(&self) -> Vec<Invariant<'_>>;
}

/// The first failing invariant, in declaration order.
pub(crate) fn first_violation(h: &dyn Handler) -> Option<(String, String)> {
    let c = h.checker()?;
    for inv in c.invariants() {
        if let Err(detail) = (inv.check)() {
            return Some((inv.name, detail));
        }
    }
    None
}
