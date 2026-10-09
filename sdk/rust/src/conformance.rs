//! The conformance handler of SPEC.md section 9.6, used by the transcript and
//! recorder conformance programs. Enabled by the `conformance` feature.

use std::error::Error;
use std::os::unix::ffi::OsStrExt;
use std::path::PathBuf;

use crate::json::{self, Value};
use crate::{
    Checker, Env, Gateways, Handler, HandlerResult, HostOptions, Input, Invariant, Scope,
    Snapshotter,
};

/// Where the spec's transcripts and recorder cases live: `KAVACH_SPEC_DIR`, or
/// the main checkout's `spec/` directory.
pub fn spec_dir() -> PathBuf {
    std::env::var_os("KAVACH_SPEC_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("/Users/koustav/code/kavach-labs/kavach/spec"))
}

/// The limit at which the `below_limit` invariant fails.
const LIMIT: u64 = 1000;

/// A count, to which every step that does not fail adds 1.
#[derive(Default)]
pub struct Conformance {
    count: u64,
}

impl Conformance {
    pub fn new() -> Conformance {
        Conformance::default()
    }

    /// All the conformance handler's gateways are remote, whatever their name.
    pub fn gateways() -> Gateways {
        Gateways::new().default_scope(Scope::Remote)
    }

    /// Host options for the conformance handler.
    pub fn host_options() -> HostOptions {
        HostOptions::new().gateways(Conformance::gateways())
    }
}

fn field<'a>(op: &'a Value, key: &str) -> Result<&'a str, Box<dyn Error>> {
    op.str_field(key)
        .ok_or_else(|| format!("operation is missing string {key:?}").into())
}

impl Handler for Conformance {
    fn handle(&mut self, env: &mut dyn Env, input: &Input) -> HandlerResult {
        let text =
            std::str::from_utf8(&input.data).map_err(|e| format!("input is not UTF-8: {e}"))?;
        let ops = json::parse(text).map_err(|e| format!("input is not JSON: {e}"))?;
        let ops = ops
            .as_array()
            .ok_or("input is not an array of operations")?;
        for op in ops {
            match field(op, "op")? {
                "clock" => {
                    let n = env.now_nanos();
                    env.emit(
                        "trace",
                        format!("{{\"clock\":\"{n}\"}}").as_bytes(),
                        Scope::Remote,
                    );
                }
                "rand" => {
                    let n = op.get("n").and_then(Value::as_u64).ok_or("rand needs n")?;
                    let mut buf = vec![0u8; n as usize];
                    env.fill_random(&mut buf);
                    env.emit("trace", &buf, Scope::Remote);
                }
                "gateway" => {
                    match env.query(field(op, "gateway")?, field(op, "request")?.as_bytes()) {
                        Ok(resp) => env.emit("trace", &resp, Scope::Remote),
                        Err(e) => {
                            let out = format!("{{\"error\":{}}}", json::quote(e.message()));
                            env.emit("trace", out.as_bytes(), Scope::Remote);
                        }
                    }
                }
                "config" => match env.config(field(op, "key")?) {
                    Some(v) => env.emit("trace", &v, Scope::Remote),
                    None => env.emit("trace", br#"{"unset":true}"#, Scope::Remote),
                },
                "getenv" => match std::env::var_os(field(op, "name")?) {
                    Some(v) => env.emit("trace", v.as_bytes(), Scope::Remote),
                    None => env.emit("trace", br#"{"unset":true}"#, Scope::Remote),
                },
                "emit" => env.emit(
                    field(op, "sink")?,
                    field(op, "data")?.as_bytes(),
                    Scope::Remote,
                ),
                "panic" => crate::panic(field(op, "message")?),
                "error" => return Err(field(op, "message")?.into()),
                "print" => println!("{}", field(op, "text")?),
                "count" => {
                    self.count += op.get("n").and_then(Value::as_u64).ok_or("count needs n")?
                }
                other => return Err(format!("unknown operation {other:?}").into()),
            }
        }
        self.count += 1;
        Ok(())
    }

    fn snapshotter(&mut self) -> Option<&mut dyn Snapshotter> {
        Some(self)
    }

    fn checker(&self) -> Option<&dyn Checker> {
        Some(self)
    }
}

impl Snapshotter for Conformance {
    fn snapshot(&mut self) -> Result<Vec<u8>, Box<dyn Error>> {
        Ok(self.count.to_string().into_bytes())
    }

    fn restore(&mut self, data: &[u8]) -> Result<(), Box<dyn Error>> {
        self.count = std::str::from_utf8(data)?.parse()?;
        Ok(())
    }
}

impl Checker for Conformance {
    fn invariants(&self) -> Vec<Invariant<'_>> {
        let count = self.count;
        vec![Invariant::new("below_limit", move || {
            if count >= LIMIT {
                Err(format!("count is {count}, the limit is {LIMIT}"))
            } else {
                Ok(())
            }
        })]
    }
}
