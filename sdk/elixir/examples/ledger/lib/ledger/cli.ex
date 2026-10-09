defmodule Ledger.CLI do
  @moduledoc """
      ledger --in events.jsonl [--fixtures DIR] [--fix]

  Also a replay host: `kavach replay FIXTURE --bin "./ledger"` (add `--fix`
  to the command to replay with the fixed handler).
  """

  def main(argv) do
    {opts, _, _} = OptionParser.parse(argv, strict: [in: :string, fixtures: :string, fix: :boolean])
    Application.put_env(:ledger, :fix, Keyword.get(opts, :fix, false))
    Kavach.maybe_host(argv, Ledger)

    path = opts[:in] || abort("ledger: pass --in FILE")

    {:ok, rec} =
      Kavach.Recorder.start_link(
        handler: Ledger,
        service: "ledger",
        open: %{"dir" => opts[:fixtures] || "fixtures"},
        deliver: fn outs -> Enum.each(outs, &IO.puts("#{String.pad_trailing(&1.sink, 18)} #{&1.data}")) end
      )

    status = fold(rec, path)
    Kavach.Recorder.close(rec)
    System.halt(status)
  end

  defp fold(rec, path) do
    path
    |> File.stream!()
    |> Stream.with_index(1)
    |> Enum.reduce_while(0, fn {line, n}, _ ->
      case String.trim(line) do
        "" ->
          {:cont, 0}

        data ->
          input = %Kavach.Input{source: "file:" <> Path.basename(path), position: Integer.to_string(n), data: data}

          case Kavach.Recorder.step(rec, input) do
            {:failed, :panic, msg} ->
              IO.puts(:stderr, "ledger: panic at #{input.position}: #{msg}")
              {:halt, 1}

            {:failed, kind, msg} ->
              IO.puts(:stderr, "ledger: #{kind} at #{input.position}: #{msg}")
              {:cont, 0}

            :ok ->
              {:cont, 0}
          end
      end
    end)
  end

  defp abort(msg) do
    IO.puts(:stderr, msg)
    System.halt(2)
  end
end
