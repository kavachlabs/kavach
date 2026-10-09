//! The shared-memory ring against the real `kavach-recorder` (SPEC.md section
//! 10.7). Needs a Go toolchain to build the recorder.

use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use kavach::{Env, Handler, HandlerResult, Input, Recorder, Scope};

const HELPER: &str = "KAVACH_RING_TEST_HELPER";

fn recorder_bin() -> &'static Path {
    static BIN: OnceLock<PathBuf> = OnceLock::new();
    BIN.get_or_init(|| {
        let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("../..");
        let out = std::env::temp_dir().join(format!("kavach-recorder-{}", std::process::id()));
        let st = Command::new("go")
            .current_dir(&root)
            .args(["build", "-o"])
            .arg(&out)
            .arg("./cmd/kavach-recorder")
            .status()
            .expect("running go build");
        assert!(st.success(), "go build ./cmd/kavach-recorder failed");
        out
    })
}

struct Killer;

impl Handler for Killer {
    fn handle(&mut self, env: &mut dyn Env, input: &Input) -> HandlerResult {
        if input.data == b"die" {
            // SIGKILL, as an abort or the OOM killer would send: nothing unwinds.
            let _ = Command::new("kill")
                .args(["-9", &std::process::id().to_string()])
                .status();
            loop {
                std::thread::sleep(Duration::from_secs(1));
            }
        }
        env.now_nanos();
        env.emit("out", &input.data, Scope::Remote);
        Ok(())
    }
}

/// The helper process, a re-executed test binary: it records some steps,
/// including two larger than the ring, then one that kills the process.
#[test]
fn killed_service_helper() {
    let Ok(dir) = std::env::var(HELPER) else {
        return;
    };
    let mut rec = Recorder::builder("killed")
        .dir(dir)
        .compression("none")
        .no_ring(std::env::var(format!("{HELPER}_PIPE")).is_ok())
        .ring_bytes(64 << 10)
        .recorder_command([recorder_bin().to_string_lossy().into_owned()])
        .build(Box::new(Killer))
        .unwrap();
    let big = vec![b'b'; 200 << 10];
    for (i, data) in [&b"fine"[..], &big, &big, b"fine", b"die"]
        .iter()
        .enumerate()
    {
        rec.step(&Input::new("t", i.to_string(), data.to_vec()))
            .unwrap();
    }
}

/// A service killed right after publishing a step's input still leaves a crash
/// marker and a fixture, over the ring and over the pipe. The inputs larger
/// than the 64 KiB ring are published in pieces and arrive whole.
#[test]
fn killed_service_leaves_crash_fixture() {
    recorder_bin();
    for transport in ["ring", "pipe"] {
        let dir = std::env::temp_dir().join(format!(
            "kavach-rust-ring-{}-{transport}",
            std::process::id()
        ));
        let _ = std::fs::remove_dir_all(&dir);
        let mut cmd = Command::new(std::env::current_exe().unwrap());
        cmd.args(["killed_service_helper", "--exact", "--nocapture"])
            .env(HELPER, &dir)
            .env("KAVACH_RECORDER", recorder_bin());
        if transport == "pipe" {
            cmd.env(format!("{HELPER}_PIPE"), "1");
        }
        let st = cmd.status().unwrap();
        assert!(!st.success(), "the helper should have been killed");

        // The recorder is no child of the test: it finishes on its own.
        let deadline = Instant::now() + Duration::from_secs(10);
        let fixture = loop {
            let found = std::fs::read_dir(dir.join("fixtures"))
                .into_iter()
                .flatten()
                .map(|e| e.unwrap().path())
                .find(|p| p.extension().is_some_and(|e| e == "kavach"));
            match found {
                Some(f) => break f,
                None => {
                    assert!(Instant::now() < deadline, "{transport}: no fixture written");
                    std::thread::sleep(Duration::from_millis(20));
                }
            }
        };
        let bytes = std::fs::read(&fixture).unwrap();
        let has = |needle: &[u8]| bytes.windows(needle.len()).any(|w| w == needle);
        assert!(bytes.len() > 400 << 10, "{transport}: large inputs missing");
        assert!(has(b"crash"), "{transport}: no crash marker");
        assert!(has(b"die"), "{transport}: failing input missing");
        let _ = std::fs::remove_dir_all(&dir);
    }
}

struct Sink;

impl Handler for Sink {
    fn handle(&mut self, env: &mut dyn Env, input: &Input) -> HandlerResult {
        env.emit("out", &input.data, Scope::Remote);
        Ok(())
    }
}

/// A recorder that stops reading while the ring is full does not hang the
/// service: recording stops (SPEC.md section 10.1).
#[test]
fn dead_recorder_ends_the_wait() {
    let mut rec = Recorder::builder("dead")
        .ring_bytes(64 << 10)
        .close_timeout(Duration::from_millis(200))
        .recorder_command(["sleep", "1"])
        .log(|_| {})
        .build(Box::new(Sink))
        .unwrap();
    let start = Instant::now();
    for _ in 0..20 {
        rec.step(&Input::new("t", "p", vec![b'x'; 30 << 10]))
            .unwrap();
    }
    assert!(start.elapsed() < Duration::from_secs(10));
    assert!(!rec.is_recording());
}

#[test]
fn bad_ring_size_falls_back_to_the_pipe() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let l = log.clone();
    let rec = Recorder::builder("t")
        .ring_bytes(1000)
        .recorder_command(["sh", "-c", "cat >/dev/null"])
        .log(move |m| l.lock().unwrap().push(m.to_string()))
        .build(Box::new(Sink))
        .unwrap();
    assert!(rec.is_recording());
    assert!(log
        .lock()
        .unwrap()
        .iter()
        .any(|m| m.contains("over the pipe")));
}
