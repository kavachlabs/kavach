"""One event consumed by a handler."""
struct Input
    source::String
    position::String
    data::Vector{UInt8}
end

"""One effect a handler requested with [`emit!`](@ref)."""
struct Output
    sink::String
    data::Vector{UInt8}
    islocal::Bool
end

"""The handler's window on the world. Handlers read time, randomness, gateways
and config only through it, and emit outputs only through it."""
abstract type Env end

"""A gateway query failed; `error` is what the connection reported."""
struct GatewayError <: Exception
    error::String
end
Base.showerror(io::IO, e::GatewayError) = print(io, "GatewayError: ", e.error)

"""Throw to fail a step as a `panic` whose marker message is exactly `message`."""
struct Panic <: Exception
    message::String
end
Base.showerror(io::IO, e::Panic) = print(io, e.message)

"""Throw to fail a step as an `error` whose marker message is exactly `message`."""
struct HandlerError <: Exception
    message::String
end
Base.showerror(io::IO, e::HandlerError) = print(io, e.message)

"""Raised when the recorder is required and could not be started."""
struct RecorderError <: Exception
    message::String
end
Base.showerror(io::IO, e::RecorderError) = print(io, "RecorderError: ", e.message)

"""A registered gateway: the `connection(request::Vector{UInt8})::Vector{UInt8}`
that makes the query, and its scope."""
struct Gateway
    connection::Function
    islocal::Bool
end
Gateway(connection::Function; islocal::Bool=false) = Gateway(connection, islocal)

# Unwinds a step after the driver answered a request with `abort` (§9.4).
struct Aborted <: Exception end

"""    handle!(handler, env, input)

Run one step. Define a method for your handler type."""
function handle! end

"""    snapshot(handler)::Vector{UInt8}

Optional: the handler's state. With [`restore!`](@ref) it enables snapshots."""
function snapshot end

"""    restore!(handler, data::Vector{UInt8})"""
function restore! end

"""    invariants(handler)

Optional: `Pair{String,Function}`s checked after every step that ended ok. An
invariant holds when its function returns without throwing and not `false`."""
const NO_INVARIANTS = Pair{String,Function}[]
invariants(_) = NO_INVARIANTS

can_snapshot(h) = hasmethod(snapshot, Tuple{typeof(h)}) && hasmethod(restore!, Tuple{typeof(h),Vector{UInt8}})

"""    now_ns(env)::Int64

Unix nanoseconds."""
function now_ns end

"""    random(env, n)::Vector{UInt8}"""
function random end

"""    query(env, gateway, request)::Vector{UInt8}

Throws [`GatewayError`](@ref) if the query failed."""
function query end

"""    config(env, key)::Union{Vector{UInt8},Nothing}"""
function config end

"""    emit!(env, sink, data; islocal=false)

(`local` is a reserved word in Julia, hence `islocal`.)"""
function emit! end

"""    now(env)::DateTime

The clock as a UTC `DateTime` (millisecond precision; see [`now_ns`](@ref))."""
now(env::Env) = Dates.DateTime(1970) + Dates.Millisecond(now_ns(env) ÷ 1_000_000)

to_bytes(b::Vector{UInt8}) = b
to_bytes(b::AbstractVector{UInt8}) = Vector{UInt8}(b)
to_bytes(s::AbstractString) = Vector{UInt8}(codeunits(s))

runtime() = "julia-$(VERSION)"

# What went wrong in a step, as the marker records it.
struct Failure
    kind::String
    message::String
    detail::String
end

# §4.5: the message must not depend on where the build runs, so it keeps the
# first line of the exception text without source locations and addresses; the
# full text and the stack go in `detail`.
function stable_message(text::AbstractString)
    line = first(split(text, '\n'; limit=2))
    line = replace(line, r"\s*(?:@\s+\S+\s+|\bat\s+)\S+:\d+(?::\d+)?" => "",
                   r"\S*\.jl:\d+(?::\d+)?" => "", r"0x[0-9a-fA-F]{6,}" => "0x?")
    return String(strip(line))
end

function classify(e, bt)
    e isa Panic && return Failure("panic", e.message, "")
    e isa HandlerError && return Failure("error", e.message, "")
    text = sprint(showerror, e)
    return Failure("panic", stable_message(text), text * "\n" * sprint(Base.show_backtrace, bt))
end

function check_invariants(handler)
    for (name, check) in invariants(handler)
        try
            check() === false && return Failure("invariant", name, "returned false")
        catch e
            return Failure("invariant", name, sprint(showerror, e))
        end
    end
    return nothing
end

resolve_gateway(g::AbstractDict, name) = haskey(g, name) ? _gw(g[name]) : throw(GatewayError("no gateway named $name"))
resolve_gateway(g::Function, name) = _gw(g(name))
resolve_gateway(::Nothing, name) = throw(GatewayError("no gateway named $name"))
_gw(g::Gateway) = g
_gw(f::Function) = Gateway(f, false)

# Returns (response, error); a failing connection is a recorded error, not a crash.
function call_gateway(gw::Gateway, request::Vector{UInt8})
    try
        return to_bytes(gw.connection(request)), ""
    catch e
        msg = e isa GatewayError ? e.error : sprint(showerror, e)
        return UInt8[], msg
    end
end
