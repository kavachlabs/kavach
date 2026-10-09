//! Failure behavior of the recorder that the spec's cases do not cover.

use std::sync::{Arc, Mutex};

use kavach::{Env, Handler, HandlerResult, Input, Recorder, RecorderError, Scope, StepError};

type Body = Box<dyn FnMut(&mut dyn Env) -> HandlerResult>;

struct Func(Body);

impl Handler for Func {
    fn handle(&mut self, env: &mut dyn Env, _: &Input) -> HandlerResult {
        (self.0)(env)
    }
}

fn input() -> Input {
    Input::new("test", "0", b"x".to_vec())
}

fn logger() -> (
    Arc<Mutex<Vec<String>>>,
    impl Fn(&str) + Send + Sync + 'static,
) {
    let log = Arc::new(Mutex::new(Vec::new()));
    let l = log.clone();
    (log, move |m: &str| l.lock().unwrap().push(m.to_string()))
}

#[test]
fn missing_recorder_does_not_fail_steps() {
    let (log, f) = logger();
    let mut rec = Recorder::builder("t")
        .recorder_command(["/nonexistent/kavach-recorder"])
        .log(f)
        .build(Box::new(Func(Box::new(|env| {
            env.emit("s", b"d", Scope::Remote);
            Ok(())
        }))))
        .unwrap();
    assert!(!rec.is_recording());
    rec.step(&input()).unwrap();
    assert!(log
        .lock()
        .unwrap()
        .iter()
        .any(|m| m.contains("cannot start the recorder")));
}

#[test]
fn required_recorder_fails_startup() {
    let r = Recorder::builder("t")
        .recorder_command(["/nonexistent/kavach-recorder"])
        .required(true)
        .build(Box::new(Func(Box::new(|_| Ok(())))));
    assert!(matches!(r, Err(RecorderError::Spawn(_))));
}

#[test]
fn broken_pipe_stops_recording_but_not_the_service() {
    let (log, f) = logger();
    let delivered = Arc::new(Mutex::new(0));
    let d = delivered.clone();
    let mut rec = Recorder::builder("t")
        .close_timeout(std::time::Duration::from_millis(200))
        .recorder_command(["true"]) // exits at once, closing the pipe
        .log(f)
        .deliver(move |outs| {
            *d.lock().unwrap() += outs.len();
            Ok(())
        })
        .build(Box::new(Func(Box::new(|env| {
            env.emit("s", &[0u8; 1 << 16], Scope::Remote);
            Ok(())
        }))))
        .unwrap();
    std::thread::sleep(std::time::Duration::from_millis(200));
    for _ in 0..3 {
        rec.step(&input()).unwrap();
    }
    assert!(!rec.is_recording());
    assert_eq!(*delivered.lock().unwrap(), 3);
    assert!(log
        .lock()
        .unwrap()
        .iter()
        .any(|m| m.contains("recording is off") || m.contains("recorder stopped")));
}

#[test]
fn failures_are_reported_and_outputs_withheld() {
    let delivered = Arc::new(Mutex::new(0));
    let d = delivered.clone();
    let mut n = 0;
    let mut rec = Recorder::builder("t")
        .recorder_command(["sh", "-c", "cat >/dev/null"])
        .close_timeout(std::time::Duration::from_millis(200))
        .deliver(move |outs| {
            *d.lock().unwrap() += outs.len();
            Ok(())
        })
        .recover_panics(true)
        .build(Box::new(Func(Box::new(move |env| {
            env.emit("s", b"d", Scope::Remote);
            n += 1;
            match n {
                1 => Err("bad input".into()),
                2 => kavach::panic("boom"),
                3 => panic!("formatted {}", 7),
                _ => Ok(()),
            }
        }))))
        .unwrap();
    assert!(
        matches!(rec.step(&input()), Err(StepError::Handler(e)) if e.to_string() == "bad input")
    );
    assert!(matches!(rec.step(&input()), Err(StepError::Panic { message }) if message == "boom"));
    assert!(
        matches!(rec.step(&input()), Err(StepError::Panic { message }) if message == "formatted 7")
    );
    rec.step(&input()).unwrap();
    assert_eq!(*delivered.lock().unwrap(), 1);
}

#[test]
fn panics_keep_unwinding_by_default() {
    let mut rec = Recorder::builder("t")
        .recorder_command(["sh", "-c", "cat >/dev/null"])
        .close_timeout(std::time::Duration::from_millis(200))
        .build(Box::new(Func(Box::new(|_| std::panic::panic_any(42u8)))))
        .unwrap();
    let r = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| rec.step(&input())));
    assert_eq!(r.unwrap_err().downcast_ref::<u8>(), Some(&42));
}
