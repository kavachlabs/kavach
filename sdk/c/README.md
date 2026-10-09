# Kavach C SDK and C++ wrapper

`libkavach` is a C11 library (static and shared) that lets a service written in
C or C++ take part in Kavach: it records what the service's handler did into the
flight recorder ([SPEC §3.6, §10](../../SPEC.md)) and it can act as a **host**
for replay and fix verification ([SPEC §9](../../SPEC.md)). It depends on libc
and pthreads only. POSIX only (macOS and Linux).

`include/kavach/kavach.hpp` is a header-only C++17 wrapper over the C API: RAII
recorder, `std::function` / virtual-class handlers, `std::vector<uint8_t>` and
`std::string_view` buffers, exceptions. It is not a second implementation.

## Build and test

```bash
cmake -S sdk/c -B build && cmake --build build && ctest --test-dir build --output-on-failure
```

Compiled with `-Wall -Wextra -Wpedantic -Werror` (turn off with
`-DKAVACH_WERROR=OFF`). `-DKAVACH_SANITIZE=ON` adds ASan and UBSan.

**Compiler on macOS:** use clang (`-DCMAKE_C_COMPILER=clang
-DCMAKE_CXX_COMPILER=clang++`). With gcc-15, exceptions such as
`std::bad_optional_access` are not caught by `catch (const std::exception&)`
across libstdc++ there, so the C++ wrapper reports them as `"unknown
exception"`.

The conformance tests read the contract from the repository's `spec/`; they never copy
it. Point them elsewhere with `-DKAVACH_SPEC_DIR=/path/to/spec` or the
`KAVACH_SPEC_DIR` environment variable (default
the repository's `spec/`). They need `python3`.

| Test | What it runs |
| --- | --- |
| `host-c`, `host-cpp` | `spec/host/run.py` against `kavach-conformance-host-c` / `-cpp` (every transcript) |
| `recorder-{c,cpp}-{pipe,ring}-<case>` | each `spec/recorder/sdk/*.json` through `kavach-recorder-runner-c` / `-cpp`, with `fake_recorder.py`, over the pipe and over the ring |
| `ring` | the real `kavach-recorder` (built with `go`): a process SIGKILLed after its input is published leaves a `crash` fixture, frames larger than the ring, a recorder that stops reading |
| `api`, `cpp-wrapper` | failure mapping, delivery, recorder failure handling, SIGPIPE, without a recorder |
| `ledger-*` | the example below |

Using it from CMake: `target_link_libraries(app kavach_static)` (C) or
`kavach_cpp` (C++, header only, links the static library).

## C API (`kavach/kavach.h`)

```c
#include <kavach/kavach.h>

static int handle(void* state, kavach_env* env, const kavach_input* in) {
  int64_t now = kavach_now_ns(env);                       /* recorded clock read */
  uint8_t id[8];
  kavach_random(env, id, sizeof id);                      /* recorded random read */
  uint8_t* rate; size_t n; char* err;
  if (kavach_query(env, "fx-rates", "EURUSD", 6, &rate, &n, &err) != KAVACH_OK) {
    int rc = kavach_errorf(env, "fx failed: %s", err);    /* `error` marker */
    kavach_free(err);
    return rc;
  }
  kavach_emit(env, "postgres:balances", rate, n, KAVACH_REMOTE);  /* delivered after success */
  kavach_free(rate);
  if (in->len == 0) kavach_panic(env, "empty event");     /* `panic` marker, does not return */
  return KAVACH_OK;
}

int main(int argc, char** argv) {
  /* Host mode: `<binary> ... kavach-host` serves a replay driver and exits. */
  kavach_maybe_host(argc, argv, make_handler, &host_opts);

  kavach_handler h = {.state = &my_state, .handle = handle /* , .snapshot, .restore, .invariants */};
  kavach_recorder_options o = {.service = "ledger", .dir = "fixtures", .gateways = gws, .n_gateways = 1};
  kavach_recorder* rec; char* err;
  if (kavach_recorder_new(&h, &o, &rec, &err) != KAVACH_OK) { /* only with .required = 1 */ }
  kavach_input in = {"kafka:wallet-events", "3:1042", data, len};
  kavach_result res;
  kavach_recorder_step(rec, &in, &res);                   /* res.outcome, res.message */
  kavach_result_clear(&res);
  kavach_recorder_free(rec);                              /* close + wait for `closed` */
}
```

* **Handler**: `kavach_handler` = `state` + `handle` (+ optional `snapshot`,
  `restore`, `invariants`, `destroy`, `flags`). `kavach_maybe_host` takes a
  factory that fills a fresh `kavach_handler` on every `hello`.
* **Env** (valid only during the step): `kavach_now_ns`, `kavach_random`,
  `kavach_query`, `kavach_config`, `kavach_emit`. Handlers must use these
  instead of `clock_gettime`, `getrandom` or direct I/O.
* **Gateways** are registered on the recorder (and the host, for the scope it
  reports on gateway requests) as `{name, callback, user, scope}`. The name `"*"` matches any
  gateway without its own entry. **Config** comes from `config`
  (`kavach_config_fn`) with `config_source` recorded as the read's source.
  **Flags** (`flag.*` facts, sent right after `open`) come from `flags`.
* **Recorder**: `kavach_recorder_new`, `_step`, `_flush(rec, durable)` (blocks
  until the recorder says `durable`, `flush_timeout_ms`), `_close` (waits for
  `closed`, `close_timeout_ms`), `_free`, `_active`. The recorder is started
  with `recorder_argv`, else `$KAVACH_RECORDER`, else `kavach-recorder` on `PATH`,
  with the process's environment unchanged. Do not call flush or close from
  inside a handler; they wait for the running step.
* **`host.runtime`** is `<language>-<compiler>-<version>` from the compiler
  macros: `c11-clang-21.1.2`, `c11-gcc-15.2.0`, `cxx17-clang-21.1.2`
  (`KAVACH_RUNTIME`, evaluated where it is used, so the C++ wrapper reports
  `cxx17-...`). Override with `runtime`.
* `kavach_recorder_options` has test hooks `clock_ns` and `random_bytes`.

### Memory ownership

* Everything you pass to kavach (strings, buffers, option structs, the input) is
  **borrowed for the duration of the call**. The exceptions: `kavach_handler.state`
  and `.invariants` must outlive the recorder/handler; the pointers inside
  `kavach_host_options` (gateways) must stay valid while hosting; option
  strings (`service`, `dir`, ...) are only read inside `kavach_recorder_new`.
  Gateway entries and `config_source` are copied.
* Every buffer kavach **returns** through an out parameter (`kavach_query`'s
  response and error, `kavach_config`'s value, `kavach_result.message/detail`)
  is `malloc`ed and **owned by the caller**: release it with `kavach_free`
  (`free`). `kavach_result_clear` frees a result.
* Callbacks that return buffers (gateway response and error, config value,
  snapshot, invariant detail) must return `malloc`ed memory; **ownership passes
  to kavach**, which frees it, or hands it on to the handler (a gateway
  response reaches `kavach_query`'s caller without a copy).
* `kavach_deliver_fn` receives an array valid only during the call.
* If a step is unwound by `kavach_panic` or an abort, buffers the handler owned
  at that moment are the handler's to clean up (keep them in `state`, as the
  conformance handler does); kavach never frees handler memory.

### Failure model (SPEC §4.5, §9.4, §10.5)

| How the step ends | Marker (recorder) / `done.outcome` (host) |
| --- | --- |
| handler returns `KAVACH_OK` | none / `ok` (then invariants: first failing one is an `invariant` marker / `invariant` outcome naming it) |
| `return kavach_error(env, msg)` or returns `KAVACH_ERROR` | `error`, message exactly `msg` (`"handler returned an error"` if none was set) |
| `kavach_panic(env, msg)` | `panic`, message exactly `msg`. It `longjmp`s to the step boundary, so it never returns |
| the process crashes (SIGSEGV, abort, ...) | the process dies; the `input` frame was written **before** the handler ran, so the recorder writes a `crash` marker and a fixture |
| host mode, the driver `abort`s a request | the env call unwinds the handler like `kavach_panic`; `done` outcome `aborted` |

Outputs of a failed step are discarded, never delivered. A recorder that fails
(cannot start, dies, reports a fatal error, pipe write fails) is logged once,
recording stops, and **no step ever fails because of it**; `required = 1` makes
`kavach_recorder_new` fail instead if it cannot start. Writes to the pipe cannot
raise `SIGPIPE`: `F_SETNOSIGPIPE` where it exists, otherwise `SIGPIPE` is
blocked in the writing thread around the write and a pending one is consumed.
On Linux the pipe is enlarged with `F_SETPIPE_SZ` (1 MiB).

### Transport (SPEC §10.7)

The record stream goes over a shared-memory ring by default: a step costs a few
memory copies and no system call. The SDK creates the ring file (`memfd_create`
on Linux, else in `/dev/shm`, `$TMPDIR` or `/tmp`, unlinked at once), maps its
data area twice back to back so a wrapped frame is one copy, and passes it to
the recorder as file descriptor 3. `no_ring = 1` (C++: `no_ring = true`) forces the
pipe; `ring_bytes` sets the capacity (a power of two of at least 64 KiB, default
8 MiB). If the ring cannot be set up, the SDK logs that and records over the pipe.
The ring needs a little-endian host.

`tests/bench_step.c` is the step-overhead benchmark (same handler shape as Go's
`BenchmarkRecorderStep`): `bench_step <kavach-recorder> pipe|ring [steps] [runs]`.

## C++ API (`kavach/kavach.hpp`, namespace `kavach`)

```cpp
#include <kavach/kavach.hpp>

class Ledger : public kavach::Handler {
  void handle(kavach::Env& env, const kavach::Input& in) override {
    auto now = env.now_ns();
    kavach::Bytes id = env.random(8);
    auto q = env.query("fx-rates", "EURUSD");        // QueryResult{ok, response, error}
    if (!q.ok) throw kavach::HandlerError("fx failed: " + q.error);   // `error` marker
    env.emit("postgres:balances", q.response);       // Scope::Remote by default
    if (in.data.empty()) throw kavach::Panic("empty event");           // `panic` marker
  }
  std::vector<kavach::Invariant> invariants() override { /* ... */ }
};

int main(int argc, char** argv) {
  kavach::maybe_host(argc, argv, [] { return std::make_unique<Ledger>(); });
  Ledger ledger;
  kavach::RecorderOptions o; o.service = "ledger"; o.dir = "fixtures";
  kavach::Recorder rec(ledger, o);                   // throws kavach::Error if o.required and it can't start
  auto r = rec.step("kafka:wallet-events", "3:1042", bytes);   // StepResult{outcome, message, detail}
}                                                    // ~Recorder closes and waits for the recorder
```

* An exception escaping the handler is a `panic` with message `what()`
  (a non-`std::exception` is `"unknown exception"`); `kavach::Panic(msg)` is a
  `panic` with exactly `msg`; `kavach::HandlerError(msg)` is an `error` with
  exactly `msg`. The wrapper catches at its own boundary and reports through
  `kavach_fail_panic` / `kavach_error`, which return normally: **no `longjmp`
  ever crosses C++ frames**. Handlers registered by the wrapper carry
  `KAVACH_HANDLER_NOJUMP`, so even a driver `abort` is delivered as the
  exception `kavach::detail::Aborted` (not a `std::exception`; do not swallow
  it with `catch (...)`, rethrow).
* `ByteView` converts from `std::string_view`, `std::string`, `const char*`,
  `std::vector<uint8_t>` and `(ptr, len)`. Views in `Input` are valid for the
  step only.
* A failing query is a `QueryResult` with `ok == false`, not an exception; a
  gateway callback fails a query by throwing (`kavach::QueryError`).
* Callbacks (`deliver`, `log`, gateway, config) are `std::function`s; their
  exceptions are caught at the C boundary and turned into the C failure result.

## Example: the ledger demo (`examples/ledger`)

A C++ port of the Go ledger demo with the `"amount": null` bug:

```bash
build/examples/ledger/ledger-buggy --in sdk/c/examples/ledger/events.jsonl --fixtures fixtures  # panics at evt-008
build/examples/ledger/ledger-fixed --in sdk/c/examples/ledger/events.jsonl --fixtures fixtures  # rejects it
```

The fix is chosen when building (two executables from one source), not by an
environment variable: environment variables are served from the journal on
replay. Both binaries also act as replay hosts (`kavach::maybe_host`).

## Conformance

`conformance/` holds the SPEC §9.6 conformance handler written once per API
(`conformance.c`, `conformance.hpp`), the host executables
`kavach-conformance-host-c` and `kavach-conformance-host-cpp`, and the
recorder-case runners `kavach-recorder-runner-c` and `-cpp`
(`runner <case.json> <fake_recorder.py> <result.json>`).

```bash
python3 $KAVACH_SPEC_DIR/host/run.py --host build/conformance/kavach-conformance-host-c      # 18/18
python3 $KAVACH_SPEC_DIR/host/run.py --host build/conformance/kavach-conformance-host-cpp    # 18/18
```

## Notes

* **Sandbox mode** (SPEC §6.3) is not supported. A host answers a `hello` with
  `mode: "sandbox"`, or a gateway answer carrying `live`, with `fatal`
  (`"sandbox mode not supported"`).
* **JSON**: `src/json.c` is a small strict RFC 8259 parser written for this
  library (no third-party code). It reads host-protocol messages and the
  recorder's control stream; the conformance programs reuse it. Emitting JSON
  and base64 is a few functions in `src/util.c`.
* **Threads**: one pthread reads the recorder's control stream. A step never
  waits on it; `snapshot_request` is answered at the next step boundary on the
  step's own thread. Steps, flush and close are serialised by a mutex.
* A `durable` message after a `flush` frame releases `kavach_recorder_flush`;
  any `durable` counts (the SDK cannot map record numbers to sequence numbers).
