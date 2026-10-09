# Kavach for Haskell

The Haskell SDK for Kavach ([SPEC.md](../../SPEC.md)): it records what a
service did (every input, clock and random read, gateway response, config
value and output) through `kavach-recorder`, and acts as a replay host so the
`kavach` CLI can reproduce a production failure and verify a fix.

Only GHC boot packages are used (base, bytestring, text, time, stm, process,
directory, unix, containers). GHC 9.4 or later.

## Determinism is checked by the type checker

A handler runs in the `Kavach` monad. It is a newtype over `EnvImpl -> IO a`
whose constructor is not exported, and it has **no `MonadIO` instance**. The
only effects a handler has are:

| Function | Effect |
| --- | --- |
| `now`, `nowNanos` | read the clock |
| `random n` | read `n` random bytes |
| `query gateway request` | ask an external system; the failure is a value, `Either GatewayError ByteString` |
| `config key` | read a feature flag or limit |
| `emit sink data`, `emitLocal` | request an effect, delivered only after the step succeeds |
| `commit s` | keep state `s` even if the step fails later (see below) |
| `kavachError`, `kavachPanic` | fail the step |

A handler cannot call `getCurrentTime`, `randomIO`, `readFile` or
`System.Environment.getEnv`: they are `IO`, and nothing converts `IO` to
`Kavach`. The SDK exports no escape hatch (`unsafePerformIO` is not
type-correct here, and `Kavach.Internal` is a hidden module). Handler code
cannot `catch` either, because `Kavach` has no `catch`, so the exception the
host uses to abort a nondeterministic step (SPEC §9.4) cannot be swallowed.
What the rules ask of other SDKs by convention, GHC enforces.

State is explicit:

```haskell
data Handler s = Handler
  { handle     :: Input -> s -> Kavach s
  , initial    :: s
  , snapshot   :: Maybe (s -> ByteString)
  , restore    :: Maybe (ByteString -> Either String s)
  , invariants :: [(Text, s -> Either Text ())]
  }
```

`handle` returns the new state. If the step fails, the state is what it was
when the step began, unless the handler called `commit s` earlier in the step.
Use `commit` for handlers whose partial progress is real, which is what a Go
handler that mutates in place has; the conformance handler of SPEC §9.6 needs
it. The new state is forced to weak head normal form inside the step, so use
strict fields: an exception hidden in a lazy field would surface in a later
step, and be attributed to it.

Invariants are checked after every step that ended `ok`, in order; the first
that returns `Left` (or throws) ends the step as `invariant`.

## Failure mapping (SPEC §4.5)

| In the handler | Marker |
| --- | --- |
| `kavachPanic msg` | `panic`, message exactly `msg` |
| `kavachError msg` | `error`, message exactly `msg` |
| any exception, including from pure code (`head []`, `error "..."`, `fromJust Nothing`, a failed pattern match, `div` by zero) | `panic`, message `failureMessage e` |
| an invariant returns `Left why` | `invariant`, message the invariant's name, data `why` |

`failureMessage` is `displayException e` with everything that depends on the
build removed, because a message recorded in production must equal its replay
on another checkout: the `CallStack (from HasCallStack)` and `HasCallStack
backtrace` blocks, any word containing a source location such as `Foo.hs:12:3`
or `Foo.hs:(12,3)-(14,5):`, and hexadecimal addresses. `head []` becomes
`Prelude.head: empty list`; a failed pattern match becomes `Non-exhaustive
patterns in function f`. The full `displayException`, call stack included, is
kept in the marker's `data` (the `detail` of a host `done`). Recording and the
host use the same function.

## Recording

```haskell
import Kavach

main :: IO ()
main = do
  maybeHost myHandler            -- first thing in main: a replay host if started with kavach-host
  let opts = (defaultOptions "ledger") { roDir = Just "fixtures", roDeliver = mapM_ print }
  withRecorder opts myHandler $ \r -> do
    res <- step r (Input "kafka:wallet-events" "0:1042" bytes)
    case stepFailure res of
      Nothing -> pure ()
      Just f  -> putStrLn ("step failed: " ++ show (failKind f, failMessage f))
```

`defaultOptions` uses the real clock, `/dev/urandom`, config from environment
variables (source `"env"`) and no gateways. Register gateways with
`roGateways :: Text -> Maybe Gateway`; a `Gateway` is a scope plus a
connection `ByteString -> IO ByteString` that may throw `GatewayError`.

- The recorder is started with `System.Process`, with the process's
  environment unchanged. Its command is `roRecorderCommand`, else
  `$KAVACH_RECORDER`, else `kavach-recorder` on `PATH`.
- The `input` frame is written before the handler runs; the rest of the step
  is written with `step_end` after it.
- The control stream is read on a `forkIO` thread. A `durable` answer is paired
  with the `flush` that asked for it by counting; `flush r True` waits (ten
  seconds at most). A `snapshot_request` is answered at the next step boundary.
- If the recorder cannot be started, dies, or reports a fatal error, the SDK
  logs to stderr, stops recording, and steps go on unrecorded. With
  `roRequired = True`, `newRecorder` throws `RecorderError` instead.
- `fixture` messages are logged to stderr and passed to `roOnFixture`.
- `host.runtime` is `ghc-<version>`.

Limits: the pipe buffer is not enlarged (`F_SETPIPE_SZ`), because `unix` does
not expose it. `/dev/urandom` means POSIX only. A step that raises an
asynchronous exception is recorded as a panic and the exception is rethrown.

## Replay

`maybeHost handler` returns at once unless the last argument is `kavach-host`.
Then it takes the protocol stream with `hDuplicate stdout`, points `stdout` at
`stderr` with `hDuplicateTo` (so a `putStrLn` in startup code or a
library cannot corrupt the protocol), serves the driver (SPEC §9) and exits. Start it before anything else in
`main`.

`ready.environment` is the output of `kavach-recorder facts` if it can be run,
plus `host.runtime`. Sandbox mode (§6.3) is not supported: a `hello` with
`mode: "sandbox"` is answered with `fatal` `"sandbox mode not supported"`.

Choose the old and new build with a command-line flag, never an environment
variable: environment variables are served from the journal on replay.

## Layout

| Path | |
| --- | --- |
| `src/Kavach.hs` | the public API |
| `src/Kavach/Recorder.hs`, `Host.hs` | the recorder half (§10) and the host (§9) |
| `src/Kavach/Conformance.hs` | the conformance handler of §9.6 |
| `src/Kavach/Json.hs`, `Wire.hs` | JSON and the record stream encoding |
| `conformance/` | `kavach-conformance-host` and `kavach-recorder-case` |
| `examples/ledger/` | the demo: `ledger` (buggy) and `ledger --fix` |
| `test/Spec.hs` | unit tests, the 7 recorder cases, the 18 host transcripts |

## Conformance

```bash
cabal build all && cabal test

# host transcripts
python3 ../../spec/host/run.py --host "$(cabal list-bin kavach-conformance-host)"

# one recorder case, through ../../spec/recorder/sdk/fake_recorder.py
$(cabal list-bin kavach-recorder-case) ../../spec/recorder/sdk/reads.json
```

`KAVACH_SPEC_DIR` overrides the spec directory the runners and tests use.

## The ledger demo

```bash
export KAVACH_RECORDER=/path/to/kavach-recorder
cabal run ledger -- --in ../../examples/ledger/testdata/events.jsonl --fixtures fixtures
kavach inspect fixtures/fixtures/*.kavach     # last records: evt-008, then marker panic
```

`evt-008` has `"amount": null`; the buggy build calls `fromJust` on it and
panics with `Maybe.fromJust: Nothing`. `ledger --fix` rejects it with
`missing amount`.
