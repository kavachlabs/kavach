# Kavach for OCaml

The OCaml SDK: a flight recorder (SPEC §10) and a replay host (SPEC §9) for
single-writer handlers. OCaml 5.1 or later; only the compiler distribution's
libraries (`stdlib`, `unix`, `threads.posix`).

```bash
dune build && dune test
```

`dune test` runs the unit tests, the 18 host transcripts and the 7 SDK recorder
cases (needs `python3`). They read `$KAVACH_SPEC_DIR`, default
the repository's `spec/`.

## A handler

State is explicit: `handle` takes the state and returns the next one.

```ocaml
module Counter : Kavach.HANDLER = struct
  type t = { mutable n : int }
  let init () = { n = 0 }
  let handle env (input : Kavach.Input.t) st =
    let at = Kavach.Env.now_ns env in
    Kavach.Env.emit env ~sink:"log" (Printf.sprintf "%Ld %s" at input.data);
    st.n <- st.n + 1;
    Ok st
  let snapshot = Some (fun st -> string_of_int st.n)
  let restore = Some (fun s -> { n = int_of_string s })
  let invariants = [ ("non_negative", fun st -> if st.n >= 0 then Ok () else Error "negative") ]
end
```

Read time, randomness, gateways and config only through `Env`
(`now_ns`, `random`, `query ~gateway`, `config`) and produce effects only with
`Env.emit ?local ~sink`. `Env.t` is a record of closures, so tests can build one.

## Failure mapping (SPEC §4.5)

| In `handle` | Recorded as |
| --- | --- |
| `Ok state` | success, then invariants are checked: the first failing one is an `invariant` marker named after it |
| `Error msg` | `error`, message exactly `msg`; the previous state is kept |
| `raise (Kavach.Panic msg)` | `panic`, message exactly `msg` |
| any other exception `e` | `panic`, message `Printexc.to_string e`; the backtrace goes to the marker data if recorded (`OCAMLRUNPARAM=b` or `Printexc.record_backtrace true`) |
| `Assert_failure`, `Match_failure`, `Undefined_recursive_module` | `panic`, message is just the exception name; the file and position go to the marker data, so the message does not depend on the checkout |

The same function produces the message when recording and when hosting, so a
recorded failure and its replay compare equal. Since `handle` returns the new
state, a failed step keeps the old value; a handler that mutates in place keeps
its mutations, like a Go handler.

## Recording

```ocaml
let rec_ = Kavach.Recorder.create ~service:"ledger" ~dir:"fixtures" (module Counter) in
(match Kavach.Recorder.step rec_ { source = "kafka:t"; position = "0:1"; data = "x" } with
 | Ok () -> () | Error f -> prerr_endline f.message);
Kavach.Recorder.close rec_
```

- The recorder is `~recorder_command` (an argv array), else `$KAVACH_RECORDER`,
  else `kavach-recorder` on `PATH`. It inherits the process environment.
- If it cannot start or dies, the SDK logs to stderr, stops recording and never
  fails a step. `~required:true` makes `create` raise `Recorder_error` instead.
- The `input` frame is written before the handler runs; the rest of the step is
  written with `step_end`. Outputs go to `~deliver` after a successful step.
- `~gateways`, `~config` (default: environment variables), `~flags`, `~clock` and
  `~random` default to the real world; reads are recorded as they are made.
- `flush ~durable:true` waits for the recorder's `durable`; `close` sends
  `close` and waits for `closed`, and also runs `at_exit`.
- A `Recorder.t` belongs to one thread; only its control-stream thread runs
  beside it.

Limits: the pipe buffer is not enlarged (OCaml's `Unix` has no `F_SETPIPE_SZ`);
timed waits poll every 2 ms; the clock goes through a float, so recorded times
have about 250 ns granularity.

## Hosting

Call `Kavach.maybe_host (module H)` first in `main`. If the last argument is
`kavach-host` it speaks the host protocol and exits; the protocol takes a
duplicate of the real stdout, and fd 1 is pointed at stderr so a handler that
prints cannot corrupt it. Sandbox mode is not supported: `hello` with
`mode: "sandbox"` is answered with `fatal`.

After an `abort` the SDK raises a private exception; if the handler catches it
the SDK still reports `aborted` and sends no further requests.

## Conformance

- `conformance/conformance_host.exe`: the §9.6 handler as a host.
  `python3 spec/host/run.py --host "$PWD/_build/default/conformance/conformance_host.exe"`
- `conformance/recorder_cases.exe [case.json...]`: the SDK recorder cases of
  §10.6, through `fake_recorder.py`.

## Example

`examples/ledger/` ports the Go ledger demo. The build is chosen by a flag,
never an environment variable (those are served from the journal on replay):

```bash
dune build
KAVACH_RECORDER=/path/to/kavach-recorder \
  _build/default/examples/ledger/ledger.exe --in events.jsonl      # panics on "amount": null
_build/default/examples/ledger/ledger.exe --fix --in events.jsonl  # the fix
```

Replay hosts are `ledger.exe` (old) and `ledger.exe --fix` (new).
