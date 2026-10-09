defmodule Kavach.Wire do
  @moduledoc false
  # The record stream's binary encoding (SPEC.md §2, §10.2).
  import Bitwise

  @open 0x01
  @record 0x02
  @step_end 0x03
  @facts 0x04
  @snapshot 0x05
  @flush 0x06
  @close 0x07

  def uvarint(n) when n < 0x80, do: <<n>>
  def uvarint(n), do: <<(n &&& 0x7F) ||| 0x80, uvarint(n >>> 7)::binary>>

  def bytes(b), do: <<uvarint(byte_size(b))::binary, b::binary>>

  defp frame(kind, payload) do
    <<uvarint(1 + byte_size(payload))::binary, kind, payload::binary>>
  end

  def open(map), do: frame(@open, Kavach.JSON.encode(map))
  def step_end, do: frame(@step_end, "")
  def snapshot(data), do: frame(@snapshot, bytes(data))
  def flush(durable?), do: frame(@flush, <<if(durable?, do: 1, else: 0)>>)
  def close, do: frame(@close, "")

  @doc "A `facts` frame of `key => value` facts, all with form 0."
  def facts(map) do
    body = for {k, v} <- Enum.sort(map), into: "", do: <<bytes(k)::binary, 0, bytes(v)::binary>>
    frame(@facts, <<uvarint(map_size(map))::binary, body::binary>>)
  end

  defp scope(:local), do: 1
  defp scope(:remote), do: 0

  defp record(type, critical?, payload) do
    frame(@record, <<type, if(critical?, do: 1, else: 0), payload::binary>>)
  end

  def input(source, position, data),
    do: record(0x01, false, bytes(source) <> bytes(position) <> bytes(data))

  def clock(ns), do: record(0x02, false, <<ns::signed-little-64>>)
  def rand(data), do: record(0x03, false, bytes(data))
  def output(sink, data, sc), do: record(0x04, false, bytes(sink) <> bytes(data) <> <<scope(sc)>>)
  def marker(kind, message, data), do: record(0x05, false, bytes(kind) <> bytes(message) <> bytes(data))

  def gateway(name, request, result, sc) do
    {response, error} =
      case result do
        {:ok, r} -> {r, ""}
        {:error, e} -> {"", e}
      end

    record(0x07, true, bytes(name) <> bytes(request) <> bytes(response) <> bytes(error) <> <<scope(sc)>>)
  end

  def config(key, value, source) do
    {present, v} = if value == nil, do: {0, ""}, else: {1, value}
    record(0x09, true, bytes(key) <> <<present>> <> bytes(v) <> bytes(source))
  end
end
