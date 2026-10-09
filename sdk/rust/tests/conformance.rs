//! Runs the spec's host transcripts and SDK recorder cases (SPEC.md sections
//! 9.6 and 10.6) against the conformance programs. The spec is read from
//! `KAVACH_SPEC_DIR` (default: the repository's `spec/`); python3 runs
//! `spec/host/run.py` and the fake recorder.

use std::path::PathBuf;
use std::process::Command;

use kavach::conformance::spec_dir;

fn spec() -> PathBuf {
    let d = spec_dir();
    assert!(
        d.join("host/run.py").exists(),
        "no spec at {}; set KAVACH_SPEC_DIR",
        d.display()
    );
    d
}

#[test]
fn host_transcripts() {
    let host = env!("CARGO_BIN_EXE_kavach-conformance-host");
    let out = Command::new("python3")
        .arg("-I")
        .arg(spec().join("host/run.py"))
        .args(["--host", host])
        .output()
        .expect("running python3");
    let stdout = String::from_utf8_lossy(&out.stdout);
    println!("{stdout}{}", String::from_utf8_lossy(&out.stderr));
    assert!(out.status.success(), "host transcripts failed:\n{stdout}");
    // "N/N transcripts passed": the spec may gain transcripts; at least 16 exist.
    let summary = stdout.lines().last().unwrap_or("");
    let passed: usize = summary
        .split('/')
        .next()
        .and_then(|n| n.parse().ok())
        .unwrap_or(0);
    assert!(
        passed >= 16 && summary == format!("{passed}/{passed} transcripts passed"),
        "unexpected summary {summary:?}"
    );
}

#[test]
fn recorder_cases() {
    let runner = env!("CARGO_BIN_EXE_kavach-conformance-recorder");
    let dir = spec().join("recorder/sdk");
    let tmp = std::env::temp_dir().join(format!("kavach-rust-conformance-{}", std::process::id()));
    std::fs::create_dir_all(&tmp).unwrap();
    let mut cases: Vec<_> = std::fs::read_dir(&dir)
        .unwrap()
        .map(|e| e.unwrap().path())
        .filter(|p| p.extension().is_some_and(|e| e == "json"))
        .collect();
    cases.sort();
    assert!(
        cases.len() >= 7,
        "expected the spec's recorder cases in {}",
        dir.display()
    );

    let mut failures = Vec::new();
    for case in &cases {
        let name = case.file_stem().unwrap().to_string_lossy().into_owned();
        let result = tmp.join(format!("{name}.result.json"));
        let out = Command::new(runner)
            .arg(case)
            .arg(&result)
            .output()
            .expect("running the case runner");
        let ok = out.status.success();
        println!("{} {name}", if ok { "PASS" } else { "FAIL" });
        if !ok {
            failures.push(format!("{name}:\n{}", String::from_utf8_lossy(&out.stderr)));
        }
    }
    let _ = std::fs::remove_dir_all(&tmp);
    assert!(
        failures.is_empty(),
        "failing cases:\n{}",
        failures.join("\n")
    );
}
