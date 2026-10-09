defmodule Ledger.MixProject do
  use Mix.Project

  def project do
    [
      app: :ledger,
      version: "0.1.0",
      elixir: "~> 1.15",
      deps: [{:kavach, path: "../.."}],
      escript: [main_module: Ledger.CLI, name: "ledger"]
    ]
  end

  def application, do: [extra_applications: [:logger]]
end
