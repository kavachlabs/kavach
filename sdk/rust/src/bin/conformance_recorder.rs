//! Runs one SDK recorder-conformance case (spec/recorder/sdk/README.md):
//!
//!     kavach-conformance-recorder [--pipe] <case.json> <result.json>
//!
//! Starts the SDK's recorder with the spec's fake recorder as its command,
//! performs the case's actions against the conformance handler, closes the
//! recorder and exits 0 if the fake's verdict file says `pass`.

use std::cell::RefCell;
use std::collections::VecDeque;
use std::process::ExitCode;
use std::rc::Rc;
use std::time::Duration;

use kavach::conformance::{spec_dir, Conformance};
use kavach::json::{self, Value};
use kavach::{b64, GatewayError, Input, Recorder};

#[derive(Default)]
struct Answers {
    clock: VecDeque<i64>,
    rand: VecDeque<Vec<u8>>,
    gateway: VecDeque<Result<Vec<u8>, String>>,
    config: VecDeque<Option<Vec<u8>>>,
}

fn load(a: &Value) -> Result<Answers, String> {
    let mut out = Answers::default();
    let list = |k: &str| a.get(k).and_then(Value::as_array).unwrap_or(&[]).to_vec();
    for v in list("clock") {
        out.clock.push_back(
            v.as_str()
                .and_then(|s| s.parse().ok())
                .ok_or("bad clock answer")?,
        );
    }
    for v in list("rand") {
        out.rand
            .push_back(v.as_str().and_then(b64::decode).ok_or("bad rand answer")?);
    }
    for v in list("gateway") {
        out.gateway
            .push_back(match (v.str_field("response"), v.str_field("error")) {
                (_, Some(e)) => Err(e.to_string()),
                (Some(r), None) => Ok(b64::decode(r).ok_or("bad gateway response")?),
                _ => return Err("gateway answer has neither response nor error".into()),
            });
    }
    for v in list("config") {
        out.config.push_back(match v.str_field("value") {
            Some(s) => Some(b64::decode(s).ok_or("bad config value")?),
            None => None,
        });
    }
    Ok(out)
}

fn run(case_path: &str, result_path: &str, no_ring: bool) -> Result<(), String> {
    let case = json::parse(&std::fs::read_to_string(case_path).map_err(|e| e.to_string())?)?;
    let open = case.get("open").ok_or("case has no open")?;
    let answers = Rc::new(RefCell::new(Answers::default()));

    let fake = spec_dir().join("recorder/sdk/fake_recorder.py");
    let mut b = Recorder::builder(open.str_field("service").unwrap_or("conformance"))
        .recorder_command([
            "python3".to_string(),
            fake.to_string_lossy().into_owned(),
            case_path.to_string(),
            result_path.to_string(),
        ])
        .required(true)
        .no_ring(no_ring)
        .snapshots(
            open.get("snapshots")
                .and_then(Value::as_bool)
                .unwrap_or(false),
        )
        .recover_panics(true);
    let a = answers.clone();
    b = b.clock(move || {
        a.borrow_mut()
            .clock
            .pop_front()
            .expect("no clock answer left")
    });
    let a = answers.clone();
    b = b.rand(move |buf| {
        let r = a
            .borrow_mut()
            .rand
            .pop_front()
            .expect("no rand answer left");
        assert_eq!(r.len(), buf.len(), "rand answer has the wrong length");
        buf.copy_from_slice(&r);
    });
    let a = answers.clone();
    b = b.gateways(
        Conformance::gateways().fallback(kavach::Scope::Remote, move |_, _| {
            a.borrow_mut()
                .gateway
                .pop_front()
                .expect("no gateway answer left")
                .map_err(GatewayError::new)
        }),
    );
    let a = answers.clone();
    b = b.config("launchdarkly", move |_| {
        a.borrow_mut()
            .config
            .pop_front()
            .expect("no config answer left")
    });
    if let Some(flags) = case.get("flags").and_then(Value::as_object) {
        let flags: Vec<(String, Vec<u8>)> = flags
            .iter()
            .map(|(k, v)| (k.clone(), v.as_str().unwrap_or("").as_bytes().to_vec()))
            .collect();
        b = b.flags(move || flags.clone());
    }
    if let Some(s) = case.str_field("snapshot") {
        b = b.start_from_snapshot(s.as_bytes().to_vec());
    }
    let mut rec = b
        .build(Box::new(Conformance::new()))
        .map_err(|e| e.to_string())?;

    for action in case
        .get("actions")
        .and_then(Value::as_array)
        .ok_or("case has no actions")?
    {
        if let Some(step) = action.get("step") {
            *answers.borrow_mut() = load(action.get("answers").unwrap_or(&Value::Null))?;
            let data = step
                .str_field("data")
                .and_then(b64::decode)
                .ok_or("step has no data")?;
            let input = Input::new(
                step.str_field("source").unwrap_or(""),
                step.str_field("position").unwrap_or(""),
                data,
            );
            let _ = rec.step(&input); // a failing step is not an error for the runner
        } else if let Some(f) = action.get("flush") {
            let durable = f.get("durable").and_then(Value::as_bool).unwrap_or(false);
            if !rec.flush(durable, Duration::from_secs(10)) {
                return Err("flush was not acknowledged".into());
            }
        } else {
            return Err("unknown action".into());
        }
    }
    if !rec.close() {
        return Err("the recorder did not answer closed".into());
    }

    let result = json::parse(
        &std::fs::read_to_string(result_path).map_err(|e| format!("no verdict: {e}"))?,
    )?;
    if result.get("pass").and_then(Value::as_bool) == Some(true) {
        let want = if no_ring { "pipe" } else { "ring" };
        match result.str_field("transport") {
            Some(got) if got == want => Ok(()),
            got => Err(format!(
                "the case ran over the {}, want the {want}",
                got.unwrap_or("unknown transport")
            )),
        }
    } else {
        Err(result
            .str_field("error")
            .unwrap_or("the fake recorder reported a failure")
            .to_string())
    }
}

fn main() -> ExitCode {
    let mut args: Vec<String> = std::env::args().collect();
    // The ring is the default transport; --pipe forces the pipe.
    let no_ring = args.get(1).is_some_and(|a| a == "--pipe");
    if no_ring {
        args.remove(1);
    }
    if args.len() != 3 {
        eprintln!("usage: {} [--pipe] <case.json> <result.json>", args[0]);
        return ExitCode::from(2);
    }
    match run(&args[1], &args[2], no_ring) {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("FAIL {}: {e}", args[1]);
            ExitCode::FAILURE
        }
    }
}
