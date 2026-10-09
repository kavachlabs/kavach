"""A small JSON reader and writer; objects are `Dict{String,Any}` on read, and
`Dict`s or `NamedTuple`s (which keep their key order) on write."""
module Json

export parse, stringify

struct ParseError <: Exception
    msg::String
end
Base.showerror(io::IO, e::ParseError) = print(io, "JSON: ", e.msg)

function parse(s::AbstractString)
    v, i = _value(String(s), _ws(s, 1))
    i = _ws(s, i)
    i > ncodeunits(s) || throw(ParseError("trailing characters at $i"))
    return v
end

function _ws(s, i)
    while i <= ncodeunits(s) && codeunit(s, i) in (0x20, 0x09, 0x0a, 0x0d)
        i += 1
    end
    return i
end

function _value(s::String, i::Int)
    i <= ncodeunits(s) || throw(ParseError("unexpected end"))
    c = codeunit(s, i)
    if c == UInt8('{')
        d = Dict{String,Any}()
        i = _ws(s, i + 1)
        if i <= ncodeunits(s) && codeunit(s, i) == UInt8('}')
            return d, i + 1
        end
        while true
            i = _ws(s, i)
            k, i = _string(s, i)
            i = _ws(s, i)
            i <= ncodeunits(s) && codeunit(s, i) == UInt8(':') || throw(ParseError("expected ':' at $i"))
            v, i = _value(s, _ws(s, i + 1))
            d[k] = v
            i = _ws(s, i)
            i <= ncodeunits(s) || throw(ParseError("unexpected end"))
            c = codeunit(s, i)
            i += 1
            c == UInt8('}') && return d, i
            c == UInt8(',') || throw(ParseError("expected ',' or '}' at $(i - 1)"))
        end
    elseif c == UInt8('[')
        a = Any[]
        i = _ws(s, i + 1)
        if i <= ncodeunits(s) && codeunit(s, i) == UInt8(']')
            return a, i + 1
        end
        while true
            v, i = _value(s, _ws(s, i))
            push!(a, v)
            i = _ws(s, i)
            i <= ncodeunits(s) || throw(ParseError("unexpected end"))
            c = codeunit(s, i)
            i += 1
            c == UInt8(']') && return a, i
            c == UInt8(',') || throw(ParseError("expected ',' or ']' at $(i - 1)"))
        end
    elseif c == UInt8('"')
        return _string(s, i)
    elseif startswith(SubString(s, i), "true")
        return true, i + 4
    elseif startswith(SubString(s, i), "false")
        return false, i + 5
    elseif startswith(SubString(s, i), "null")
        return nothing, i + 4
    else
        j = i
        while j <= ncodeunits(s) && codeunit(s, j) in b"+-0123456789.eE"
            j += 1
        end
        j > i || throw(ParseError("unexpected character at $i"))
        t = SubString(s, i, j - 1)
        n = tryparse(Int64, t)
        n === nothing && (n = tryparse(Float64, t))
        n === nothing && throw(ParseError("bad number $t"))
        return n, j
    end
end

function _hex4(s, i)
    i + 3 <= ncodeunits(s) || throw(ParseError("bad \\u escape"))
    n = tryparse(UInt16, SubString(s, i, i + 3); base=16)
    n === nothing && throw(ParseError("bad \\u escape"))
    return n
end

function _string(s::String, i::Int)
    i <= ncodeunits(s) && codeunit(s, i) == UInt8('"') || throw(ParseError("expected string at $i"))
    i += 1
    buf = IOBuffer()
    while true
        i <= ncodeunits(s) || throw(ParseError("unterminated string"))
        c = codeunit(s, i)
        if c == UInt8('"')
            return String(take!(buf)), i + 1
        elseif c == UInt8('\\')
            i += 1
            i <= ncodeunits(s) || throw(ParseError("unterminated string"))
            e = Char(codeunit(s, i))
            if e == 'u'
                u = _hex4(s, i + 1)
                i += 4
                if 0xd800 <= u < 0xdc00 && i + 6 <= ncodeunits(s) && codeunit(s, i + 1) == UInt8('\\') && codeunit(s, i + 2) == UInt8('u')
                    lo = _hex4(s, i + 3)
                    if 0xdc00 <= lo < 0xe000
                        print(buf, Char(0x10000 + ((UInt32(u) - 0xd800) << 10) + (UInt32(lo) - 0xdc00)))
                        i += 6
                    else
                        print(buf, '�')
                    end
                elseif 0xd800 <= u < 0xe000
                    print(buf, '�')
                else
                    print(buf, Char(u))
                end
            else
                m = get(_UNESC, e, nothing)
                m === nothing && throw(ParseError("bad escape \\$e"))
                print(buf, m)
            end
            i += 1
        else
            write(buf, c)
            i += 1
        end
    end
end
const _UNESC = Dict('"' => '"', '\\' => '\\', '/' => '/', 'b' => '\b', 'f' => '\f', 'n' => '\n', 'r' => '\r', 't' => '\t')

stringify(v) = (io = IOBuffer(); _write(io, v); String(take!(io)))

function _write(io::IO, s::AbstractString)
    write(io, '"')
    for c in s
        if c == '"'
            write(io, "\\\"")
        elseif c == '\\'
            write(io, "\\\\")
        elseif c == '\n'
            write(io, "\\n")
        elseif c == '\r'
            write(io, "\\r")
        elseif c == '\t'
            write(io, "\\t")
        elseif c < ' '
            write(io, "\\u", string(UInt16(c); base=16, pad=4))
        else
            write(io, c)
        end
    end
    write(io, '"')
end
_write(io::IO, ::Nothing) = write(io, "null")
_write(io::IO, b::Bool) = write(io, b ? "true" : "false")
_write(io::IO, n::Integer) = print(io, n)
_write(io::IO, n::AbstractFloat) = isfinite(n) ? print(io, n) : write(io, "null")
_write(io::IO, s::Symbol) = _write(io, String(s))
function _write(io::IO, a::AbstractVector)
    write(io, '[')
    for (n, v) in enumerate(a)
        n > 1 && write(io, ',')
        _write(io, v)
    end
    write(io, ']')
end
function _write(io::IO, d::Union{AbstractDict,NamedTuple})
    write(io, '{')
    first = true
    for (k, v) in pairs(d)
        first || write(io, ',')
        first = false
        _write(io, string(k))
        write(io, ':')
        _write(io, v)
    end
    write(io, '}')
end

end
