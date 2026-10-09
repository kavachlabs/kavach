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
| Recorder overhead, happy path | < 100 ns/event | median 81.2 ns/event; range 80.8–82.0 over 5 runs (Apple M3 Pro, macOS, shared-memory ring). Over the pipe: median 493.7 ns/event; range 486.8–497.9 | `go test -run '^$' -bench RecorderStep -benchtime 2s -count 5 ./replay` |
| Replay of demo crash fixture, in-process | — | median 61.1 µs; range 59.7–67.3 µs over 5 runs | `go test -run '^$' -bench ReplayFixture -benchtime 2s -count 5 ./examples/ledger` |
| Replay of demo crash fixture, CLI end to end | < 50 ms | median 5.9 ms, p95 9.1 ms (100 runs) | see below |
| Fix verification of the demo crash, in-process (2 replays + 44 variants old + 41 new) | — | median 5.9 ms; range 5.7–6.2 ms over 5 runs | `go test -run '^$' -bench VerifyFixture -benchtime 2s -count 5 ./examples/ledger` |
| Fix verification of the demo crash, `kavach diff` end to end (87 binary runs, 4 in parallel) | — | correct fix: median 196.4 ms, p95 224.3 ms; partial fix: median 215.7 ms, p95 273.5 ms (50 runs each) | see below |
| Variants of the demo crash that reproduce it on the old build | ≥ 10 | 41 of 44 | `go test -run VerifyCandidateFixes -v ./examples/ledger` |
| Wrong fixes rejected by variants (demo crash) | — | 2 of 2 (`skip evt-008`: 5 variants fail; `deposits only`: 4 fail); correct fix passes 41 of 41 | same |
| Ten planted bugs (panic, returned error, invariant; see below): old build reproduces the recorded failure | 10 of 10 | 10 of 10 | `go test ./bench -run TestBenchmark -v` |
| Same ten: correct fix verified as `fixed` | 10 of 10 | 10 of 10 (255 of 340 variants reproduce on the old build, 25.5 per bug on average; each fix passes all it is shown) | same |
| Same ten: narrow fix rejected | — | 10 of 10, all `variant_failed` | same |
| Demo crash fixture size | < 100 KB | 1,975 bytes | `wc -c examples/ledger/testdata/null-amount.kavach` |
| Determinism | 1,000 replays byte-identical | 1,000 / 1,000 | `go test -run Determinism ./examples/ledger` |
| Test coverage, core packages | ≥ 80% | `sdk/go` with `replay` 80.3%, `journal` 86.8% | `go test -coverpkg=./sdk/go,./replay -cover ./replay` and `go test -cover ./journal` |

## What each number includes

**Recorder overhead.** One `Recorder.Step` journals four events (input, clock
read, 8-byte random read, output) for a 33-byte input; ns/event is the step time
divided by four. It includes `time.Now`, encoding the frames straight into a
reused buffer, and publishing them into the shared-memory ring of SPEC.md
§10.7: the `input` frame before the handler runs, then the rest of the step
with its `step_end`. Publishing is a copy into the mapped ring and a release
store; no system call is made unless the ring is more than half full. The
recorder process, which numbers, compresses and writes the journal, runs
alongside on other cores and is not counted. The figure is a steady rate, not a
burst: the 8 MiB ring fills within milliseconds, so it also includes any wait
for the recorder to make room, and it holds only because the recorder takes
less than the step's 330 ns to process a step. With `NoRing` the same benchmark uses the pipe, where each of the two
publishes is a `write` system call; that is the 494 ns/event above. It does not
include crypto/rand: the benchmark injects a seeded `math/rand` source, so the
number measures Kavach rather than the kernel's random number generator. The
median is the number to quote. 1 allocation and 8 bytes per step (the
handler's own random buffer).

**In-process replay.** `replay.RunFile` on the demo fixture: read and decode
the 33-record file, replay 8 steps through the ledger handler, check two
invariants per step, compare outputs.

**CLI end to end.** Wall time of
`kavach replay examples/ledger/testdata/null-amount.kavach --bin <ledger>`,
from process start to exit: starting the CLI, starting the host process,
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

**Fix verification.** In process, `replay.Verify` with the planted-bug ledger as
the old build and the correct nil check as the new one, run serially. End to
end, wall time of `kavach diff` from process start to exit, which starts a
host process for each of the 87 replays, at most one per CPU at a time:

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

**Ten planted bugs.** [`bench/`](bench) holds ten small handlers, each with one
planted bug: nil pointer, index out of range, division by zero, nil map write,
type assertion, an oversold capacity invariant, an update delivered before its
create, a redelivered charge, a leap-day array index, and an exhausted random
pool. For each, `bench.Record` runs a short event stream through the flight
recorder with a stepped clock and seeded randomness until the last event fails,
and `replay.Verify` then checks two candidate fixes against that fixture: a
correct one that handles every input of the failing kind, and a narrow one that
handles the recorded incident but not its siblings (for example, guarding only
`signup` events when `update` events carry the same null). `-v` prints one row
per bug, with the verdicts. Each bug's story is in `bench/bugs.go`.

What this does and does not show. The bugs, fixtures and both fixes were
written by this project, in-process rather than through the CLI, so the result
measures whether variants catch narrow fixes that differ from the correct fix
along an axis variants perturb (an event type, a field value, the clock, an
earlier input). It does not show how often an agent's real fixes are narrow in
ways variants cannot see; a fix that differs from a correct one only on inputs
no variant produces would pass. The first draft of the correct fix for the
type-assertion bug validated `qty` but not `price`, and was rejected by the
variant that removes `price`; it was fixed in the benchmark, and the numbers
above are for the fixed version. Each narrow fix is run once, and none
passed, so there is no false-accept rate yet beyond "0 of 10".

**End to end.** `go test ./bench -run EndToEnd -v` repeats the ten bugs through
real binaries instead of in-process calls: each bug is built as a service
(`bench/cmd/benchsvc`) in its buggy, fixed and narrow forms, the buggy build
records its own fixture, and the `kavach` CLI inspects it, replays it, accepts
the correct fix, rejects the narrow fix and saves a failing variant that still
fails the narrow build and passes the correct one. The same verdicts are then
read back over `kavach mcp`. It builds 31 binaries (about 7 s here) and is
skipped under `-short`.

