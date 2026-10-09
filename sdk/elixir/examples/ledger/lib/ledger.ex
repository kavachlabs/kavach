defmodule Ledger do
  @moduledoc """
  A single-writer wallet ledger: folds events into balances. Port of the Go
  demo (`examples/ledger`), including its planted bug: an event with
  `"amount": null` crashes the handler unless `fix: true` is set in the
  `:ledger` application environment (the CLI's `--fix` flag does that).
  """
  @behaviour Kavach.Handler
  alias Kavach.{Env, JSON}

  @impl true
  def init, do: %{balances: %{}, net: 0}

  @impl true
  def handle(env, input, ledger) do
    case JSON.decode(input.data) do
      {:ok, %{} = ev} -> event(env, ev, ledger)
      _ -> {:error, "decode event at #{input.position}: invalid JSON"}
    end
  end

  defp event(env, ev, ledger) do
    amount = ev["amount"]

    cond do
      fix?() and amount == nil -> reject(env, ev, "missing amount", ledger)
      amount <= 0 -> reject(env, ev, "amount must be positive", ledger)
      true -> apply_event(env, ev, amount, Env.now(env), ledger)
    end
  end

  defp apply_event(env, %{"type" => "deposit"} = ev, amount, at, l) do
    post(env, ev, ev["account"], amount, at, %{l | net: l.net + amount})
  end

  defp apply_event(env, %{"type" => "withdraw"} = ev, amount, at, l) do
    if balance(l, ev["account"]) < amount do
      reject(env, ev, "insufficient funds", l)
    else
      post(env, ev, ev["account"], -amount, at, %{l | net: l.net - amount})
    end
  end

  defp apply_event(env, %{"type" => "transfer"} = ev, amount, at, l) do
    if balance(l, ev["account"]) < amount do
      reject(env, ev, "insufficient funds", l)
    else
      l = post(env, ev, ev["account"], -amount, at, l) |> elem(1)
      post(env, ev, ev["to"], amount, at, l)
    end
  end

  defp apply_event(env, ev, _amount, _at, l), do: reject(env, ev, "unknown event type #{ev["type"]}", l)

  defp post(env, ev, account, delta, at, l) do
    balances = Map.update(l.balances, account, delta, &(&1 + delta))
    txn = Base.encode16(Env.random(env, 8), case: :lower)

    entry = %{
      "txn" => txn,
      "event" => ev["id"],
      "account" => account,
      "delta" => delta,
      "balance" => balances[account],
      "at" => DateTime.to_iso8601(at)
    }

    Env.emit(env, "ledger.entries", JSON.encode(entry))
    {:ok, %{l | balances: balances}}
  end

  defp reject(env, ev, reason, l) do
    Env.emit(env, "ledger.rejections", JSON.encode(%{"event" => ev["id"], "reason" => reason}))
    {:ok, l}
  end

  defp balance(l, account), do: Map.get(l.balances, account, 0)
  defp fix?, do: Application.get_env(:ledger, :fix, false)

  @impl true
  def invariants(l) do
    [
      {"balances_non_negative",
       fn ->
         case l.balances |> Enum.sort() |> Enum.find(fn {_, b} -> b < 0 end) do
           nil -> :ok
           {a, b} -> {:error, "account #{a} has balance #{b}"}
         end
       end},
      {"money_conserved",
       fn ->
         sum = l.balances |> Map.values() |> Enum.sum()
         if sum == l.net, do: :ok, else: {:error, "balances sum to #{sum}, deposits minus withdrawals is #{l.net}"}
       end}
    ]
  end

  @impl true
  def snapshot(l), do: JSON.encode(%{"balances" => l.balances, "net" => l.net})

  @impl true
  def restore(bin) do
    {:ok, %{"balances" => b, "net" => n}} = JSON.decode(bin)
    %{balances: b, net: n}
  end
end
