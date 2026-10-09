defmodule Kavach.Env do
  @moduledoc """
  The handler's only way to read the world and to produce effects. Live, the
  reads are recorded; in replay they are served from the journal.
  """

  @enforce_keys [:now_ns, :random, :query, :config, :emit]
  defstruct [:now_ns, :random, :query, :config, :emit]

  @type t :: %__MODULE__{}

  @doc "Nanoseconds since the Unix epoch."
  @spec now_ns(t) :: integer
  def now_ns(%__MODULE__{now_ns: f}), do: f.()

  @spec now(t) :: DateTime.t()
  def now(env), do: DateTime.from_unix!(now_ns(env), :nanosecond)

  @doc "`n` (at least 1) random bytes."
  @spec random(t, pos_integer) :: binary
  def random(%__MODULE__{random: f}, n) when n >= 1, do: f.(n)

  @doc "Queries the gateway `gateway`; the response is recorded as the connection returned it."
  @spec query(t, String.t(), binary) :: {:ok, binary} | {:error, String.t()}
  def query(%__MODULE__{query: f}, gateway, request), do: f.(gateway, request)

  @doc "A config value that can change what the handler does, or `nil` if unset."
  @spec config(t, String.t()) :: binary | nil
  def config(%__MODULE__{config: f}, key), do: f.(key)

  @doc "Requests an effect. Recorded always, delivered only if the step succeeds."
  @spec emit(t, String.t(), binary, :remote | :local) :: :ok
  def emit(%__MODULE__{emit: f}, sink, data, scope \\ :remote), do: f.(sink, data, scope)
end
