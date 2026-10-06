# Benchmarks

Every number Kavach publishes comes from this file, and every number here comes
from a command you can run. Numbers are re-measured when the code they describe
changes.

## Environment

Measured 2026-10-06 on a cloud VM: 4 vCPU Intel Xeon @ 2.10 GHz, Linux,
Go 1.24.7. Laptop numbers will differ; the commands are below.

## Results

| Metric | Target | Measured | Command |
| --- | --- | --- | --- |
| Recorder overhead, happy path | < 100 ns/event | median 82.6 ns/event; range 79.0–102.8 over 5 runs (one run over target) | `go test -run '^$' -bench RecorderStep -benchtime 2s -count 5 .` |
| Replay of demo crash fixture, in-process | — | median 61.1 µs; range 59.7–67.3 µs over 5 runs | `go test -run '^$' -bench ReplayFixture -benchtime 2s -count 5 ./examples/ledger` |
| Replay of demo crash fixture, CLI end to end | < 50 ms | median 5.9 ms, p95 9.1 ms (100 runs) | see below |
| Fix verification of the demo crash, in-process (2 replays + 44 variants old + 41 new) | — | median 5.9 ms; range 5.7–6.2 ms over 5 runs | `go test -run '^$' -bench VerifyFixture -benchtime 2s -count 5 ./examples/ledger` |
| Fix verification of the demo crash, `kavach diff` end to end (87 binary runs, 4 in parallel) | — | correct fix: median 196.4 ms, p95 224.3 ms; partial fix: median 215.7 ms, p95 273.5 ms (50 runs each) | see below |
| Variants of the demo crash that reproduce it on the old build | ≥ 10 | 41 of 44 | `go test -run VerifyCandidateFixes -v ./examples/ledger` |
| Wrong fixes rejected by variants (demo crash) | — | 2 of 2 (`skip evt-008`: 5 variants fail; `deposits only`: 4 fail); correct fix passes 41 of 41 | same |
| Demo crash fixture size | < 100 KB | 3,815 bytes | `wc -c examples/ledger/testdata/null-amount.kavach` |
| Determinism | 1,000 replays byte-identical | 1,000 / 1,000 | `go test -run Determinism ./examples/ledger` |
| Test coverage, core packages | ≥ 80% | `kavach` 83.3%, `journal` 85.8% | `go test -cover . ./journal` |

## What each number includes

**Recorder overhead.** One `Recorder.Step` journals four events (input, clock
read, 8-byte random read, output) for a 33-byte input; ns/event is the step time
divided by four. It includes `time.Now`, copying the input and output, and the
recorder's mutex. It does not include crypto/rand: the benchmark injects a
seeded `math/rand` source, so the number measures Kavach rather than the
kernel's random number generator. The VM is shared, so run-to-run noise is
large; the median is the number to quote, and the target is not yet met with
margin. The window is bounded by snapshots every 1,000
steps, as in production. 4 allocations and 80 bytes per step.

**In-process replay.** `kavach.ReplayFile` on the demo fixture: read and decode
the 33-record file, replay 8 steps through the ledger handler, check two
invariants per step, compare outputs.

**CLI end to end.** Wall time of
`kavach replay examples/ledger/testdata/null-amount.kavach --bin <ledger>`,
from process start to exit: starting the CLI, starting the replay binary,
replaying, writing and reading the JSON result. Measured with:

```bash
go build -o /tmp/kavach ./cmd/kavach && go build -o /tmp/ledger ./examples/ledger
python3 - <<'EOF'
import subprocess, time, statistics
ts = []
for _ in range(100):
    t = time.perf_counter()
    subprocess.run(["/tmp/kavach", "replay", "examples/ledger/testdata/null-amount.kavach",
                    "--bin", "/tmp/ledger"], stdout=subprocess.DEVNULL)
    ts.append((time.perf_counter() - t) * 1000)
ts.sort()
print(f"median {statistics.median(ts):.1f} ms  p95 {ts[94]:.1f} ms")
EOF
```

**Fix verification.** In process, `kavach.Verify` with the planted-bug ledger as
the old build and the correct nil check as the new one, run serially. End to
end, wall time of `kavach diff` from process start to exit, which starts a
replay binary for each of the 87 replays, at most one per CPU at a time:

```bash
go build -o /tmp/ledger-new -tags ledgerfix ./examples/ledger
go build -o /tmp/ledger-partial -tags ledgerpartialfix ./examples/ledger
# then time, as above:
#   kavach diff examples/ledger/testdata/null-amount.kavach --old /tmp/ledger --new /tmp/ledger-new
#   kavach diff examples/ledger/testdata/null-amount.kavach --old /tmp/ledger --new /tmp/ledger-partial --keep /tmp/v
```

**Determinism.** The `Result` of each replay, serialized as JSON, is compared
byte for byte against the first. It holds with the planted bug and with
`-tags ledgerfix`.
