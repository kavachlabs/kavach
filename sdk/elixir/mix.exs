defmodule Kavach.MixProject do
  use Mix.Project

  def project do
    [
      app: :kavach,
      version: "0.1.0",
      elixir: "~> 1.15",
      start_permanent: false,
      deps: [],
      escript: [main_module: Kavach.Conformance.Host, name: "kavach_conformance"]
    ]
  end

  def application do
    [extra_applications: [:logger, :crypto]]
  end
end
