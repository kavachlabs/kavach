//! Recorder overhead per journaled event, the Rust counterpart of Go's
//! `BenchmarkRecorderStep` (replay/replay_test.go): the same handler shape and
//! input, over the pipe and over the ring, against the real `kavach-recorder`.
//!
//!     go build -o /tmp/kavach-recorder ./cmd/kavach-recorder
//!     KAVACH_RECORDER=/tmp/kavach-recorder cargo run --release --example bench_step
//!
//! Each step journals 4 events: input, clock, rand, output. Prints the median
//! ns/event of 5 runs per transport.

use std::time::Instant;

use kavach::{Env, Handler, HandlerResult, Input, Recorder, Scope, Snapshotter};

const STEPS: u32 = 200_000;
const RUNS: usize = 5;

struct Bench;

impl Handler for Bench {
    fn handle(&mut self, env: &mut dyn Env, input: &Input) -> HandlerResult {
        env.now_nanos();
        let mut id = [0u8; 8];
        env.fill_random(&mut id);
        env.emit("entries", &input.data, Scope::Remote);
        Ok(())
    }

    fn snapshotter(&mut self) -> Option<&mut dyn Snapshotter> {
        Some(self)
    }
}

impl Snapshotter for Bench {
    fn snapshot(&mut self) -> Result<Vec<u8>, Box<dyn std::error::Error>> {
        Ok(b"{}".to_vec())
    }

    fn restore(&mut self, _: &[u8]) -> Result<(), Box<dyn std::error::Error>> {
        Ok(())
    }
}

fn run(no_ring: bool) -> f64 {
    let dir = std::env::temp_dir().join(format!("kavach-bench-{}", std::process::id()));
    // A seeded xorshift, so that every run records the same bytes.
    let mut state = 1u64;
    let mut rec = Recorder::builder("bench")
        .dir(dir.to_string_lossy())
        .no_ring(no_ring)
        .required(true)
        .rand(move |buf| {
            for b in buf {
                state ^= state << 13;
                state ^= state >> 7;
                state ^= state << 17;
                *b = state as u8;
            }
        })
        .build(Box::new(Bench))
        .expect("starting kavach-recorder");
    let input = Input::new("bench", "0", br#"{"account":"alice","amount":10}"#.to_vec());
    let start = Instant::now();
    for _ in 0..STEPS {
        rec.step(&input).unwrap();
    }
    let elapsed = start.elapsed();
    rec.close();
    let _ = std::fs::remove_dir_all(&dir);
    elapsed.as_nanos() as f64 / (STEPS as f64 * 4.0)
}

fn main() {
    for (name, no_ring) in [("pipe", true), ("ring", false)] {
        let mut runs: Vec<f64> = (0..RUNS).map(|_| run(no_ring)).collect();
        runs.sort_by(f64::total_cmp);
        println!(
            "{name}: median {:.1} ns/event (runs: {})",
            runs[RUNS / 2],
            runs.iter()
                .map(|r| format!("{r:.1}"))
                .collect::<Vec<_>>()
                .join(" ")
        );
    }
}
