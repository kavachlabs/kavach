defmodule Kavach.Conformance.RecorderCases do
  @moduledoc """
  Runs one `spec/recorder/sdk/*.json` case (its README) against the SDK, with
  `fake_recorder.py` standing in for `kavach-recorder`. Returns `:ok` or
  `{:error, message}` from the fake's verdict.
  """
  alias Kavach.{Conformance, Input, Recorder}

  def spec_dir, do: System.get_env("KAVACH_SPEC_DIR") || "/Users/koustav/code/kavach-labs/kavach/spec"

  def cases, do: Path.wildcard(Path.join(spec_dir(), "recorder/sdk/*.json")) |> Enum.sort()

  def run(case_path) do
    case_ = case_path |> File.read!() |> Kavach.JSON.decode() |> elem(1)
    fake = Path.join(Path.dirname(case_path), "fake_recorder.py")
    result = Path.join(System.tmp_dir!(), "kavach-recorder-case-#{System.unique_integer([:positive])}.json")
    {:ok, answers} = Agent.start_link(fn -> %{} end)

    open = case_["open"]
    pop = fn kind ->
      Agent.get_and_update(answers, fn a ->
        [h | t] = Map.fetch!(a, kind)
        {h, Map.put(a, kind, t)}
      end)
    end

    opts =
      [
        handler: Conformance.Handler,
        service: open["service"],
        snapshots: open["snapshots"],
        command: ["python3", fake, case_path, result],
        clock: fn -> pop.("clock") |> String.to_integer() end,
        random: fn n ->
          bytes = pop.("rand") |> Base.decode64!()
          ^n = byte_size(bytes)
          bytes
        end,
        gateway: fn _name, _req ->
          case pop.("gateway") do
            %{"response" => r} -> {:ok, Base.decode64!(r)}
            %{"error" => e} -> {:error, e}
          end
        end,
        config: fn _key ->
          case pop.("config") do
            %{"value" => v} -> Base.decode64!(v)
            %{"unset" => true} -> nil
          end
        end,
        flags: Map.new(case_["flags"] || %{})
      ] ++ if(case_["snapshot"], do: [snapshot: case_["snapshot"]], else: [])

    {:ok, rec} = Recorder.start_link(opts)

    for action <- case_["actions"] do
      case action do
        %{"step" => s, "answers" => a} ->
          Agent.update(answers, fn _ -> a end)
          Recorder.step(rec, %Input{source: s["source"], position: s["position"], data: Base.decode64!(s["data"])})

        %{"flush" => %{"durable" => d}} ->
          :ok = Recorder.flush(rec, durable: d)
      end
    end

    :ok = Recorder.close(rec)
    verdict = result |> File.read!() |> Kavach.JSON.decode() |> elem(1)
    File.rm(result)
    if verdict["pass"], do: :ok, else: {:error, verdict["error"]}
  end
end
