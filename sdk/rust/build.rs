//! Captures the compiler version for `host.runtime` (SPEC.md section 4.8).

use std::process::Command;

fn main() {
    println!("cargo:rerun-if-changed=build.rs");
    let rustc = std::env::var("RUSTC").unwrap_or_else(|_| "rustc".into());
    let out = Command::new(rustc).arg("--version").output();
    let runtime = out
        .ok()
        .and_then(|o| String::from_utf8(o.stdout).ok())
        .and_then(|s| {
            // "rustc 1.92.0 (ded5c06cf 2025-12-08)" -> "rustc-1.92.0"
            let mut it = s.split_whitespace();
            Some(format!("{}-{}", it.next()?, it.next()?))
        })
        .unwrap_or_else(|| "rustc-unknown".into());
    println!("cargo:rustc-env=KAVACH_RUNTIME={runtime}");
}
