defmodule Kavach.Failure do
  @moduledoc """
  The SDK's mapping of Elixir failures to marker messages (SPEC.md §4.5).

  A message must not depend on where or how the build runs, so PIDs,
  references, ports, funs, addresses and `file.ex:12` locations that the
  runtime puts into messages are replaced or removed; the stacktrace carries
  them in the marker's data.
  """

  @rules [
    {~r/#(PID|Reference|Port|Function)<[^>]*>/, "#\\1<>"},
    {~r/<\d+\.\d+\.\d+>/, "<>"},
    {~r/0x[0-9a-fA-F]{6,}/, "0x"},
    {~r/\s*\(?\/?(?:[\w.\-]+\/)*[\w.\-]+\.(?:exs?|erl|hrl):\d+(?::\d+)?\)?/, ""}
  ]

  @spec normalize(String.t()) :: String.t()
  def normalize(message) do
    Enum.reduce(@rules, message, fn {re, to}, m -> Regex.replace(re, m, to) end)
  end

  @doc "The marker message of a raised exception."
  @spec exception(Exception.t()) :: String.t()
  def exception(%Kavach.Panic{message: m}), do: m
  def exception(e), do: normalize("#{inspect(e.__struct__)}: #{Exception.message(e)}")

  @spec throwed(term) :: String.t()
  def throwed(v), do: normalize("throw: " <> inspect(v))

  @spec exited(term) :: String.t()
  def exited(r), do: normalize("exit: " <> inspect(r))

  @spec stacktrace(Exception.stacktrace()) :: String.t()
  def stacktrace(st), do: Exception.format_stacktrace(st)
end
