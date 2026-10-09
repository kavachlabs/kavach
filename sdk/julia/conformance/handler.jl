"""The conformance handler of SPEC.md §9.6, exactly as specified."""
module KavachConformance

using Kavach
using Kavach: Json

export Conformance, AnyGateway

# §9.6: escape only " \ \n \r \t and other characters below U+0020 (as \u00xx,
# lowercase hex); everything else is raw UTF-8.
function jstr(s::AbstractString)
    io = IOBuffer()
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
    return String(take!(io))
end

unset_json() = Vector{UInt8}(codeunits("{\"unset\":true}"))

mutable struct Conformance
    count::Int64
end
Conformance() = Conformance(0)

# A gateway registry in which every name is a remote gateway.
AnyGateway(connection::Function=_ -> throw(Kavach.GatewayError("conformance host has no live connections"))) =
    _ -> Kavach.Gateway(connection)

function Kavach.handle!(h::Conformance, env::Kavach.Env, input::Input)
    for op in Json.parse(String(copy(input.data)))
        kind = op["op"]
        if kind == "clock"
            emit!(env, "trace", "{\"clock\":" * jstr(string(now_ns(env))) * "}")
        elseif kind == "rand"
            emit!(env, "trace", Kavach.random(env, op["n"]))
        elseif kind == "gateway"
            try
                emit!(env, "trace", query(env, op["gateway"], op["request"]))
            catch e
                e isa Kavach.GatewayError || rethrow()
                emit!(env, "trace", "{\"error\":" * jstr(e.error) * "}")
            end
        elseif kind == "config"
            v = Kavach.config(env, op["key"])
            emit!(env, "trace", v === nothing ? unset_json() : v)
        elseif kind == "getenv"
            v = get(ENV, op["name"], nothing)
            emit!(env, "trace", v === nothing ? unset_json() : v)
        elseif kind == "emit"
            emit!(env, op["sink"], op["data"])
        elseif kind == "panic"
            throw(Kavach.Panic(op["message"]))
        elseif kind == "error"
            throw(Kavach.HandlerError(op["message"]))
        elseif kind == "print"
            println(op["text"])
        elseif kind == "count"
            h.count += op["n"]
        else
            throw(Kavach.HandlerError("unknown operation $kind"))
        end
    end
    h.count += 1
    return nothing
end

Kavach.snapshot(h::Conformance) = Vector{UInt8}(codeunits(string(h.count)))
Kavach.restore!(h::Conformance, data::Vector{UInt8}) = (h.count = parse(Int64, String(copy(data))); nothing)
Kavach.invariants(h::Conformance) = Pair{String,Function}["below_limit" => () -> h.count < 1000 || error("count is $(h.count)")]

end
