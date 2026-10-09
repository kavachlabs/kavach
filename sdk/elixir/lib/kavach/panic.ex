defmodule Kavach.Panic do
  @moduledoc """
  Raise to fail a step as a `panic` whose marker message is exactly `message`.

  `keep` is `{:state, state}` to make the step's state changes survive the
  failure (the Go SDK has no rollback; with immutable state the handler says
  which state to carry on with). Without it the state from before the step is
  kept.
  """
  defexception [:message, keep: nil]
end
