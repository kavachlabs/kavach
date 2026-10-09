//! Kavach's demo service in Rust: a single-writer wallet ledger that folds a
//! stream of JSON events into balances. A port of `examples/ledger` (Go).
//!
//!     cargo run --example ledger -- --in ../../examples/ledger/testdata/events.jsonl
//!     cargo run --example ledger -- --fixed --in ../../examples/ledger/testdata/events.jsonl
//!
//! One upstream event has `"amount": null`, which crashes the buggy handler; the
//! flight recorder has written the step's input and the panic marker before the
//! process dies, and `kavach-recorder` cuts a fixture from them. `--fixed`
//! selects the fixed handler. (A command-line flag, not an environment
//! variable: environment variables are served from the journal on replay.)
//!
//! Run as `ledger [--fixed] kavach-host`, the binary is a replay host.

use std::collections::BTreeMap;
use std::error::Error;
use std::io::{BufRead, BufReader};

use kavach::{
    Checker, Env, Handler, HandlerResult, HostOptions, Input, Invariant, Recorder, Scope,
    Snapshotter, StepError,
};
use serde_json::Value;

struct Ledger {
    /// Selects the fixed handler. Set from the command line.
    fixed: bool,
    balances: BTreeMap<String, i64>,
    /// Deposits minus withdrawals.
    net: i64,
}

impl Ledger {
    fn new(fixed: bool) -> Ledger {
        Ledger {
            fixed,
            balances: BTreeMap::new(),
            net: 0,
        }
    }

    fn post(&mut self, env: &mut dyn Env, event: &str, account: &str, delta: i64, at: &str) {
        let balance = {
            let b = self.balances.entry(account.to_string()).or_insert(0);
            *b += delta;
            *b
        };
        let mut txn = [0u8; 8];
        env.fill_random(&mut txn);
        let txn: String = txn.iter().map(|b| format!("{b:02x}")).collect();
        let entry = format!(
            "{{\"txn\":{},\"event\":{},\"account\":{},\"delta\":{delta},\"balance\":{balance},\"at\":{}}}",
            q(&txn),
            q(event),
            q(account),
            q(at)
        );
        env.emit("ledger.entries", entry.as_bytes(), Scope::Remote);
    }
}

fn q(s: &str) -> String {
    Value::from(s).to_string()
}

fn reject(env: &mut dyn Env, event: &str, reason: &str) -> HandlerResult {
    let r = format!("{{\"event\":{},\"reason\":{}}}", q(event), q(reason));
    env.emit("ledger.rejections", r.as_bytes(), Scope::Remote);
    Ok(())
}

impl Handler for Ledger {
    fn handle(&mut self, env: &mut dyn Env, input: &Input) -> HandlerResult {
        let ev: Value = serde_json::from_slice(&input.data)
            .map_err(|e| format!("decode event at {}: {e}", input.position))?;
        let text = |k: &str| ev.get(k).and_then(Value::as_str).unwrap_or("").to_string();
        let (id, kind, account, to) = (text("id"), text("type"), text("account"), text("to"));
        let amount = ev.get("amount").and_then(Value::as_i64); // None when null or missing

        if self.fixed && amount.is_none() {
            return reject(env, &id, "missing amount");
        }
        // The bug: an event with "amount": null panics here, and the service dies.
        let amount = amount.unwrap();
        if amount <= 0 {
            return reject(env, &id, "amount must be positive");
        }
        let at = rfc3339_nano(env.now_nanos());

        match kind.as_str() {
            "deposit" => {
                self.net += amount;
                self.post(env, &id, &account, amount, &at);
            }
            "withdraw" => {
                if self.balances.get(&account).copied().unwrap_or(0) < amount {
                    return reject(env, &id, "insufficient funds");
                }
                self.net -= amount;
                self.post(env, &id, &account, -amount, &at);
            }
            "transfer" => {
                if self.balances.get(&account).copied().unwrap_or(0) < amount {
                    return reject(env, &id, "insufficient funds");
                }
                self.post(env, &id, &account, -amount, &at);
                self.post(env, &id, &to, amount, &at);
            }
            other => return reject(env, &id, &format!("unknown event type {other}")),
        }
        Ok(())
    }

    fn snapshotter(&mut self) -> Option<&mut dyn Snapshotter> {
        Some(self)
    }

    fn checker(&self) -> Option<&dyn Checker> {
        Some(self)
    }
}

impl Snapshotter for Ledger {
    fn snapshot(&mut self) -> Result<Vec<u8>, Box<dyn Error>> {
        let balances: serde_json::Map<String, Value> = self
            .balances
            .iter()
            .map(|(k, v)| (k.clone(), Value::from(*v)))
            .collect();
        Ok(serde_json::to_vec(
            &serde_json::json!({ "balances": balances, "net": self.net }),
        )?)
    }

    fn restore(&mut self, data: &[u8]) -> Result<(), Box<dyn Error>> {
        let v: Value = serde_json::from_slice(data)?;
        self.net = v
            .get("net")
            .and_then(Value::as_i64)
            .ok_or("snapshot has no net")?;
        self.balances = v
            .get("balances")
            .and_then(Value::as_object)
            .ok_or("snapshot has no balances")?
            .iter()
            .filter_map(|(k, v)| v.as_i64().map(|n| (k.clone(), n)))
            .collect();
        Ok(())
    }
}

impl Checker for Ledger {
    fn invariants(&self) -> Vec<Invariant<'_>> {
        vec![
            Invariant::new("balances_non_negative", || {
                match self.balances.iter().find(|(_, b)| **b < 0) {
                    Some((a, b)) => Err(format!("account {a} has balance {b}")),
                    None => Ok(()),
                }
            }),
            Invariant::new("money_conserved", || {
                let sum: i64 = self.balances.values().sum();
                if sum == self.net {
                    Ok(())
                } else {
                    Err(format!(
                        "balances sum to {sum}, deposits minus withdrawals is {}",
                        self.net
                    ))
                }
            }),
        ]
    }
}

/// Formats Unix nanoseconds like Go's `time.RFC3339Nano` in UTC.
fn rfc3339_nano(nanos: i64) -> String {
    let secs = nanos.div_euclid(1_000_000_000);
    let frac = nanos.rem_euclid(1_000_000_000);
    let (days, rem) = (secs.div_euclid(86_400), secs.rem_euclid(86_400));
    // Civil date from days since 1970-01-01 (Howard Hinnant's algorithm).
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = yoe + era * 400 + i64::from(m <= 2);
    let mut s = format!(
        "{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}",
        rem / 3600,
        rem % 3600 / 60,
        rem % 60
    );
    if frac != 0 {
        s.push('.');
        s.push_str(format!("{frac:09}").trim_end_matches('0'));
    }
    s.push('Z');
    s
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let fixed = args.iter().any(|a| a == "--fixed");

    // Lets the kavach CLI use this binary as a replay host: `ledger [--fixed] kavach-host`.
    kavach::maybe_host(move || Box::new(Ledger::new(fixed)), HostOptions::new());

    let value = |flag: &str| {
        args.iter()
            .position(|a| a == flag)
            .and_then(|i| args.get(i + 1))
            .cloned()
    };
    let Some(path) = value("--in") else {
        eprintln!("usage: ledger [--fixed] [--dir DIR] --in FILE.jsonl");
        std::process::exit(2);
    };
    let dir = value("--dir").unwrap_or_else(|| "kavach".to_string());

    let mut rec = Recorder::builder("ledger")
        .dir(dir)
        .deliver(|outs| {
            for o in outs {
                println!("{:<18} {}", o.sink, String::from_utf8_lossy(&o.data));
            }
            Ok(())
        })
        .build(Box::new(Ledger::new(fixed)))
        .unwrap_or_else(|e| {
            eprintln!("ledger: {e}");
            std::process::exit(1);
        });

    let file = std::fs::File::open(&path).unwrap_or_else(|e| {
        eprintln!("ledger: {e}");
        std::process::exit(1);
    });
    let name = std::path::Path::new(&path)
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    for (i, line) in BufReader::new(file).lines().enumerate() {
        let line = line.expect("reading events");
        if line.is_empty() {
            continue;
        }
        let input = Input::new(
            format!("file:{name}"),
            (i + 1).to_string(),
            line.into_bytes(),
        );
        // A panic is recorded and then keeps unwinding: the service dies, as in
        // the Go demo. Errors and invariant failures are logged and skipped.
        match rec.step(&input) {
            Ok(()) => {}
            Err(StepError::Deliver(e)) => eprintln!("ledger: delivering outputs: {e}"),
            Err(e) => eprintln!("ledger: {} @ {}: {e}", input.source, input.position),
        }
    }
    rec.close();
}
