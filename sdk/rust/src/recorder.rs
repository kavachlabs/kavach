//! The SDK half of the flight recorder pipe (SPEC.md sections 3.6 and 10).

use std::error::Error;
use std::fmt;
use std::io::{self, BufRead, BufReader, Write};
use std::os::unix::io::AsRawFd;
use std::os::unix::process::CommandExt;
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Condvar, Mutex, MutexGuard};
use std::thread::JoinHandle;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use crate::gateway::Gateways;
use crate::json::{self, Value};
use crate::panics::{self, Caught};
use crate::ring::{self, Ring};
use crate::sys;
use crate::wire::*;
use crate::{first_violation, Env, GatewayError, Handler, Input, Output, Scope};

/// Executes a step's outputs after the step succeeds.
pub type Deliver = Box<dyn FnMut(&[Output]) -> Result<(), Box<dyn Error>>>;

type LogSink = Arc<dyn Fn(&str) + Send + Sync>;
type RandFn = Box<dyn FnMut(&mut [u8])>;
type ConfigFn = Box<dyn FnMut(&str) -> Option<Vec<u8>>>;
type FlagsFn = Box<dyn FnMut() -> Vec<(String, Vec<u8>)>>;

#[derive(Clone)]
struct LogFn(LogSink);

impl LogFn {
    fn say(&self, m: &str) {
        (self.0)(m);
    }
}

/// Why a recorder could not be created.
#[derive(Debug)]
pub enum RecorderError {
    /// The recorder process could not be started (only when `required(true)`).
    Spawn(io::Error),
    /// The recorder did not become ready or reported a fatal error (only when
    /// `required(true)`).
    NotReady(String),
    /// `start_from_snapshot` was used with a handler that is not a `Snapshotter`,
    /// or its `restore` failed.
    Snapshot(String),
    /// Operating-system randomness could not be opened.
    Random(io::Error),
}

impl fmt::Display for RecorderError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            RecorderError::Spawn(e) => write!(f, "cannot start the recorder: {e}"),
            RecorderError::NotReady(m) => write!(f, "the recorder is not ready: {m}"),
            RecorderError::Snapshot(m) => write!(f, "cannot start from a snapshot: {m}"),
            RecorderError::Random(e) => write!(f, "cannot open /dev/urandom: {e}"),
        }
    }
}

impl Error for RecorderError {}

/// How a step ended other than `ok`. The failure has already been recorded.
#[derive(Debug)]
pub enum StepError {
    /// The handler returned an error.
    Handler(Box<dyn Error>),
    /// The handler panicked. Only returned when `recover_panics(true)`; otherwise
    /// the panic continues to unwind after it is recorded.
    Panic { message: String },
    /// A declared invariant failed after the step.
    Invariant { name: String, detail: String },
    /// The step succeeded but delivering its outputs failed.
    Deliver(Box<dyn Error>),
}

impl fmt::Display for StepError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            StepError::Handler(e) => write!(f, "handler error: {e}"),
            StepError::Panic { message } => write!(f, "handler panicked: {message}"),
            StepError::Invariant { name, detail } => {
                write!(f, "invariant {name} violated: {detail}")
            }
            StepError::Deliver(e) => write!(f, "delivering outputs: {e}"),
        }
    }
}

impl Error for StepError {}

/// State shared with the control-stream thread (SPEC.md section 10.3).
#[derive(Default)]
struct Ctl {
    ready: bool,
    durables: u64,
    closed: bool,
    fatal: Option<String>,
}

struct Shared {
    ctl: Mutex<Ctl>,
    cv: Condvar,
    /// The recorder reported a fatal error or its output ended: stop recording.
    dead: AtomicBool,
    /// Kept out of `ctl` so that each step checks it without taking the lock.
    snapshot_requested: AtomicBool,
}

impl Shared {
    fn lock(&self) -> MutexGuard<'_, Ctl> {
        self.ctl.lock().unwrap_or_else(|p| p.into_inner())
    }
}

/// Configures and starts a [`Recorder`].
pub struct RecorderBuilder {
    service: String,
    argv: Option<Vec<String>>,
    required: bool,
    handler_id: Option<String>,
    dir: Option<String>,
    compression: Option<String>,
    level: Option<u32>,
    block_bytes: Option<u64>,
    flush_ms: Option<u64>,
    segment_bytes: Option<u64>,
    segment_seconds: Option<u64>,
    retain_segments: Option<u64>,
    secret_keys: Vec<String>,
    clock: Option<Box<dyn FnMut() -> i64>>,
    rand: Option<RandFn>,
    gateways: Gateways,
    config: Option<(String, ConfigFn)>,
    flags: Option<FlagsFn>,
    deliver: Option<Deliver>,
    log: Option<LogSink>,
    start_snapshot: Option<Vec<u8>>,
    snapshots: Option<bool>,
    recover_panics: bool,
    capture_backtraces: bool,
    no_ring: bool,
    ring_bytes: usize,
    ready_timeout: Duration,
    close_timeout: Duration,
}

impl RecorderBuilder {
    fn new(service: &str) -> RecorderBuilder {
        RecorderBuilder {
            service: service.to_string(),
            argv: None,
            required: false,
            handler_id: None,
            dir: None,
            compression: None,
            level: None,
            block_bytes: None,
            flush_ms: None,
            segment_bytes: None,
            segment_seconds: None,
            retain_segments: None,
            secret_keys: Vec::new(),
            clock: None,
            rand: None,
            gateways: Gateways::new(),
            config: None,
            flags: None,
            deliver: None,
            log: None,
            start_snapshot: None,
            snapshots: None,
            recover_panics: false,
            capture_backtraces: false,
            no_ring: false,
            ring_bytes: ring::DEFAULT_CAPACITY,
            ready_timeout: Duration::from_secs(5),
            close_timeout: Duration::from_secs(10),
        }
    }

    /// The recorder command as an argument vector. Without it, the
    /// `KAVACH_RECORDER` environment variable names the executable; failing
    /// that, `kavach-recorder` on `PATH`.
    pub fn recorder_command<I, S>(mut self, argv: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.argv = Some(argv.into_iter().map(Into::into).collect());
        self
    }

    /// Fail `build` if the recorder cannot be started or does not become ready
    /// (SPEC.md section 10.1). Off by default: recording then stops with a log
    /// message and the service runs on.
    pub fn required(mut self, required: bool) -> Self {
        self.required = required;
        self
    }

    /// Build identity of the handler, for the journal header.
    pub fn handler_id(mut self, id: impl Into<String>) -> Self {
        self.handler_id = Some(id.into());
        self
    }

    /// Directory for journal segments (recorder default `kavach`).
    pub fn dir(mut self, dir: impl Into<String>) -> Self {
        self.dir = Some(dir.into());
        self
    }

    /// `"zstd"` or `"none"`.
    pub fn compression(mut self, compression: impl Into<String>) -> Self {
        self.compression = Some(compression.into());
        self
    }

    /// zstd compression level.
    pub fn level(mut self, level: u32) -> Self {
        self.level = Some(level);
        self
    }

    /// Target raw size of a block.
    pub fn block_bytes(mut self, n: u64) -> Self {
        self.block_bytes = Some(n);
        self
    }

    /// Flush interval in milliseconds.
    pub fn flush_ms(mut self, ms: u64) -> Self {
        self.flush_ms = Some(ms);
        self
    }

    /// Size and age limits after which the recorder starts a new segment.
    pub fn segment_limits(mut self, bytes: u64, seconds: u64) -> Self {
        self.segment_bytes = Some(bytes);
        self.segment_seconds = Some(seconds);
        self
    }

    /// Segments to keep.
    pub fn retain_segments(mut self, n: u64) -> Self {
        self.retain_segments = Some(n);
        self
    }

    /// Environment variable names to hash besides those the spec requires.
    pub fn secret_keys<I, S>(mut self, keys: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.secret_keys = keys.into_iter().map(Into::into).collect();
        self
    }

    /// The clock the handler reads, as nanoseconds since the Unix epoch.
    /// Defaults to the system clock.
    pub fn clock(mut self, f: impl FnMut() -> i64 + 'static) -> Self {
        self.clock = Some(Box::new(f));
        self
    }

    /// The random source the handler reads: fills the whole slice. Defaults to
    /// `/dev/urandom`.
    pub fn rand(mut self, f: impl FnMut(&mut [u8]) + 'static) -> Self {
        self.rand = Some(Box::new(f));
        self
    }

    /// The gateways the handler may query.
    pub fn gateways(mut self, gateways: Gateways) -> Self {
        self.gateways = gateways;
        self
    }

    /// Where config reads come from; `source` is recorded with each read.
    /// Defaults to environment variables, with source `"env"`.
    pub fn config(
        mut self,
        source: impl Into<String>,
        f: impl FnMut(&str) -> Option<Vec<u8>> + 'static,
    ) -> Self {
        self.config = Some((source.into(), Box::new(f)));
        self
    }

    /// Reports the feature flags the process could evaluate, as `(key, value)`
    /// with keys like `flag.beta`, for the environment (SPEC.md section 4.8).
    /// Called once, when the recorder starts.
    pub fn flags(mut self, f: impl FnMut() -> Vec<(String, Vec<u8>)> + 'static) -> Self {
        self.flags = Some(Box::new(f));
        self
    }

    /// Executes a step's outputs after the step succeeds. Without it, outputs
    /// are only recorded.
    pub fn deliver(
        mut self,
        f: impl FnMut(&[Output]) -> Result<(), Box<dyn Error>> + 'static,
    ) -> Self {
        self.deliver = Some(Box::new(f));
        self
    }

    /// Where the SDK logs recorder problems and `fixture` messages. Defaults to
    /// standard error.
    pub fn log(mut self, f: impl Fn(&str) + Send + Sync + 'static) -> Self {
        self.log = Some(Arc::new(f));
        self
    }

    /// Starts the journal at a snapshot: `data` is restored into the handler,
    /// which must be a `Snapshotter`, and recorded as the journal's first record
    /// (SPEC.md section 10.4).
    pub fn start_from_snapshot(mut self, data: Vec<u8>) -> Self {
        self.start_snapshot = Some(data);
        self
    }

    /// Whether to tell the recorder that it may ask for snapshots (and so start
    /// new segments). Defaults to whether the handler is a `Snapshotter`.
    pub fn snapshots(mut self, on: bool) -> Self {
        self.snapshots = Some(on);
        self
    }

    /// Return `StepError::Panic` from [`Recorder::step`] after recording a panic,
    /// instead of continuing to unwind.
    pub fn recover_panics(mut self, on: bool) -> Self {
        self.recover_panics = on;
        self
    }

    /// Install the chaining panic hook ([`install_panic_hook`](crate::install_panic_hook))
    /// so that `panic` markers carry a backtrace in their data. Off by default,
    /// since it changes the process's panic hook (though it calls the previous one).
    pub fn capture_backtraces(mut self, on: bool) -> Self {
        self.capture_backtraces = on;
        self
    }

    /// Carry the record stream over the recorder's pipe instead of a
    /// shared-memory ring (SPEC.md section 10.7). The ring is the default, and
    /// the pipe is used where it cannot be set up.
    pub fn no_ring(mut self, on: bool) -> Self {
        self.no_ring = on;
        self
    }

    /// The ring's capacity, a power of two of at least 64 KiB (default 8 MiB).
    /// Another value is logged and the pipe is used.
    pub fn ring_bytes(mut self, n: usize) -> Self {
        self.ring_bytes = n;
        self
    }

    /// How long `build` waits for `ready` when `required(true)` (default 5 s).
    pub fn ready_timeout(mut self, d: Duration) -> Self {
        self.ready_timeout = d;
        self
    }

    /// How long `close` waits for `closed` (default 10 s).
    pub fn close_timeout(mut self, d: Duration) -> Self {
        self.close_timeout = d;
        self
    }

    /// Starts the recorder and returns a recorder driving `handler`.
    pub fn build(self, mut handler: Box<dyn Handler>) -> Result<Recorder, RecorderError> {
        let log = LogFn(
            self.log
                .clone()
                .unwrap_or_else(|| Arc::new(|m: &str| eprintln!("kavach: {m}"))),
        );
        if self.capture_backtraces {
            panics::install_panic_hook();
        }
        let snapshots = self
            .snapshots
            .unwrap_or_else(|| handler.snapshotter().is_some());
        if let Some(data) = &self.start_snapshot {
            match handler.snapshotter() {
                None => {
                    return Err(RecorderError::Snapshot(
                        "the handler is not a Snapshotter".into(),
                    ))
                }
                Some(s) => s
                    .restore(data)
                    .map_err(|e| RecorderError::Snapshot(e.to_string()))?,
            }
        }

        let urandom = if self.rand.is_none() {
            Some(sys::Urandom::open().map_err(RecorderError::Random)?)
        } else {
            None
        };

        let shared = Arc::new(Shared {
            ctl: Mutex::new(Ctl::default()),
            cv: Condvar::new(),
            dead: AtomicBool::new(false),
            snapshot_requested: AtomicBool::new(false),
        });

        let argv = self.argv.clone().unwrap_or_else(|| {
            vec![std::env::var("KAVACH_RECORDER").unwrap_or_else(|_| "kavach-recorder".to_string())]
        });
        let mut child = None;
        let mut stdin = None;
        let mut thread = None;
        let ring = if self.no_ring {
            None
        } else {
            match Ring::create(self.ring_bytes) {
                Ok(r) => Some(r),
                Err(e) => {
                    log.say(&format!(
                        "no shared-memory ring, recording over the pipe: {e}"
                    ));
                    None
                }
            }
        };
        let (ring, ring_file) = ring.unzip();
        match spawn(&argv, ring_file.as_ref()) {
            Ok(mut c) => {
                if let Some(s) = c.stdin.take() {
                    sys::grow_pipe(s.as_raw_fd(), 1 << 20);
                    stdin = Some(s);
                }
                if let Some(out) = c.stdout.take() {
                    let (sh, lg) = (shared.clone(), log.clone());
                    thread = Some(std::thread::spawn(move || control_loop(out, &sh, &lg)));
                }
                child = Some(c);
            }
            Err(e) => {
                if self.required {
                    return Err(RecorderError::Spawn(e));
                }
                log.say(&format!(
                    "cannot start the recorder {:?}: {e}; recording is off",
                    argv[0]
                ));
                shared.dead.store(true, Ordering::SeqCst);
            }
        }

        drop(ring_file);
        let mut pipe = Pipe {
            stdin,
            ring: None,
            belled: false,
            log: log.clone(),
            shared: shared.clone(),
        };

        let mut buf = Vec::new();
        let ring_capacity = ring.as_ref().map(Ring::capacity);
        put_frame(
            &mut buf,
            FRAME_OPEN,
            self.open_json(snapshots, ring_capacity).as_bytes(),
        );
        // The open frame is the only one on standard input; every later frame
        // goes into the ring (SPEC.md section 10.7).
        pipe.write_stdin(&buf);
        pipe.ring = ring;
        buf.clear();
        let mut facts: Vec<(String, Vec<u8>)> =
            vec![("host.runtime".into(), crate::runtime().as_bytes().to_vec())];
        let mut flags = self.flags;
        if let Some(f) = flags.as_mut() {
            facts.extend(f());
        }
        let mut payload = Vec::new();
        put_uvarint(&mut payload, facts.len() as u64);
        for (k, v) in &facts {
            put_str(&mut payload, k);
            payload.push(0);
            put_bytes(&mut payload, v);
        }
        put_frame(&mut buf, FRAME_FACTS, &payload);
        if let Some(data) = &self.start_snapshot {
            let mut p = Vec::new();
            put_bytes(&mut p, data);
            put_frame(&mut buf, FRAME_SNAPSHOT, &p);
        }
        pipe.write(&buf);

        if self.required {
            let deadline = Instant::now() + self.ready_timeout;
            let mut ctl = shared.lock();
            while !ctl.ready && ctl.fatal.is_none() && !shared.dead.load(Ordering::SeqCst) {
                let left = deadline.saturating_duration_since(Instant::now());
                if left.is_zero() {
                    return Err(RecorderError::NotReady(
                        "timed out waiting for ready".into(),
                    ));
                }
                ctl = shared
                    .cv
                    .wait_timeout(ctl, left)
                    .unwrap_or_else(|p| p.into_inner())
                    .0;
            }
            if !ctl.ready {
                return Err(RecorderError::NotReady(
                    ctl.fatal
                        .clone()
                        .unwrap_or_else(|| "the recorder exited".into()),
                ));
            }
        }

        let reads = Reads {
            clock: self.clock.unwrap_or_else(|| Box::new(system_nanos)),
            rand: self.rand,
            urandom,
            gateways: self.gateways,
            config: self.config.unwrap_or_else(|| {
                (
                    "env".to_string(),
                    Box::new(|k: &str| {
                        std::env::var_os(k).map(|v| v.to_string_lossy().into_owned().into_bytes())
                    }),
                )
            }),
        };
        Ok(Recorder {
            handler,
            pipe,
            reads,
            deliver: self.deliver,
            recover_panics: self.recover_panics,
            close_timeout: self.close_timeout,
            shared,
            child,
            thread,
            closed: false,
            buf: Vec::new(),
            scratch: Vec::new(),
        })
    }

    fn open_json(&self, snapshots: bool, ring: Option<u64>) -> String {
        let mut o = vec![
            ("protocol", Value::int(1)),
            ("service", Value::str(&self.service)),
            (
                "start",
                Value::str(if self.start_snapshot.is_some() {
                    "snapshot"
                } else {
                    "genesis"
                }),
            ),
            ("producer", Value::str(crate::producer())),
            ("snapshots", Value::Bool(snapshots)),
        ];
        if let Some(c) = ring {
            o.push(("ring", Value::Num(c as f64)));
        }
        if let Some(h) = &self.handler_id {
            o.push(("handler", Value::str(h)));
        }
        if let Some(d) = &self.dir {
            o.push(("dir", Value::str(d)));
        }
        if let Some(c) = &self.compression {
            o.push(("compression", Value::str(c)));
        }
        for (k, v) in [
            ("level", self.level.map(u64::from)),
            ("block_bytes", self.block_bytes),
            ("flush_ms", self.flush_ms),
            ("segment_bytes", self.segment_bytes),
            ("segment_seconds", self.segment_seconds),
            ("retain_segments", self.retain_segments),
        ] {
            if let Some(v) = v {
                o.push((k, Value::Num(v as f64)));
            }
        }
        if !self.secret_keys.is_empty() {
            o.push((
                "secret_keys",
                Value::Arr(self.secret_keys.iter().map(Value::str).collect()),
            ));
        }
        Value::Obj(o.into_iter().map(|(k, v)| (k.to_string(), v)).collect()).to_json()
    }
}

fn spawn(argv: &[String], ring: Option<&std::fs::File>) -> io::Result<Child> {
    let (prog, args) = argv
        .split_first()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "empty recorder command"))?;
    // The environment is inherited unchanged: the recorder collects `env.` facts
    // from it (SPEC.md section 10.1).
    let mut cmd = Command::new(prog);
    cmd.args(args)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit());
    if let Some(f) = ring {
        let fd = f.as_raw_fd();
        // SAFETY: the closure only calls dup2 and fcntl, which are async-signal-safe.
        unsafe {
            cmd.pre_exec(move || {
                if fd == RING_FD {
                    sys::keep_across_exec(fd);
                    Ok(())
                } else {
                    sys::dup2_fd(fd, RING_FD)
                }
            });
        }
    }
    cmd.spawn()
}

/// The descriptor the recorder finds the ring on (SPEC.md section 10.7).
const RING_FD: i32 = 3;

fn system_nanos() -> i64 {
    match SystemTime::now().duration_since(UNIX_EPOCH) {
        Ok(d) => d.as_nanos() as i64,
        Err(e) => -(e.duration().as_nanos() as i64),
    }
}

/// Reads the control stream on its own thread (SPEC.md section 10.3).
fn control_loop(out: std::process::ChildStdout, sh: &Shared, log: &LogFn) {
    for line in BufReader::new(out).lines() {
        let Ok(line) = line else { break };
        if line.trim().is_empty() {
            continue;
        }
        let msg = match json::parse(&line) {
            Ok(m) => m,
            Err(e) => {
                log.say(&format!(
                    "unreadable message from the recorder ({e}): {line}"
                ));
                continue;
            }
        };
        match msg.str_field("t") {
            Some("ready") => sh.lock().ready = true,
            Some("snapshot_request") => sh.snapshot_requested.store(true, Ordering::Release),
            Some("durable") => sh.lock().durables += 1,
            Some("closed") => sh.lock().closed = true,
            Some("fixture") => {
                let failure = match msg.get("failure") {
                    Some(Value::Str(s)) => s.clone(),
                    Some(v) => v.to_json(),
                    None => String::new(),
                };
                log.say(&format!(
                    "wrote fixture {} for the failure at input {}: {}",
                    msg.str_field("file").unwrap_or("?"),
                    msg.str_field("seq").unwrap_or("?"),
                    failure
                ));
            }
            Some("error") => {
                let m = msg.str_field("message").unwrap_or("unknown error");
                let fatal = msg.get("fatal").and_then(Value::as_bool).unwrap_or(false);
                log.say(&format!(
                    "recorder error{}: {m}",
                    if fatal { " (fatal)" } else { "" }
                ));
                if fatal {
                    sh.lock().fatal = Some(m.to_string());
                    sh.dead.store(true, Ordering::SeqCst);
                }
            }
            _ => {} // segment, and messages from a newer recorder
        }
        sh.cv.notify_all();
    }
    let closed = sh.lock().closed;
    if !closed {
        sh.dead.store(true, Ordering::SeqCst);
    }
    sh.cv.notify_all();
}

/// The write end of the record stream: the ring if there is one, else the
/// recorder's standard input. A failed write ends recording.
struct Pipe {
    stdin: Option<ChildStdin>,
    ring: Option<Ring>,
    /// The doorbell rang since the ring was last under half full.
    belled: bool,
    log: LogFn,
    shared: Arc<Shared>,
}

impl Pipe {
    fn write(&mut self, buf: &[u8]) -> bool {
        if self.stdin.is_none() {
            return false;
        }
        if self.shared.dead.load(Ordering::SeqCst) {
            self.stopped();
            return false;
        }
        if self.ring.is_some() {
            self.publish(buf)
        } else {
            self.write_stdin(buf)
        }
    }

    fn stopped(&mut self) {
        if !self.shared.lock().closed {
            self.log.say("the recorder stopped; recording is off");
        }
        self.stdin = None;
    }

    fn write_stdin(&mut self, buf: &[u8]) -> bool {
        let Some(w) = self.stdin.as_mut() else {
            return false;
        };
        match w.write_all(buf).and_then(|()| w.flush()) {
            Ok(()) => true,
            Err(e) => {
                // BrokenPipe when the recorder died; never fail the step.
                self.log.say(&format!(
                    "cannot write to the recorder ({e}); recording is off"
                ));
                self.shared.dead.store(true, Ordering::SeqCst);
                self.stdin = None;
                false
            }
        }
    }

    /// Publishes whole frames into the ring, waiting for the recorder to make
    /// room when it is full (SPEC.md sections 10.1 and 10.7).
    fn publish(&mut self, mut buf: &[u8]) -> bool {
        while !buf.is_empty() {
            let (mut n, mut used) = self.try_publish(buf);
            if n == 0 {
                if !self.bell() {
                    return false;
                }
                while n == 0 {
                    if self.shared.dead.load(Ordering::SeqCst) {
                        self.stopped();
                        return false;
                    }
                    std::thread::sleep(Duration::from_micros(20));
                    (n, used) = self.try_publish(buf);
                }
            }
            buf = &buf[n..];
            let half = self.ring.as_ref().map_or(0, Ring::capacity) / 2;
            if used > half && !self.belled {
                self.belled = true;
                if !self.bell() {
                    return false;
                }
            } else if used <= half {
                self.belled = false;
            }
        }
        true
    }

    fn try_publish(&self, buf: &[u8]) -> (usize, u64) {
        self.ring.as_ref().map_or((0, 0), |r| r.try_publish(buf))
    }

    /// Wakes a recorder that polls the ring: one byte on standard input.
    fn bell(&mut self) -> bool {
        self.ring.is_none() || self.write_stdin(&[1])
    }

    fn live(&self) -> bool {
        self.stdin.is_some() && !self.shared.dead.load(Ordering::SeqCst)
    }
}

struct Reads {
    clock: Box<dyn FnMut() -> i64>,
    rand: Option<RandFn>,
    urandom: Option<sys::Urandom>,
    gateways: Gateways,
    config: (String, ConfigFn),
}

/// The `Env` of a recording step: reads are live, and each is appended to the
/// step's frames in program order.
struct RecEnv<'a> {
    reads: &'a mut Reads,
    buf: &'a mut Vec<u8>,
    /// Reused for encoding a record's fields.
    scratch: &'a mut Vec<u8>,
    /// Outputs are kept only when there is a `deliver` to hand them to.
    outs: Option<&'a mut Vec<Output>>,
}

impl Env for RecEnv<'_> {
    fn now_nanos(&mut self) -> i64 {
        let n = (self.reads.clock)();
        put_record(self.buf, REC_CLOCK, 0, &n.to_le_bytes());
        n
    }

    fn fill_random(&mut self, buf: &mut [u8]) {
        if buf.is_empty() {
            return;
        }
        match (self.reads.rand.as_mut(), self.reads.urandom.as_mut()) {
            (Some(f), _) => f(buf),
            (None, Some(u)) => u.fill(buf).expect("reading /dev/urandom"),
            (None, None) => unreachable!("a recorder always has a random source"),
        }
        self.scratch.clear();
        put_bytes(self.scratch, buf);
        put_record(self.buf, REC_RAND, 0, self.scratch);
    }

    fn query(&mut self, gateway: &str, request: &[u8]) -> Result<Vec<u8>, GatewayError> {
        let scope = self.reads.gateways.scope(gateway);
        let result = self.reads.gateways.call(gateway, request);
        let (response, error): (&[u8], &str) = match &result {
            Ok(r) => (r, ""),
            Err(e) => (&[], e.message()),
        };
        let mut f = Vec::new();
        put_str(&mut f, gateway);
        put_bytes(&mut f, request);
        put_bytes(&mut f, response);
        put_str(&mut f, error);
        f.push(scope.code());
        put_record(self.buf, REC_GATEWAY, CRITICAL, &f);
        result
    }

    fn config(&mut self, key: &str) -> Option<Vec<u8>> {
        let value = (self.reads.config.1)(key);
        let mut f = Vec::new();
        put_str(&mut f, key);
        f.push(u8::from(value.is_some()));
        put_bytes(&mut f, value.as_deref().unwrap_or(&[]));
        put_str(&mut f, &self.reads.config.0);
        put_record(self.buf, REC_CONFIG, CRITICAL, &f);
        value
    }

    fn emit(&mut self, sink: &str, data: &[u8], scope: Scope) {
        self.scratch.clear();
        put_str(self.scratch, sink);
        put_bytes(self.scratch, data);
        self.scratch.push(scope.code());
        put_record(self.buf, REC_OUTPUT, 0, self.scratch);
        if let Some(outs) = self.outs.as_deref_mut() {
            outs.push(Output {
                sink: sink.to_string(),
                data: data.to_vec(),
                scope,
            });
        }
    }
}

fn put_marker(buf: &mut Vec<u8>, kind: &str, message: &str, data: &[u8]) {
    let mut f = Vec::new();
    put_str(&mut f, kind);
    put_str(&mut f, message);
    put_bytes(&mut f, data);
    put_record(buf, REC_MARKER, 0, &f);
}

/// A flight recorder: runs a handler step by step and streams everything it
/// reads, consumes and produces to `kavach-recorder`.
///
/// If the recorder cannot be started, or later stops, the SDK logs it and the
/// handler keeps running unrecorded; a step never fails because of the recorder.
pub struct Recorder {
    handler: Box<dyn Handler>,
    pipe: Pipe,
    reads: Reads,
    deliver: Option<Deliver>,
    recover_panics: bool,
    close_timeout: Duration,
    shared: Arc<Shared>,
    child: Option<Child>,
    thread: Option<JoinHandle<()>>,
    closed: bool,
    /// Per-step scratch space, kept to avoid allocating on every step.
    buf: Vec<u8>,
    scratch: Vec<u8>,
}

impl Recorder {
    /// Starts configuring a recorder for `service`.
    pub fn builder(service: &str) -> RecorderBuilder {
        RecorderBuilder::new(service)
    }

    /// The handler being recorded.
    pub fn handler(&self) -> &dyn Handler {
        &*self.handler
    }

    /// Whether the recorder is still accepting records.
    pub fn is_recording(&self) -> bool {
        self.pipe.live()
    }

    /// Runs the handler on one input and records the step.
    ///
    /// The `input` frame is written before the handler is called, so a step that
    /// ends the process without unwinding still leaves its input on record
    /// (SPEC.md section 10.5). Everything else the step produced is written with
    /// `step_end`.
    ///
    /// A handler `Err` is recorded as an `error` marker and returned as
    /// [`StepError::Handler`]. A panic is recorded as a `panic` marker and then
    /// continues to unwind, unless `recover_panics(true)`. Invariants are checked
    /// after a step that ended `ok`. Outputs are delivered only after a fully
    /// successful step.
    pub fn step(&mut self, input: &Input) -> Result<(), StepError> {
        self.answer_snapshot_request();

        let mut scratch = std::mem::take(&mut self.scratch);
        let mut buf = std::mem::take(&mut self.buf);
        if self.pipe.live() {
            scratch.clear();
            put_str(&mut scratch, &input.source);
            put_str(&mut scratch, &input.position);
            put_bytes(&mut scratch, &input.data);
            buf.clear();
            put_record(&mut buf, REC_INPUT, 0, &scratch);
            self.pipe.write(&buf);
        }

        buf.clear();
        let mut outs = Vec::new();
        let caught = {
            let handler = &mut self.handler;
            let mut env = RecEnv {
                reads: &mut self.reads,
                buf: &mut buf,
                scratch: &mut scratch,
                outs: self.deliver.is_some().then_some(&mut outs),
            };
            panics::catch(|| match handler.handle(&mut env, input) {
                Ok(()) => Ok(first_violation(&**handler)),
                Err(e) => Err(e),
            })
        };

        let mut resume = None;
        let result = match caught {
            Caught::Done(Ok(None)) => Ok(()),
            Caught::Done(Ok(Some((name, detail)))) => {
                put_marker(&mut buf, "invariant", &name, detail.as_bytes());
                Err(StepError::Invariant { name, detail })
            }
            Caught::Done(Err(e)) => {
                put_marker(&mut buf, "error", &e.to_string(), &[]);
                Err(StepError::Handler(e))
            }
            Caught::Panicked {
                payload,
                message,
                backtrace,
            } => {
                put_marker(&mut buf, "panic", &message, &backtrace);
                if self.recover_panics {
                    Err(StepError::Panic { message })
                } else {
                    resume = Some(payload);
                    Ok(())
                }
            }
        };
        put_frame(&mut buf, FRAME_STEP_END, &[]);
        self.pipe.write(&buf);
        self.scratch = scratch;
        self.buf = buf;

        if let Some(payload) = resume {
            std::panic::resume_unwind(payload);
        }
        result?;
        if let Some(deliver) = self.deliver.as_mut() {
            if !outs.is_empty() {
                deliver(&outs).map_err(StepError::Deliver)?;
            }
        }
        Ok(())
    }

    /// Answers a pending `snapshot_request` at this step boundary (SPEC.md
    /// section 10.4).
    fn answer_snapshot_request(&mut self) {
        let requested = self
            .shared
            .snapshot_requested
            .swap(false, Ordering::Acquire);
        if !requested || !self.pipe.live() {
            return;
        }
        let Some(s) = self.handler.snapshotter() else {
            return;
        };
        match s.snapshot() {
            Ok(data) => {
                let mut p = Vec::new();
                put_bytes(&mut p, &data);
                let mut frame = Vec::new();
                put_frame(&mut frame, FRAME_SNAPSHOT, &p);
                self.pipe.write(&frame);
            }
            Err(e) => self.pipe.log.say(&format!(
                "snapshot failed, the segment will not change: {e}"
            )),
        }
    }

    /// Closes the open block now. With `durable`, also waits (up to `timeout`)
    /// until the recorder says it is durable, and returns whether it did.
    pub fn flush(&mut self, durable: bool, timeout: Duration) -> bool {
        let before = self.shared.lock().durables;
        let mut f = Vec::new();
        put_frame(&mut f, FRAME_FLUSH, &[u8::from(durable)]);
        if !(self.pipe.write(&f) && self.pipe.bell()) {
            return false;
        }
        if !durable {
            return true;
        }
        let deadline = Instant::now() + timeout;
        let mut ctl = self.shared.lock();
        while ctl.durables <= before {
            let left = deadline.saturating_duration_since(Instant::now());
            if left.is_zero() || self.shared.dead.load(Ordering::SeqCst) {
                return false;
            }
            ctl = self
                .shared
                .cv
                .wait_timeout(ctl, left)
                .unwrap_or_else(|p| p.into_inner())
                .0;
        }
        true
    }

    /// Orderly shutdown (SPEC.md section 10.5): tells the recorder to finish the
    /// journal and waits for `closed`, up to the close timeout. Returns whether
    /// it was acknowledged. Also done on drop.
    pub fn close(&mut self) -> bool {
        if self.closed {
            return self.shared.lock().closed;
        }
        self.closed = true;
        let mut f = Vec::new();
        put_frame(&mut f, FRAME_CLOSE, &[]);
        if self.pipe.write(&f) {
            self.pipe.bell();
        }
        let deadline = Instant::now() + self.close_timeout;
        let mut acked;
        {
            let mut ctl = self.shared.lock();
            loop {
                acked = ctl.closed;
                if acked || self.shared.dead.load(Ordering::SeqCst) {
                    break;
                }
                let left = deadline.saturating_duration_since(Instant::now());
                if left.is_zero() {
                    self.pipe
                        .log
                        .say("timed out waiting for the recorder to close");
                    break;
                }
                ctl = self
                    .shared
                    .cv
                    .wait_timeout(ctl, left)
                    .unwrap_or_else(|p| p.into_inner())
                    .0;
            }
        }
        self.pipe.stdin = None; // EOF for the recorder
        if let Some(t) = self.thread.take() {
            if acked {
                let _ = t.join();
            }
        }
        if let Some(mut c) = self.child.take() {
            if acked {
                let _ = c.wait();
            }
        }
        acked
    }
}

impl Drop for Recorder {
    fn drop(&mut self) {
        self.close();
    }
}
