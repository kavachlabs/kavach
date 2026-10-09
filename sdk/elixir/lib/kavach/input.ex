defmodule Kavach.Input do
  @moduledoc "One event consumed by the handler (SPEC.md §4.1)."
  @enforce_keys [:source, :data]
  defstruct [:source, :data, position: ""]

  @type t :: %__MODULE__{source: String.t(), position: String.t(), data: binary}
end
