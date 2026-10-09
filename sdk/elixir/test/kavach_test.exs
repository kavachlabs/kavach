defmodule Kavach.JSONTest do
  use ExUnit.Case, async: true
  alias Kavach.JSON

  test "decodes" do
    assert {:ok, %{"a" => [1, -2.5, 1.0e3, true, nil, "x\né😀"]}} =
             JSON.decode(~s( {"a" : [1, -2.5, 1e3, true, null, "x\\n\\u00e9\\ud83d\\ude00"]} ))

    assert {:error, _} = JSON.decode("{")
    assert {:error, _} = JSON.decode("[1] x")
  end

  test "string escapes exactly what the spec says" do
    assert JSON.string("a\"b\\c\n\r\t\u0001\u001f<>&é\u007f") ==
             ~s("a\\"b\\\\c\\n\\r\\t\\u0001\\u001f<>&é\u007f")
  end

  test "encode round-trips" do
    m = %{"t" => "x", "n" => [1, 2], "ok" => true}
    assert JSON.decode(JSON.encode(m)) == {:ok, m}
  end
end

defmodule Kavach.WireTest do
  use ExUnit.Case, async: true
  alias Kavach.Wire

  test "uvarint is LEB128" do
    assert Wire.uvarint(0) == <<0>>
    assert Wire.uvarint(300) == <<0xAC, 0x02>>
  end

  test "step_end frame" do
    assert Wire.step_end() == <<1, 3>>
  end
end

defmodule Kavach.FailureTest do
  use ExUnit.Case, async: true
  alias Kavach.Failure

  test "messages carry no PIDs, references, addresses or locations" do
    assert Failure.normalize("bad #PID<0.123.0> and #Reference<0.1.2.3> and #Port<0.5> via <0.9.0>") ==
             "bad #PID<> and #Reference<> and #Port<> via <>"

    assert Failure.normalize("#Function<0.12345/1 in Foo.bar/0> at 0x00007f8a1c") == "#Function<> at 0x"
    assert Failure.normalize("boom (lib/ledger.ex:42) twice (ledger.exs:3:7)") == "boom twice"
  end

  test "exceptions map to Module: message" do
    assert Failure.exception(%ArgumentError{message: "no"}) == "ArgumentError: no"
    assert Failure.exception(%RuntimeError{message: "pid #{inspect(self())} failed"}) == "RuntimeError: pid #PID<> failed"
    assert Failure.exception(%Kavach.Panic{message: "exact #PID<0.1.0>"}) == "exact #PID<0.1.0>"
    assert Failure.throwed({:x, self()}) == "throw: {:x, #PID<>}"
    assert Failure.exited({:badarg, make_ref()}) == "exit: {:badarg, #Reference<>}"
  end

  test "a failing handler gives the same message on every run" do
    defmodule Crash do
      @behaviour Kavach.Handler
      def init, do: nil
      def handle(_env, _in, _s), do: raise(RuntimeError, "died in #{inspect(self())} at #{__ENV__.file}:#{__ENV__.line}")
    end

    env = %Kavach.Env{now_ns: nil, random: nil, query: nil, config: nil, emit: nil}
    input = %Kavach.Input{source: "t", data: ""}
    {_, {:panic, m1, detail}} = Kavach.Step.run(Crash, nil, env, input)
    {_, {:panic, m2, _}} = Task.async(fn -> Kavach.Step.run(Crash, nil, env, input) end) |> Task.await()
    assert m1 == m2
    assert m1 == "RuntimeError: died in #PID<> at"
    assert detail =~ "kavach_test.exs"
  end
end

defmodule Kavach.RecorderCasesTest do
  use ExUnit.Case, async: true
  alias Kavach.Conformance.RecorderCases

  for path <- RecorderCases.cases() do
    @path path
    test "recorder case #{Path.basename(path)}" do
      assert RecorderCases.run(@path) == :ok
    end
  end

  test "required: true fails to start without a recorder" do
    assert {:error, _} =
             Kavach.Recorder.start_link(
               handler: Kavach.Conformance.Handler,
               service: "x",
               command: ["/nonexistent/kavach-recorder"],
               required: true
             )
  end

  test "an unavailable recorder never fails the step" do
    {:ok, rec} =
      Kavach.Recorder.start_link(handler: Kavach.Conformance.Handler, service: "x", command: ["/nonexistent"])

    assert :ok = Kavach.Recorder.step(rec, %Kavach.Input{source: "t", data: "[]"})
  end
end
