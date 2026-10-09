defmodule Kavach.Host do
  @moduledoc false
  # The host side of the host protocol (SPEC.md §9). Single process: it reads
  # the driver's lines, and the Env closures it hands the handler block on the
  # driver's answers.
  alias Kavach.{Env, Input, JSON, Step}

  @flag :kavach_aborted

  defmodule Stderr do
    @moduledoc false
    # An IO device that writes to standard error, installed as the group
    # leader so a handler's IO.puts cannot corrupt the protocol stream (§9.1).
    def start, do: spawn_link(&loop/0)

    defp loop do
      receive do
        {:io_request, from, ref, req} ->
          send(from, {:io_reply, ref, request(req)})
          loop()
      end
    end

    defp request({:put_chars, enc, chars}), do: write(chars, enc)
    defp request({:put_chars, chars}), do: write(chars, :latin1)
    defp request({:put_chars, enc, m, f, a}), do: write(apply(m, f, a), enc)
    defp request({:put_chars, m, f, a}), do: write(apply(m, f, a), :latin1)
    defp request({:requests, reqs}), do: Enum.reduce(reqs, :ok, fn r, _ -> request(r) end)
    defp request(_), do: {:error, :enotsup}

    defp write(chars, enc) do
      case :unicode.characters_to_binary(chars, enc, :utf8) do
        bin when is_binary(bin) -> IO.binwrite(:standard_error, bin)
        _ -> :ok
      end
    end
  end

  @spec run(module) :: no_return
  def run(mod) do
    stdin = Process.group_leader()
    out = Port.open({:fd, 1, 1}, [:out, :binary])
    Process.group_leader(self(), Stderr.start())
    loop(%{mod: mod, in: stdin, out: out}, :no_session)
  end

  defp loop(ctx, session) do
    case recv(ctx) do
      %{"t" => "hello"} = m -> hello(ctx, m)
      %{"t" => "step"} = m when session != :no_session -> loop(ctx, step(ctx, m, session))
      %{"t" => "end"} -> System.halt(0)
      m -> fatal(ctx, "unexpected message #{inspect(m["t"])}")
    end
  end

  defp hello(ctx, %{"mode" => "sandbox"}), do: fatal(ctx, "sandbox mode not supported")

  defp hello(ctx, m) do
    state =
      try do
        case m do
          %{"start" => "snapshot", "snapshot" => snap} -> ctx.mod.restore(Base.decode64!(snap))
          _ -> ctx.mod.init()
        end
      rescue
        e -> fatal(ctx, "could not create the handler: " <> Exception.message(e))
      end

    send_msg(ctx, %{
      t: "ready",
      protocol: 1,
      sdk: Kavach.sdk(),
      invariants: Step.invariant_names(ctx.mod, state),
      environment: environment()
    })

    loop(ctx, state)
  end

  defp step(ctx, m, state) do
    Process.put(@flag, false)
    input = %Input{source: m["source"], position: m["position"], data: Base.decode64!(m["data"])}
    {state, result} = Step.run(ctx.mod, state, env(ctx), input)

    done =
      case if(Process.get(@flag), do: :aborted, else: result) do
        :aborted -> %{outcome: "aborted"}
        {kind, msg, detail} -> with_detail(%{outcome: Atom.to_string(kind), message: msg}, detail)
        :ok ->
          case Step.check_invariants(ctx.mod, state) do
            :ok -> %{outcome: "ok"}
            {:invariant, name, detail} -> with_detail(%{outcome: "invariant", message: name}, detail)
          end
      end

    send_msg(ctx, Map.put(done, :t, "done"))
    state
  end

  defp with_detail(m, ""), do: m
  defp with_detail(m, detail), do: Map.put(m, :detail, detail)

  defp env(ctx) do
    %Env{
      now_ns: fn -> request(ctx, %{t: "clock"}, "clock")["unix_nanos"] |> String.to_integer() end,
      random: fn n ->
        bytes = Base.decode64!(request(ctx, %{t: "rand", n: n}, "rand")["data"])
        if byte_size(bytes) == n, do: bytes, else: fatal(ctx, "driver sent #{byte_size(bytes)} random bytes, wanted #{n}")
      end,
      query: fn name, req ->
        r = request(ctx, %{t: "gateway", gateway: name, request: Base.encode64(req), scope: "remote"}, "gateway")

        case r do
          %{"error" => e} -> {:error, e}
          %{"response" => resp} -> {:ok, Base.decode64!(resp)}
          _ -> fatal(ctx, "gateway answer has neither response nor error")
        end
      end,
      config: fn key ->
        case request(ctx, %{t: "config", key: key}, "config") do
          %{"present" => true, "value" => v} -> Base.decode64!(v)
          _ -> nil
        end
      end,
      emit: fn sink, data, scope ->
        if Process.get(@flag), do: Step.abort()
        send_msg(ctx, %{t: "emit", sink: sink, data: Base.encode64(data), scope: Atom.to_string(scope)})
      end
    }
  end

  # Sends a request and waits for its answer. After an abort the handler
  # unwinds without asking the driver anything more (§9.4).
  defp request(ctx, msg, expect) do
    if Process.get(@flag), do: Step.abort()
    send_msg(ctx, msg)

    case recv(ctx) do
      %{"t" => ^expect} = r ->
        r

      %{"t" => "abort"} ->
        Process.put(@flag, true)
        Step.abort()

      m ->
        fatal(ctx, "expected #{expect}, got #{inspect(m["t"])}")
    end
  end

  defp send_msg(ctx, map) do
    Port.command(ctx.out, [JSON.encode(map), ?\n])
    :ok
  end

  defp recv(ctx) do
    case IO.binread(ctx.in, :line) do
      line when is_binary(line) ->
        case JSON.decode(line) do
          {:ok, %{"t" => _} = m} -> m
          _ -> fatal(ctx, "malformed message")
        end

      _ ->
        System.halt(0)
    end
  end

  defp fatal(ctx, message) do
    send_msg(ctx, %{t: "fatal", message: message})
    System.halt(1)
  end

  # `kavach-recorder facts` collects the same env. and host. facts the
  # recorder writes into a genesis environment; host.runtime is ours (§9.2).
  defp environment do
    runtime = %{"host.runtime" => %{"value" => Base.encode64(Kavach.runtime())}}

    try do
      with exe when is_binary(exe) <- System.find_executable(System.get_env("KAVACH_RECORDER") || "kavach-recorder"),
           {out, 0} <- System.cmd(exe, ["facts"]),
           {:ok, %{} = facts} <- JSON.decode(out) do
        Map.merge(facts, runtime)
      else
        _ -> runtime
      end
    rescue
      _ -> runtime
    end
  end
end
