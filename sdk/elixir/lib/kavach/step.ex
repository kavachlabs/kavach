defmodule Kavach.Step do
  @moduledoc false
  # Runs one handler step and maps how it ended; shared by the recorder and the host.
  alias Kavach.Failure

  @type result ::
          :ok
          | :aborted
          | {:panic | :error, String.t(), String.t()}

  @doc "Throw this from an `Env` read to unwind a step that must stop (SPEC.md §9.4)."
  def abort, do: throw({__MODULE__, :abort})

  @spec run(module, term, Kavach.Env.t(), Kavach.Input.t()) :: {term, result}
  def run(mod, state, env, input) do
    case mod.handle(env, input, state) do
      {:ok, s} -> {s, :ok}
      {:error, msg} when is_binary(msg) -> {state, {:error, msg, ""}}
      {:error, msg, s} when is_binary(msg) -> {s, {:error, msg, ""}}
      other -> {state, {:panic, Failure.normalize("invalid handler result: " <> inspect(other)), ""}}
    end
  rescue
    e ->
      keep =
        case e do
          %Kavach.Panic{keep: {:state, s}} -> s
          _ -> state
        end

      {keep, {:panic, Failure.exception(e), Failure.stacktrace(__STACKTRACE__)}}
  catch
    :throw, {__MODULE__, :abort} -> {state, :aborted}
    :throw, v -> {state, {:panic, Failure.throwed(v), Failure.stacktrace(__STACKTRACE__)}}
    :exit, r -> {state, {:panic, Failure.exited(r), Failure.stacktrace(__STACKTRACE__)}}
  end

  @spec invariant_names(module, term) :: [String.t()]
  def invariant_names(mod, state), do: Enum.map(invariants(mod, state), &elem(&1, 0))

  @doc "The first failing invariant, in declared order."
  @spec check_invariants(module, term) :: :ok | {:invariant, String.t(), String.t()}
  def check_invariants(mod, state) do
    Enum.find_value(invariants(mod, state), :ok, fn {name, check} ->
      case safe_check(check) do
        :ok -> nil
        {:error, detail} -> {:invariant, name, detail}
      end
    end)
  end

  defp invariants(mod, state) do
    if function_exported?(mod, :invariants, 1), do: mod.invariants(state), else: []
  end

  defp safe_check(check) do
    check.()
  rescue
    e -> {:error, Failure.exception(e)}
  end
end
