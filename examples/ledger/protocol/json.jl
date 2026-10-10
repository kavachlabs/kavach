# Just enough JSON for events and protocol messages: Julia's standard library
# has none. Objects parse to Dict{String,Any}, null to `nothing`.

struct JSONError <: Exception
    msg::String
end
Base.showerror(io::IO, e::JSONError) = print(io, "JSONError: ", e.msg)

function parsejson(s::AbstractString)
    s = String(s)
    v, i = _value(s, _ws(s, 1))
    _ws(s, i) <= ncodeunits(s) && throw(JSONError("trailing data"))
    return v
end

function _ws(s, i)
    while i <= ncodeunits(s) && s[i] in (' ', '\t', '\n', '\r')
        i += 1
    end
    return i
end

function _value(s, i)
    i > ncodeunits(s) && throw(JSONError("unexpected end"))
    c = s[i]
    if c == '{'
        d = Dict{String,Any}()
        i = _ws(s, i + 1)
        s[i] == '}' && return d, i + 1
        while true
            k, i = _string(s, _ws(s, i))
            i = _ws(s, i)
            s[i] == ':' || throw(JSONError("expected ':'"))
            d[k], i = _value(s, _ws(s, i + 1))
            i = _ws(s, i)
            s[i] == '}' && return d, i + 1
            s[i] == ',' || throw(JSONError("expected ',' or '}'"))
            i += 1
        end
    elseif c == '['
        a = Any[]
        i = _ws(s, i + 1)
        s[i] == ']' && return a, i + 1
        while true
            v, i = _value(s, _ws(s, i))
            push!(a, v)
            i = _ws(s, i)
            s[i] == ']' && return a, i + 1
            s[i] == ',' || throw(JSONError("expected ',' or ']'"))
            i += 1
        end
    elseif c == '"'
        return _string(s, i)
    elseif startswith(SubString(s, i), "true")
        return true, i + 4
    elseif startswith(SubString(s, i), "false")
        return false, i + 5
    elseif startswith(SubString(s, i), "null")
        return nothing, i + 4
    end
    m = match(r"-?\d+(\.\d+)?([eE][-+]?\d+)?", s, i)
    (m === nothing || m.offset != i) && throw(JSONError("unexpected '$c'"))
    n = m.match
    v = something(tryparse(Int64, n), parse(Float64, n))
    return v, i + ncodeunits(n)
end

const _ESC = Dict('"' => '"', '\\' => '\\', '/' => '/', 'b' => '\b', 'f' => '\f', 'n' => '\n', 'r' => '\r', 't' => '\t')

function _string(s, i)
    s[i] == '"' || throw(JSONError("expected a string"))
    io = IOBuffer()
    i += 1
    while true
        i > ncodeunits(s) && throw(JSONError("unterminated string"))
        c = s[i]
        if c == '"'
            return String(take!(io)), i + 1
        elseif c == '\\'
            e = s[i+1]
            if e == 'u'
                u = parse(UInt16, s[i+2:i+5]; base=16)
                i += 6
                if 0xd800 <= u < 0xdc00 # a surrogate pair
                    lo = parse(UInt16, s[i+2:i+5]; base=16)
                    u = 0x10000 + (UInt32(u - 0xd800) << 10) + (lo - 0xdc00)
                    i += 6
                end
                print(io, Char(u))
            else
                print(io, _ESC[e])
                i += 2
            end
        else
            print(io, c)
            i = nextind(s, i)
        end
    end
end

# writejson writes compact JSON; NamedTuple keys keep their order.
writejson(io::IO, v::AbstractString) = (print(io, '"'); foreach(c -> _char(io, c), v); print(io, '"'))
writejson(io::IO, v::Union{Integer,AbstractFloat}) = print(io, v)
writejson(io::IO, v::Bool) = print(io, v)
writejson(io::IO, ::Nothing) = print(io, "null")
function writejson(io::IO, v::AbstractVector)
    print(io, '[')
    for (n, x) in enumerate(v)
        n > 1 && print(io, ',')
        writejson(io, x)
    end
    print(io, ']')
end
function writejson(io::IO, v::Union{AbstractDict,NamedTuple})
    print(io, '{')
    for (n, (k, x)) in enumerate(pairs(v))
        n > 1 && print(io, ',')
        writejson(io, string(k))
        print(io, ':')
        writejson(io, x)
    end
    print(io, '}')
end
tojson(v) = sprint(writejson, v)

function _char(io, c)
    if c == '"' || c == '\\'
        print(io, '\\', c)
    elseif c == '\n'
        print(io, "\\n")
    elseif c < ' '
        print(io, "\\u", string(UInt16(c); base=16, pad=4))
    else
        print(io, c)
    end
end
