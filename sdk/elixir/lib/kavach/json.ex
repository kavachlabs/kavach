defmodule Kavach.JSON do
  @moduledoc """
  A small JSON codec, so the SDK needs neither Hex nor Elixir 1.18's `JSON`.

  `string/1` escapes exactly as SPEC.md §9.6 requires of the conformance
  handler: `"` `\\` `\\n` `\\r` `\\t`, other characters below U+0020 as
  lowercase `\\u00xx`, everything else as raw UTF-8.
  """

  @spec decode(binary) :: {:ok, term} | {:error, String.t()}
  def decode(bin) when is_binary(bin) do
    {v, rest} = value(skip(bin))
    if skip(rest) == "", do: {:ok, v}, else: {:error, "trailing data"}
  catch
    :bad -> {:error, "invalid JSON"}
  end

  @spec encode(term) :: binary
  def encode(v), do: IO.iodata_to_binary(enc(v))

  @spec string(binary) :: binary
  def string(s), do: IO.iodata_to_binary([?", esc(s), ?"])

  defp enc(m) when is_map(m), do: [?{, Enum.intersperse(for({k, v} <- m, do: [string(to_string(k)), ?:, enc(v)]), ?,), ?}]
  defp enc(l) when is_list(l), do: [?[, Enum.intersperse(Enum.map(l, &enc/1), ?,), ?]]
  defp enc(s) when is_binary(s), do: string(if String.valid?(s), do: s, else: inspect(s))
  defp enc(n) when is_integer(n) or is_float(n), do: to_string(n)
  defp enc(true), do: "true"
  defp enc(false), do: "false"
  defp enc(nil), do: "null"
  defp enc(a) when is_atom(a), do: string(Atom.to_string(a))

  defp esc(<<>>), do: []
  defp esc(<<?", r::binary>>), do: ["\\\"" | esc(r)]
  defp esc(<<?\\, r::binary>>), do: ["\\\\" | esc(r)]
  defp esc(<<?\n, r::binary>>), do: ["\\n" | esc(r)]
  defp esc(<<?\r, r::binary>>), do: ["\\r" | esc(r)]
  defp esc(<<?\t, r::binary>>), do: ["\\t" | esc(r)]

  defp esc(<<c, r::binary>>) when c < 0x20,
    do: ["\\u00", String.downcase(Integer.to_string(c, 16) |> String.pad_leading(2, "0")) | esc(r)]

  defp esc(<<c, r::binary>>), do: [c | esc(r)]

  defp skip(<<c, r::binary>>) when c in [?\s, ?\t, ?\n, ?\r], do: skip(r)
  defp skip(s), do: s

  defp value(<<?{, r::binary>>), do: object(skip(r), %{})
  defp value(<<?[, r::binary>>), do: array(skip(r), [])
  defp value(<<?", r::binary>>), do: str(r, [])
  defp value("true" <> r), do: {true, r}
  defp value("false" <> r), do: {false, r}
  defp value("null" <> r), do: {nil, r}

  defp value(s) do
    case Regex.run(~r/\A-?\d+(\.\d+)?([eE][+-]?\d+)?/, s) do
      [num | rest] ->
        n = if rest == [] or rest == [""], do: String.to_integer(num), else: String.to_float(fix_float(num))
        {n, binary_part(s, byte_size(num), byte_size(s) - byte_size(num))}

      _ ->
        throw(:bad)
    end
  end

  # String.to_float needs a fraction ("1e5" is not accepted).
  defp fix_float(num), do: if(String.contains?(num, "."), do: num, else: String.replace(num, ~r/[eE]/, ".0e", global: false))

  defp object(<<?}, r::binary>>, acc), do: {acc, r}

  defp object(<<?", r::binary>>, acc) do
    {k, r} = str(r, [])
    <<?:, r::binary>> = skip(r)
    {v, r} = value(skip(r))

    case skip(r) do
      <<?,, r::binary>> -> object(skip(r), Map.put(acc, k, v))
      <<?}, r::binary>> -> {Map.put(acc, k, v), r}
      _ -> throw(:bad)
    end
  end

  defp object(_, _), do: throw(:bad)

  defp array(<<?], r::binary>>, []), do: {[], r}

  defp array(s, acc) do
    {v, r} = value(skip(s))

    case skip(r) do
      <<?,, r::binary>> -> array(skip(r), [v | acc])
      <<?], r::binary>> -> {Enum.reverse([v | acc]), r}
      _ -> throw(:bad)
    end
  end

  defp str(<<?", r::binary>>, acc), do: {IO.iodata_to_binary(Enum.reverse(acc)), r}

  defp str(<<?\\, ?u, h::binary-size(4), r::binary>>, acc) do
    hi = hex(h)

    case {hi, r} do
      {hi, <<?\\, ?u, l::binary-size(4), r2::binary>>} when hi in 0xD800..0xDBFF ->
        lo = hex(l)
        if lo not in 0xDC00..0xDFFF, do: throw(:bad)
        str(r2, [<<0x10000 + (hi - 0xD800) * 0x400 + (lo - 0xDC00)::utf8>> | acc])

      {hi, _} when hi in 0xD800..0xDFFF ->
        throw(:bad)

      {hi, _} ->
        str(r, [<<hi::utf8>> | acc])
    end
  end

  defp str(<<?\\, c, r::binary>>, acc) do
    ch =
      case c do
        ?n -> ?\n
        ?t -> ?\t
        ?r -> ?\r
        ?b -> ?\b
        ?f -> ?\f
        c when c in [?", ?\\, ?/] -> c
        _ -> throw(:bad)
      end

    str(r, [ch | acc])
  end

  defp str(<<c::utf8, r::binary>>, acc), do: str(r, [<<c::utf8>> | acc])
  defp str(_, _), do: throw(:bad)

  defp hex(h) do
    case Integer.parse(h, 16) do
      {n, ""} -> n
      _ -> throw(:bad)
    end
  end
end
