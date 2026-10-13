# Task A6: tokio-rs__axum-1934

Track A (incident fixes), entry 6 of the sourcing list's best candidates
(`../../sourcing/README.md`). A Rust web framework this time: a `Router`
nested at `/` and used as another router's fallback panics on every request
it serves. Built on the redis-10068 recipe. **No agent has been run on it
yet.**

| | |
| --- | --- |
| Source | SWE-bench Multilingual, `tokio-rs__axum-1934` (MIT) |
| Base commit | `e97462d45251c21ffd1ba4fb5db30f3cf6f6cb6c` (axum 0.6.15, 2023-04-17) |
| Bug | A fallback `Router` sets `MatchedPath` (to its catch-all route) before calling the fallback service. When that service is a `Router` with `nest_service("/", …)`, the nested router finds a `MatchedPath` already present and fails `debug_assert!(matches!(extensions.remove::<MatchedPath>(), None))`. Debug builds only: in release the assertion is compiled out. |
| Hidden test | `routing::tests::fallback::doesnt_panic_if_used_with_nested_router` plus the 15 other tests matching `routing::tests::fallback` (`hidden/test.patch`, from the SWE-bench record) |

## Results

`./run.sh` reproduces the table below from a clean checkout, on the host, in
about 1.5 minutes on an Apple M3 Pro with the crates already in cargo's
cache, most of it the first build of axum's dependencies. On the host it
also replays the committed `incident.kavach`: `still_failing@40`, the same
as the fresh fixture. Measured 2026-10-12 at Kavach `cdfafd3`, Rust 1.92.

**Recording.** The integration is a driver, not a server
(`axum-kavach.patch`, below): each line of `traffic.sh` is one HTTP request,
fed to the `Router` with `tower::ServiceExt::oneshot`. The traffic follows the
report's setup in a small todo service: nine API requests (creates, reads, a
404, a rejected create, a delete), then a browser loading the front end, whose
files are served by a `Router` with `nest_service("/", …)` used as
`fallback_service`. `GET /` is served (it matches the fallback's `/` route,
not the catch-all); `GET /style.css`, the next request, panics. The driver
recovers from the panic, as hyper does when a connection's task panics, and
goes on; `/app.js` and `/favicon.ico` panic too, and the API keeps working.
The recorder cuts a fixture at the first panic.

`incident.kavach` (4.2 KB, 11 steps, 43 records) is that fixture, recorded in
the Kavach arm's image by `record.sh`, so its environment facts are the
container's. **No invariant:** the failure is a panic.

**Replay.** On the old build the fixture replays as `still_failing@40`, with
the panic message
`assertion failed: matches!(extensions.remove::<MatchedPath>(), None)`, every
time.

**Candidate fixes**, each judged by `kavach diff` against the fixture and by
the hidden test, separately. The last column is upstream's own new unit test
`extract::matched_path::tests::cant_extract_in_fallback` (part of the merged
fix, not of the hidden test), with the other `extract::matched_path` tests.

| Candidate | `kavach diff` | Variants that reproduce on the old build / pass on the new | Hidden test | `extract::matched_path` tests (not graded) |
| --- | --- | --- | --- | --- |
| (old build) | — | — | fail | 14 of 15 |
| `upstream`: the merged fix (the fallback's `PathRouter` never sets `MatchedPath`, by a const generic) | `fixed` | 44 of 48 / 44 | pass | 15 of 15 |
| `fallback-flag`: the same behaviour with a runtime `is_fallback` field | `fixed` | 44 of 48 / 44 | pass | 15 of 15 |
| `remove-stale-matched-path`: replace the assertion with a plain `remove` | `fixed` | 44 of 48 / 44 | pass | 14 of 15 |
| `skip-matched-path`: stop setting `MatchedPath` in the path router at all | `diverged@1` | — | pass | 4 of 15 |

The hidden test and Kavach agree on the old build and the three
non-panicking fixes that keep `MatchedPath` for routes. They disagree on one:

- **`skip-matched-path` is wrong and passes the hidden test.** No route sets
  `MatchedPath` any more, which breaks every `MatchedPath` extractor and
  middleware. The hidden tests only check that fallbacks answer; Kavach sees
  the service's per-route metrics label change from `/api/todos` to
  `unmatched` on the first recorded request. This is the case Kavach is for:
  behaviour before the failure is part of the check.
- **`remove-stale-matched-path` is narrow and passes both.** It stops the
  panic but still exposes the fallback's internal catch-all route
  (`/*__private__axum_fallback`) as `MatchedPath` to fallback handlers, which
  upstream also fixed in the same PR. Neither grader can see it: the hidden
  test does not check it, and in the recorded service nothing reads
  `MatchedPath` in the fallback. Only upstream's own unit test catches it.
- **What a crash fixture cannot check.** The failing step has no recorded
  output, so Kavach accepts any non-panicking answer there. A fix that made
  the fallback return 404 would be `fixed` for Kavach and fail the hidden
  test (which expects 200). I did not write one: no fix I could think of does this on purpose.

**Variants.** 48 variants: the failing request's fields changed or removed,
the request moved earlier, earlier requests dropped or delivered twice, the
clock shifted. 44 reproduce: any path the API does not serve, other than `/`,
reaches the catch-all, so the panic does not depend on the rest of the input
or on earlier state. The sourcing ledger's `variants-cannot-reach` gap and
`variants_reach_trigger = ?` do not hold for this wrapper. As in redis-10068,
the variants confirmed fixes and caught nothing the recorded input did not.

## 45-minute assessment

| | |
| --- | --- |
| Incremental rebuild after a one-file change in `axum/src` | 16 s (`cargo test -p axum --lib --no-run`, in the image); the driver then links in 2.5 s more |
| Focused test (`cargo test -p axum --lib -- routing::tests`) | under 1 s once built; 157 tests, `logging_rejections` fails at the base commit (needs the `tracing` feature) |
| Full crate (`cargo test -p axum`, lib + integration + doctests) | about 35 s once built; also fails at the base (4 doctests need features) |
| `kavach diff` | 0.1 s for the fixture, a few seconds with variants |
| Reference fix | 24 added, 19 removed lines in two files (`routing/path_router.rs`, `routing/mod.rs`), almost all of it threading a const generic through `PathRouter`'s impls; the logic is one `if`. Plus a 12-line unit test and a changelog entry. |

Doable in 45 minutes. The panic message and location point straight at
`set_matched_path_for_request`; the work is understanding why a fallback sets
`MatchedPath`, in about 1,300 lines (`routing/mod.rs`,
`routing/path_router.rs`, `extract/matched_path.rs`). The risk is
the opposite: the one-line `remove-stale-matched-path` is the obvious change
and passes the hidden test.

Note for both arms: the test suite has failures at the base commit (above),
named in the task text so that agents don't chase them.

## The integration (`axum-kavach.patch`)

A new workspace member, `examples/kavach-driver` (about 220 lines of Rust),
using the Rust SDK (`sdk/rust`), which the image installs at
`/opt/kavach/sdk/rust`; `run.sh` points the path at this checkout instead. No
line of axum changes.

- **The service** (`src/app.rs`): a todo API (`/api/todos`,
  `/api/todos/:id`) with a `route_layer` metrics middleware that labels each
  request with its `MatchedPath`, as axum's own Prometheus example does, and
  the front end's files (compiled in) served by
  `Router::new().nest_service("/", get_service(…))` as `fallback_service`:
  the report's setup, without `ServeDir`, so the files are not filesystem
  state.
- **Steps.** One request per step: `{"method","uri","headers","body"}` as
  JSON. The driver builds an `http::Request`, calls
  `Router::oneshot` on a current-thread tokio runtime, and emits the
  response (`http.response`: status, headers, body) and the metrics label
  (`metrics.requests`).
- **Clock.** The driver reads the Kavach clock once per request and passes it
  as a request extension; the create handler uses it for `created`. Nothing
  else reads time or randomness.
- **Panics** are recorded (with a backtrace in the marker data) and
  recovered: the driver logs them and serves the next request.
- **Replay.** `kavach-driver kavach-host` serves the host protocol (SDK
  `maybe_host`).

**Known limits.**
- The driver is not hyper: no connection handling, `Date` or
  `content-length` added by the server, or streaming bodies. Bodies are
  recorded as UTF-8 text.
- The panic needs a debug build. Both the recorded service and every build
  in the task are debug builds, as the reporter's was. A release build of the
  old code does not panic, and Kavach would see nothing.
- The panic message is the compiler's rendering of the assertion; Rust 1.92
  renders it differently from the 2023 compiler in the report. The fixture,
  the old build and every grade use the image's Rust 1.92. `run.sh` on a host
  with another Rust version records its own fixture and is self-consistent.
- No snapshots: the todo store is not saved, so the recorder cannot segment.

## Images

`Dockerfile` targets `control`, `kavach` (on `kavach-build`, the same without
the task files, where `record.sh` runs) and `grader`. Rust 1.92 on Debian
bookworm. `Cargo.lock` (axum ignores its lockfile) pins every crate; the image
fetches all of the roughly 520 crates the workspace locks, compiles the axum test target (and
in the Kavach arm the driver), and sets `CARGO_NET_OFFLINE=true`. Checked with
`--network none`. The images are large: 4.9 GB (control), 5.3 GB (kavach),
9.1 GB (grader, two prebuilt trees).

## Harbor

`../../harbor/tasks/axum-1934-{control,kavach}`, as for redis-10068:
separate verifier container (the grader image), collect hook, `solution/`
(the upstream fix), `timeout_sec = 2700`, `network_mode = "no-network"`.

| Run | Agent | Reward | Kavach (verifier) | Time |
| --- | --- | --- | --- | --- |
| `axum-1934-oracle-control` | oracle | 1.0 | `fixed` (44 of 48) | 9m 11s, with image builds |
| `axum-1934-oracle-kavach` | oracle | 1.0 | `fixed` (44 of 48) | 2m 0s |
| `axum-1934-nop-control` | nop | 0.0 | `still_failing@40` | 5m 46s |
| `axum-1934-nop-kavach` | nop | 0.0 | `still_failing@40` | 2m 48s |

Times include building the images where the cache did not have them (the
first run built the grader's trees). No exceptions in any run.

## Files

| File | |
| --- | --- |
| `task.md` | What the agent is given; the control arm gets it without the "Kavach arm only" section |
| `axum-kavach.patch` | The integration, against the base commit |
| `Cargo.lock` | The pinned dependency versions for every build of the task |
| `traffic.sh` | The incident traffic, one request per line |
| `record.sh` | Records `incident.kavach` in the `kavach-build` image |
| `incident.kavach` | The recorded fixture agents receive |
| `candidates/*.patch` | Candidate fixes, applied on top of the integration |
| `hidden/test.patch` | The upstream test, from SWE-bench; never shown to the agent |
| `run.sh` | Builds, records, replays and grades everything above, on the host |
| `Dockerfile` | The agent images for each arm and the grader image |
| `grade.sh` | Grades one attempt in the grader image |
