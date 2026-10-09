//! The host side of the replay protocol (SPEC.md section 9).

use std::error::Error;
use std::fs::File;
use std::io::{BufRead, Write};
use std::os::unix::io::FromRawFd;
use std::panic::resume_unwind;
use std::process::{Command, Stdio};

use crate::b64;
use crate::gateway::Gateways;
use crate::json::Value;
use crate::panics::{self, Caught};
use crate::recorder::Deliver;
use crate::sys;
use crate::{first_violation, Env, GatewayError, Handler, Input, Output, Scope};

/// The argument that makes a binary act as a host (SPEC.md section 9.1).
const HOST_ARG: &str = "kavach-host";

/// Options for [`maybe_host`].
#[derive(Default)]
pub struct HostOptions {
    gateways: Gateways,
    local_setup: Option<Box<dyn FnMut()>>,
    deliver: Option<Deliver>,
}

impl HostOptions {
    pub fn new() -> HostOptions {
        HostOptions::default()
    }

    /// The gateways the handler queries. The host needs their scopes to report
    /// queries, and calls the connection of a local gateway when the driver
    /// answers `live` in sandbox replay (SPEC.md section 6.3).
    pub fn gateways(mut self, gateways: Gateways) -> Self {
        self.gateways = gateways;
        self
    }

    /// The application's local setup (SPEC.md section 6.3): creates the local
    /// resources the handler uses. Run before `ready`, in `sandbox` mode only. It
    /// must not reach the network.
    pub fn local_setup(mut self, f: impl FnMut() + 'static) -> Self {
        self.local_setup = Some(Box::new(f));
        self
    }

    /// Executes a successful step's local outputs. Used in `sandbox` mode only;
    /// remote outputs are never delivered by a host.
    pub fn deliver_local(
        mut self,
        f: impl FnMut(&[Output]) -> Result<(), Box<dyn Error>> + 'static,
    ) -> Self {
        self.deliver = Some(Box::new(f));
        self
    }
}

/// If this process was started as a host, that is, with `kavach-host` as its
/// last argument, serves the host protocol on standard input and output and
/// exits; otherwise returns at once. Call it first thing in `main`, before the
/// service consumes anything or starts a [`Recorder`](crate::Recorder).
///
/// `factory` creates a fresh handler for each `hello`. The protocol stream is
/// taken from standard output, which is then pointed at standard error, so that
/// a handler that prints cannot corrupt it.
pub fn maybe_host<F>(factory: F, options: HostOptions)
where
    F: FnMut() -> Box<dyn Handler>,
{
    let mut args = std::env::args_os();
    if args.len() < 2 || args.next_back().as_deref() != Some(std::ffi::OsStr::new(HOST_ARG)) {
        return;
    }
    let code = run(factory, options);
    std::process::exit(code);
}

/// Unwinds a step after an `abort` (SPEC.md section 9.4). Private, so handler
/// code cannot name it, and raised with `resume_unwind`, so no panic hook runs.
struct Aborted;

struct Proto {
    input: std::io::StdinLock<'static>,
    out: File,
}

impl Proto {
    fn send(&mut self, v: &Value) {
        let mut line = v.to_json();
        line.push('\n');
        if self
            .out
            .write_all(line.as_bytes())
            .and_then(|()| self.out.flush())
            .is_err()
        {
            std::process::exit(1); // the driver is gone
        }
    }

    /// The next message; `None` when the driver closed its end.
    fn recv(&mut self) -> Option<Value> {
        let mut line = String::new();
        loop {
            line.clear();
            match self.input.read_line(&mut line) {
                Ok(0) | Err(_) => return None,
                Ok(_) => {}
            }
            if line.trim().is_empty() {
                continue;
            }
            return match crate::json::parse(&line) {
                Ok(v) => Some(v),
                Err(e) => self.fatal(&format!("malformed message: {e}")),
            };
        }
    }

    fn fatal(&mut self, message: &str) -> ! {
        self.send(&Value::obj([
            ("t", Value::str("fatal")),
            ("message", Value::str(message)),
        ]));
        std::process::exit(1);
    }
}

fn run<F>(mut factory: F, mut options: HostOptions) -> i32
where
    F: FnMut() -> Box<dyn Handler>,
{
    // Take the protocol stream for ourselves and point fd 1 at stderr.
    let out = match sys::dup_fd(1).and_then(|fd| sys::dup2_fd(2, 1).map(|()| fd)) {
        // SAFETY: `fd` was just returned by dup and nothing else owns it.
        Ok(fd) => unsafe { File::from_raw_fd(fd) },
        Err(e) => {
            eprintln!("kavach: cannot take the protocol stream: {e}");
            return 1;
        }
    };
    let mut proto = Proto {
        input: std::io::stdin().lock(),
        out,
    };
    panics::install_panic_hook();

    let mut handler: Option<Box<dyn Handler>> = None;
    let mut sandbox = false;
    while let Some(msg) = proto.recv() {
        match msg.str_field("t") {
            Some("hello") => {
                if msg.get("protocol").and_then(Value::as_u64) != Some(1) {
                    proto.fatal("unsupported protocol version");
                }
                sandbox = msg.str_field("mode") == Some("sandbox");
                let snapshot = match msg.get("snapshot").and_then(Value::as_str) {
                    Some(s) => match b64::decode(s) {
                        Some(b) => Some(b),
                        None => proto.fatal("hello: snapshot is not valid base64"),
                    },
                    None => None,
                };
                if msg.str_field("start") == Some("snapshot") && snapshot.is_none() {
                    proto.fatal("hello: start is snapshot but there is no snapshot");
                }
                let setup = &mut options.local_setup;
                let made = panics::catch(|| {
                    if sandbox {
                        if let Some(f) = setup.as_mut() {
                            f();
                        }
                    }
                    let mut h = factory();
                    if let Some(data) = &snapshot {
                        match h.snapshotter() {
                            None => return Err("the handler is not a Snapshotter".to_string()),
                            Some(s) => s
                                .restore(data)
                                .map_err(|e| format!("restoring the snapshot: {e}"))?,
                        }
                    }
                    Ok(h)
                });
                let h = match made {
                    Caught::Done(Ok(h)) => h,
                    Caught::Done(Err(m)) => proto.fatal(&m),
                    Caught::Panicked { message, .. } => {
                        proto.fatal(&format!("creating the handler panicked: {message}"))
                    }
                };
                let invariants: Vec<Value> = h
                    .checker()
                    .map(|c| {
                        c.invariants()
                            .into_iter()
                            .map(|i| Value::Str(i.name))
                            .collect()
                    })
                    .unwrap_or_default();
                proto.send(&Value::obj([
                    ("t", Value::str("ready")),
                    ("protocol", Value::int(1)),
                    ("sdk", Value::str(crate::producer())),
                    ("invariants", Value::Arr(invariants)),
                    ("environment", environment()),
                ]));
                handler = Some(h);
            }
            Some("step") => {
                let Some(h) = handler.as_mut() else {
                    proto.fatal("step before hello")
                };
                let (Some(source), Some(position), Some(data)) = (
                    msg.str_field("source"),
                    msg.str_field("position"),
                    msg.str_field("data").and_then(b64::decode),
                ) else {
                    proto.fatal("malformed step")
                };
                let input = Input::new(source, position, data);
                step(&mut proto, h.as_mut(), &input, &mut options, sandbox);
            }
            Some("end") => return 0,
            t => proto.fatal(&format!("unexpected message {:?}", t.unwrap_or(""))),
        }
    }
    0 // the driver closed its end between steps
}

fn step(
    proto: &mut Proto,
    handler: &mut dyn Handler,
    input: &Input,
    options: &mut HostOptions,
    sandbox: bool,
) {
    let mut env = HostEnv {
        proto,
        gateways: &mut options.gateways,
        sandbox,
        local_outs: Vec::new(),
        aborted: false,
    };
    let caught = panics::catch(|| match handler.handle(&mut env, input) {
        Ok(()) => Ok(first_violation(&*handler)),
        Err(e) => Err(e),
    });
    let HostEnv {
        proto,
        local_outs,
        aborted,
        ..
    } = env;

    let mut done = vec![("t", Value::str("done"))];
    let mut ok = false;
    if aborted {
        done.push(("outcome", Value::str("aborted")));
    } else {
        match caught {
            Caught::Done(Ok(None)) => {
                ok = true;
                done.push(("outcome", Value::str("ok")));
            }
            Caught::Done(Ok(Some((name, detail)))) => {
                done.push(("outcome", Value::str("invariant")));
                done.push(("message", Value::Str(name)));
                done.push(("detail", Value::Str(detail)));
            }
            Caught::Done(Err(e)) => {
                done.push(("outcome", Value::str("error")));
                done.push(("message", Value::Str(e.to_string())));
            }
            Caught::Panicked {
                payload,
                message,
                backtrace,
            } => {
                if payload.is::<Aborted>() {
                    // An abort that the handler's own code unwound past us.
                    done.push(("outcome", Value::str("aborted")));
                } else {
                    done.push(("outcome", Value::str("panic")));
                    done.push(("message", Value::Str(message)));
                    if !backtrace.is_empty() {
                        done.push((
                            "detail",
                            Value::Str(String::from_utf8_lossy(&backtrace).into_owned()),
                        ));
                    }
                }
            }
        }
    }
    proto.send(&Value::obj_vec(done));

    // Local outputs are delivered after the step succeeds, in sandbox mode.
    if ok && sandbox && !local_outs.is_empty() {
        if let Some(deliver) = options.deliver.as_mut() {
            if let Err(e) = deliver(&local_outs) {
                eprintln!("kavach: delivering local outputs: {e}");
            }
        }
    }
}

struct HostEnv<'a> {
    proto: &'a mut Proto,
    gateways: &'a mut Gateways,
    sandbox: bool,
    local_outs: Vec<Output>,
    aborted: bool,
}

impl HostEnv<'_> {
    fn abort(&mut self) -> ! {
        self.aborted = true;
        resume_unwind(Box::new(Aborted));
    }

    /// Sends a request and waits for its answer of type `expect`.
    fn request(&mut self, req: Value, expect: &str) -> Value {
        if self.aborted {
            self.abort();
        }
        self.proto.send(&req);
        let Some(answer) = self.proto.recv() else {
            std::process::exit(1); // the driver went away during the step
        };
        match answer.str_field("t") {
            Some("abort") => self.abort(),
            Some(t) if t == expect => answer,
            t => self.proto.fatal(&format!(
                "expected a {expect} answer, got {:?}",
                t.unwrap_or("")
            )),
        }
    }

    fn bytes_field(&mut self, answer: &Value, key: &str) -> Vec<u8> {
        match answer.str_field(key).and_then(b64::decode) {
            Some(b) => b,
            None => self
                .proto
                .fatal(&format!("answer has no valid base64 {key:?}")),
        }
    }
}

impl Env for HostEnv<'_> {
    fn now_nanos(&mut self) -> i64 {
        let a = self.request(Value::obj([("t", Value::str("clock"))]), "clock");
        match a
            .str_field("unix_nanos")
            .and_then(|s| s.parse::<i64>().ok())
        {
            Some(n) => n,
            None => self.proto.fatal("clock answer has no valid unix_nanos"),
        }
    }

    fn fill_random(&mut self, buf: &mut [u8]) {
        if buf.is_empty() {
            return;
        }
        let a = self.request(
            Value::obj([
                ("t", Value::str("rand")),
                ("n", Value::int(buf.len() as i64)),
            ]),
            "rand",
        );
        let data = self.bytes_field(&a, "data");
        if data.len() != buf.len() {
            self.proto.fatal(&format!(
                "rand answer has {} bytes, expected {}",
                data.len(),
                buf.len()
            ));
        }
        buf.copy_from_slice(&data);
    }

    fn query(&mut self, gateway: &str, request: &[u8]) -> Result<Vec<u8>, GatewayError> {
        let scope = self.gateways.scope(gateway);
        let a = self.request(
            Value::obj([
                ("t", Value::str("gateway")),
                ("gateway", Value::str(gateway)),
                ("request", Value::Str(b64::encode(request))),
                ("scope", Value::str(scope.name())),
            ]),
            "gateway",
        );
        if a.get("live").and_then(Value::as_bool) == Some(true) {
            // Sandbox replay: a local query runs for real and the result is reported.
            let result = self.gateways.call(gateway, request);
            let observed = match &result {
                Ok(r) => Value::obj([
                    ("t", Value::str("observed")),
                    ("response", Value::Str(b64::encode(r))),
                ]),
                Err(e) => Value::obj([
                    ("t", Value::str("observed")),
                    ("error", Value::str(e.message())),
                ]),
            };
            self.proto.send(&observed);
            return result;
        }
        if let Some(e) = a.str_field("error") {
            return Err(GatewayError::new(e));
        }
        Ok(self.bytes_field(&a, "response"))
    }

    fn config(&mut self, key: &str) -> Option<Vec<u8>> {
        let a = self.request(
            Value::obj([("t", Value::str("config")), ("key", Value::str(key))]),
            "config",
        );
        if a.get("present").and_then(Value::as_bool) == Some(true) {
            Some(self.bytes_field(&a, "value"))
        } else {
            None
        }
    }

    fn emit(&mut self, sink: &str, data: &[u8], scope: Scope) {
        if self.aborted {
            self.abort();
        }
        self.proto.send(&Value::obj([
            ("t", Value::str("emit")),
            ("sink", Value::str(sink)),
            ("data", Value::Str(b64::encode(data))),
            ("scope", Value::str(scope.name())),
        ]));
        if self.sandbox && scope == Scope::Local {
            self.local_outs.push(Output {
                sink: sink.to_string(),
                data: data.to_vec(),
                scope,
            });
        }
    }
}

/// `ready.environment` (SPEC.md section 9.2): the replay machine's facts from
/// `kavach-recorder facts` if it can be run, plus `host.runtime`.
fn environment() -> Value {
    let mut facts: Vec<(String, Value)> = recorder_facts().unwrap_or_default();
    facts.retain(|(k, _)| k != "host.runtime");
    facts.push((
        "host.runtime".to_string(),
        Value::obj([(
            "value",
            Value::Str(b64::encode(crate::runtime().as_bytes())),
        )]),
    ));
    Value::Obj(facts)
}

fn recorder_facts() -> Option<Vec<(String, Value)>> {
    let prog = std::env::var("KAVACH_RECORDER").unwrap_or_else(|_| "kavach-recorder".to_string());
    let out = Command::new(prog)
        .arg("facts")
        .stdin(Stdio::null())
        .stderr(Stdio::null())
        .output()
        .ok()
        .filter(|o| o.status.success())?;
    match crate::json::parse(std::str::from_utf8(&out.stdout).ok()?).ok()? {
        Value::Obj(m) => Some(m),
        _ => None,
    }
}
