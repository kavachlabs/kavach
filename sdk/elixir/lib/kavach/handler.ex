defmodule Kavach.Handler do
  @moduledoc """
  A single-writer handler with explicit state, in the shape of a GenServer.

  `handle/3` is a pure function of its state, its input and the world it reads
  through `env` (`Kavach.Env`). Failure mapping (SPEC.md §4.5):

    * `{:error, message}` is an `error` marker with exactly `message`; the
      state from before the step is kept. `{:error, message, state}` is the
      same but carries on with `state`.
    * `raise Kavach.Panic, message: m` is a `panic` with exactly `m`.
    * any other exception is a `panic` with `"Module: message"`, where PIDs,
      references, ports, funs, addresses and source locations are replaced
      (`Kavach.Failure`); `throw v` is `"throw: " <> inspect(v)` and `exit r`
      is `"exit: " <> inspect(r)`. The stacktrace is the marker's data.
  """

  @type state :: term
  @type invariant :: {String.t(), (-> :ok | {:error, String.t()})}

  @callback init() :: state
  @callback handle(Kavach.Env.t(), Kavach.Input.t(), state) ::
              {:ok, state} | {:error, String.t()} | {:error, String.t(), state}
  @callback snapshot(state) :: binary
  @callback restore(binary) :: state
  @callback invariants(state) :: [invariant]

  @optional_callbacks snapshot: 1, restore: 1, invariants: 1
end
