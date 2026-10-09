defmodule Kavach do
  @moduledoc """
  Kavach SDK for Elixir: record what a handler did before it failed, and
  replay it from a fixture (SPEC.md §9, §10).

  A handler is a module implementing `Kavach.Handler`. Record it with
  `Kavach.Recorder`; let `kavach replay` drive it by calling `maybe_host/2`
  first thing in `main`.
  """

  @version "0.1.0"

  def version, do: @version

  @doc "The `ready.sdk` string and the `producer` of recorded journals."
  def sdk, do: "kavach-elixir/" <> @version

  @doc "The language runtime, as recorded in the `host.runtime` fact."
  def runtime, do: "elixir-#{System.version()}-otp-#{System.otp_release()}"

  @doc """
  Acts as a replay host if `argv` ends with `kavach-host`, and never returns
  then. Otherwise returns `:ok`. Call it before the program does anything else.
  """
  @spec maybe_host([String.t()], module) :: :ok
  def maybe_host(argv, handler) do
    if List.last(argv) == "kavach-host", do: Kavach.Host.run(handler), else: :ok
  end
end
