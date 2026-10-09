defmodule Kavach.Conformance.Handler do
  @moduledoc """
  The conformance handler of SPEC.md §9.6. State is the count.

  `count` takes effect immediately even if a later operation fails the step,
  so failures carry the count forward (`{:error, msg, count}`,
  `Kavach.Panic` with `keep`).
  """
  @behaviour Kavach.Handler
  alias Kavach.{Env, JSON, Panic}

  @limit 1000

  @impl true
  def init, do: 0

  @impl true
  def snapshot(count), do: Integer.to_string(count)

  @impl true
  def restore(bin), do: String.to_integer(bin)

  @impl true
  def invariants(count) do
    [
      {"below_limit",
       fn -> if count >= @limit, do: {:error, "count #{count} reached #{@limit}"}, else: :ok end}
    ]
  end

  @impl true
  def handle(env, input, count) do
    {:ok, ops} = JSON.decode(input.data)

    case run(ops, env, count) do
      {:ok, count} -> {:ok, count + 1}
      {:error, msg, count} -> {:error, msg, count}
      {:panic, msg, count} -> raise Panic, message: msg, keep: {:state, count}
    end
  end

  defp run([], _env, count), do: {:ok, count}

  defp run([op | rest], env, count) do
    case apply_op(op, env, count) do
      {:ok, count} -> run(rest, env, count)
      failed -> failed
    end
  end

  defp apply_op(%{"op" => "clock"}, env, count) do
    trace(env, ["{\"clock\":", JSON.string(Integer.to_string(Env.now_ns(env))), "}"])
    {:ok, count}
  end

  defp apply_op(%{"op" => "rand", "n" => n}, env, count) do
    trace(env, Env.random(env, n))
    {:ok, count}
  end

  defp apply_op(%{"op" => "gateway", "gateway" => g, "request" => r}, env, count) do
    case Env.query(env, g, r) do
      {:ok, resp} -> trace(env, resp)
      {:error, e} -> trace(env, ["{\"error\":", JSON.string(e), "}"])
    end

    {:ok, count}
  end

  defp apply_op(%{"op" => "config", "key" => k}, env, count) do
    trace(env, Env.config(env, k) || ~s({"unset":true}))
    {:ok, count}
  end

  defp apply_op(%{"op" => "getenv", "name" => n}, env, count) do
    trace(env, System.get_env(n) || ~s({"unset":true}))
    {:ok, count}
  end

  defp apply_op(%{"op" => "emit", "sink" => s, "data" => d}, env, count) do
    Env.emit(env, s, d)
    {:ok, count}
  end

  defp apply_op(%{"op" => "panic", "message" => m}, _env, count), do: {:panic, m, count}
  defp apply_op(%{"op" => "error", "message" => m}, _env, count), do: {:error, m, count}

  defp apply_op(%{"op" => "print", "text" => t}, _env, count) do
    IO.puts(t)
    {:ok, count}
  end

  defp apply_op(%{"op" => "count", "n" => n}, _env, count), do: {:ok, count + n}

  defp trace(env, data), do: Env.emit(env, "trace", IO.iodata_to_binary(data))
end
