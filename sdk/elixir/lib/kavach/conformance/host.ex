defmodule Kavach.Conformance.Host do
  @moduledoc "Escript entry point of the conformance host: `kavach_conformance kavach-host`."

  def main(argv) do
    Kavach.maybe_host(argv, Kavach.Conformance.Handler)
    IO.puts(:stderr, "usage: kavach_conformance kavach-host")
    System.halt(2)
  end
end
