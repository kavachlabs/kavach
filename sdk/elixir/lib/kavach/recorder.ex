defmodule Kavach.Recorder do
  @moduledoc """
  The SDK half of the flight recorder (SPEC.md §10).

  A GenServer owns the handler's state and a `Port` to `kavach-recorder`, and
  runs each step inside `step/2`: the `input` frame is written before the
  handler is called, the rest of the step's frames together with `step_end`.
  Control messages from the recorder are handled between steps, so a step
  never waits on them.

  If the recorder cannot be started, or fails later, the SDK logs it with
  `Logger.error/1` and stops recording; steps keep running. `required: true`
  makes `start_link/1` fail instead of continuing unrecorded.

  Options:

    * `:handler` (required), `:service` (required), `:snapshot` (a binary to
      start from instead of `init/0`)
    * `:command` - argv of the recorder; default `KAVACH_RECORDER` or
      `kavach-recorder` on `PATH`
    * `:required` - default `false`
    * `:open` - extra keys for the `open` frame, e.g. `%{"dir" => "kavach"}`
    * `:snapshots` - answer `snapshot_request`; default: the handler
      implements `snapshot/1` and `restore/1`
    * `:flags` - `%{"flag.name" => "value"}` sent as facts
    * `:clock` (`fn -> unix_nanos`), `:random` (`fn n -> binary`),
      `:gateway` (`fn name, request -> {:ok, binary} | {:error, string}`),
      `:local_gateways` (names), `:config` (`fn key -> binary | nil`),
      `:config_source` (default `"env"`)
    * `:deliver` - `fn [%{sink:, data:, scope:}] -> any`, called with a
      step's outputs after it succeeds
  """
  use GenServer
  require Logger
  alias Kavach.{Env, Input, JSON, Step, Wire}

  @durable_timeout 10_000
  @start_wait 2_000

  @doc "Starts the recorder. Returns `{:error, reason}` if `required: true` and recording cannot start."
  def start_link(opts) do
    with {:ok, pid} <- GenServer.start(__MODULE__, opts) do
      Process.link(pid)
      {:ok, pid}
    end
  end

  @doc """
  Runs one step. Returns `:ok` or `{:failed, kind, message}` with `kind` one of
  `:panic`, `:error`, `:invariant`.
  """
  @spec step(GenServer.server(), Input.t()) :: :ok | {:failed, atom, String.t()}
  def step(rec, %Input{} = input), do: GenServer.call(rec, {:step, input}, :infinity)

  @doc "Closes the open block; with `durable: true`, waits until it is durable."
  @spec flush(GenServer.server(), keyword) :: :ok | {:error, term}
  def flush(rec, opts \\ []) do
    GenServer.call(rec, {:flush, Keyword.get(opts, :durable, false)}, Keyword.get(opts, :timeout, @durable_timeout))
  catch
    :exit, {:timeout, _} -> {:error, :timeout}
  end

  @doc "Sends `close`, waits for `closed` and stops the recorder."
  @spec close(GenServer.server(), timeout) :: :ok | {:error, term}
  def close(rec, timeout \\ 10_000) do
    GenServer.call(rec, :close, timeout)
  catch
    :exit, {:timeout, _} -> {:error, :timeout}
  end

  @impl true
  def init(opts) do
    mod = Keyword.fetch!(opts, :handler)

    state =
      case opts[:snapshot] do
        nil -> mod.init()
        snap -> mod.restore(snap)
      end

    st = %{
      opts: opts,
      mod: mod,
      state: state,
      port: nil,
      buf: "",
      durable_waiters: :queue.new(),
      closer: nil,
      closed: false,
      ready: false,
      snapshot_requested: false
    }

    case start_port(opts) do
      {:ok, port} ->
        st = %{st | port: port}
        send_open(st)
        {:ok, await_ready(st, System.monotonic_time(:millisecond) + @start_wait)}

      {:error, reason} ->
        Logger.error("kavach: cannot start the recorder, NOT recording: #{reason}")
        if opts[:required], do: {:stop, reason}, else: {:ok, st}
    end
  end

  @impl true
  def handle_call({:step, input}, _from, st) do
    st = maybe_snapshot(st)
    {reply, st} = run_step(st, input)
    {:reply, reply, st}
  end

  def handle_call({:flush, durable?}, from, st) do
    write(st, Wire.flush(durable?))

    cond do
      st.port == nil -> {:reply, {:error, :not_recording}, st}
      durable? -> {:noreply, %{st | durable_waiters: :queue.in(from, st.durable_waiters)}}
      true -> {:reply, :ok, st}
    end
  end

  def handle_call(:close, from, st) do
    if st.port == nil do
      {:stop, :normal, :ok, st}
    else
      write(st, Wire.close())
      {:noreply, %{st | closer: from}}
    end
  end

  @impl true
  def handle_info({port, {:data, data}}, %{port: port} = st) do
    {lines, rest} = split_lines(st.buf <> data)
    {:noreply, Enum.reduce(lines, %{st | buf: rest}, &control/2)}
  end

  def handle_info({port, {:exit_status, status}}, %{port: port} = st) do
    if st.closed do
      {:stop, :normal, %{st | port: nil}}
    else
      {:noreply, stop_recording(st, "the recorder exited with status #{status}")}
    end
  end

  def handle_info(_, st), do: {:noreply, st}

  @impl true
  def terminate(_, %{port: port}) when is_port(port) do
    Port.close(port)
  rescue
    _ -> :ok
  end

  def terminate(_, _), do: :ok

  defp start_port(opts) do
    argv = opts[:command] || [System.get_env("KAVACH_RECORDER") || "kavach-recorder"]

    with [exe | args] <- argv,
         path when is_binary(path) <- System.find_executable(exe) do
      {:ok, Port.open({:spawn_executable, path}, [:binary, :exit_status, :use_stdio, args: args])}
    else
      _ -> {:error, "#{inspect(argv)} is not an executable"}
    end
  rescue
    e -> {:error, Exception.message(e)}
  end

  # Waiting here, not in a step, keeps a slow-starting recorder from finding a
  # full pipe at its first read (SPEC.md §10.1); on timeout recording goes on.
  defp await_ready(%{ready: true} = st, _), do: st
  defp await_ready(%{port: nil} = st, _), do: st

  defp await_ready(%{port: port} = st, deadline) do
    receive do
      {^port, {:data, data}} ->
        {lines, rest} = split_lines(st.buf <> data)
        await_ready(Enum.reduce(lines, %{st | buf: rest}, &control/2), deadline)

      {^port, {:exit_status, status}} ->
        stop_recording(st, "the recorder exited with status #{status}")
    after
      max(deadline - System.monotonic_time(:millisecond), 0) -> st
    end
  end

  defp send_open(st) do
    o = st.opts
    mod = st.mod

    snapshots =
      Keyword.get(o, :snapshots, function_exported?(mod, :snapshot, 1) and function_exported?(mod, :restore, 1))

    open =
      %{
        "protocol" => 1,
        "service" => Keyword.fetch!(o, :service),
        "start" => if(o[:snapshot], do: "snapshot", else: "genesis"),
        "snapshots" => snapshots,
        "producer" => Kavach.sdk(),
        "handler" => inspect(mod)
      }
      |> Map.merge(o[:open] || %{})

    write(st, Wire.open(open))
    write(st, Wire.facts(Map.put(o[:flags] || %{}, "host.runtime", Kavach.runtime())))
    if o[:snapshot], do: write(st, Wire.snapshot(o[:snapshot]))
  end

  defp maybe_snapshot(%{snapshot_requested: true} = st) do
    write(st, Wire.snapshot(st.mod.snapshot(st.state)))
    %{st | snapshot_requested: false}
  end

  defp maybe_snapshot(st), do: st

  defp run_step(st, input) do
    write(st, Wire.input(input.source, input.position, input.data))
    Process.put(:kavach_frames, [])
    Process.put(:kavach_outputs, [])

    {state, result} = Step.run(st.mod, st.state, env(st.opts), input)

    {result, marker} =
      case result do
        :ok ->
          case Step.check_invariants(st.mod, state) do
            :ok -> {:ok, nil}
            {:invariant, name, detail} -> {{:failed, :invariant, name}, Wire.marker("invariant", name, detail)}
          end

        {kind, msg, detail} ->
          {{:failed, kind, msg}, Wire.marker(Atom.to_string(kind), msg, detail)}
      end

    frames = Enum.reverse([Wire.step_end(), marker | Process.get(:kavach_frames)]) |> Enum.reject(&is_nil/1)
    write(st, frames)

    if result == :ok and st.opts[:deliver], do: st.opts[:deliver].(Enum.reverse(Process.get(:kavach_outputs)))
    {result, %{st | state: state}}
  end

  defp push(frame), do: Process.put(:kavach_frames, [frame | Process.get(:kavach_frames)])

  defp env(o) do
    local = o[:local_gateways] || []

    %Env{
      now_ns: fn ->
        ns = (o[:clock] || fn -> System.os_time(:nanosecond) end).()
        push(Wire.clock(ns))
        ns
      end,
      random: fn n ->
        bytes = (o[:random] || (&:crypto.strong_rand_bytes/1)).(n)
        push(Wire.rand(bytes))
        bytes
      end,
      query: fn name, req ->
        result = o[:gateway].(name, req)
        push(Wire.gateway(name, req, result, if(name in local, do: :local, else: :remote)))
        result
      end,
      config: fn key ->
        value = (o[:config] || fn _ -> nil end).(key)
        push(Wire.config(key, value, o[:config_source] || "env"))
        value
      end,
      emit: fn sink, data, scope ->
        push(Wire.output(sink, data, scope))
        Process.put(:kavach_outputs, [%{sink: sink, data: data, scope: scope} | Process.get(:kavach_outputs)])
        :ok
      end
    }
  end

  defp write(%{port: nil}, _), do: :ok

  defp write(%{port: port}, data) do
    Port.command(port, data)
    :ok
  rescue
    # A closed port: the exit_status message follows and stops recording.
    ArgumentError -> :ok
  end

  defp split_lines(data) do
    parts = String.split(data, "\n")
    {Enum.slice(parts, 0..-2//1), List.last(parts)}
  end

  defp control(line, st) do
    case JSON.decode(line) do
      {:ok, %{"t" => "ready"}} ->
        %{st | ready: true}

      {:ok, %{"t" => "snapshot_request"}} ->
        %{st | snapshot_requested: st.opts[:snapshots] != false and function_exported?(st.mod, :snapshot, 1)}

      {:ok, %{"t" => "durable"}} ->
        case :queue.out(st.durable_waiters) do
          {{:value, from}, q} ->
            GenServer.reply(from, :ok)
            %{st | durable_waiters: q}

          {:empty, _} ->
            st
        end

      {:ok, %{"t" => "fixture"} = m} ->
        Logger.warning("kavach: wrote fixture #{m["file"]} for the failure at input #{m["seq"]}")
        st

      {:ok, %{"t" => "error", "message" => msg, "fatal" => true}} ->
        stop_recording(st, "the recorder failed: #{msg}")

      {:ok, %{"t" => "error", "message" => msg}} ->
        Logger.error("kavach: recorder: #{msg}")
        st

      {:ok, %{"t" => "closed"}} ->
        if st.closer, do: GenServer.reply(st.closer, :ok)
        %{st | closer: nil, closed: true}

      _ ->
        st
    end
  end

  defp stop_recording(st, why) do
    Logger.error("kavach: #{why}; NOT recording from here on")
    for from <- :queue.to_list(st.durable_waiters), do: GenServer.reply(from, {:error, :not_recording})
    if st.closer, do: GenServer.reply(st.closer, {:error, :not_recording})
    %{st | port: nil, durable_waiters: :queue.new(), closer: nil}
  end
end
