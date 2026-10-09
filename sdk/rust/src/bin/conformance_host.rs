//! The conformance host: `spec/host/run.py --host <this binary>` plays the
//! host-protocol transcripts against it (SPEC.md section 9.6).

use kavach::conformance::Conformance;

fn main() {
    kavach::maybe_host(|| Box::new(Conformance::new()), Conformance::host_options());
    eprintln!("usage: kavach-conformance-host kavach-host   (speaks the host protocol on stdin and stdout)");
    std::process::exit(2);
}
