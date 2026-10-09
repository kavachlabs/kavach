const PROTOCOL = 1
const HOST_ARG = "kavach-host"

# The protocol was violated; the host sends `fatal` and stops.
struct Fatal <: Exception
    message::String
end
# The driver closed the pipe.
struct Gone <: Exception end

b64(b::AbstractVector{UInt8}) = Base64.base64encode(b)

function unb64(s)
    s isa AbstractString || throw(Fatal("expected a base64 string"))
    try
        return Base64.base64decode(s)
    catch
        throw(Fatal("invalid base64"))
    end
end

"""`ready.environment`: the output of `kavach-recorder facts` if it can be run,
plus `host.runtime` (§9.2)."""
function collect_environment(recorder_command=nothing)
    env = Dict{String,Any}()
    cmd = find_recorder(recorder_command)
    if cmd !== nothing
        try
            p = open(pipeline(Cmd([cmd; "facts"]); stdin=devnull, stderr=devnull), "r")
            t = Threads.@spawn read(p, String)
            if timedwait(() -> istaskdone(t), 10.0; pollint=0.005) == :ok
                facts = Json.parse(fetch(t))
                facts isa Dict && merge!(env, Dict(k => v for (k, v) in facts if v isa Dict))
            else
                kill(p)
            end
        catch
        end
    end
    env["host.runtime"] = (value=b64(to_bytes(runtime())),)
    return env
end

mutable struct Host
    factory::Function
    rd::IO
    wr::IO
    gateways::Any
    environment::Function
    mode::String
    aborted::Bool
end

function send(h::Host, msg)
    try
        write(h.wr, Json.stringify(msg), '\n')
        flush(h.wr)
    catch
        throw(Gone())
    end
end

function recv(h::Host)
    line = try
        readline(h.rd)
    catch
        throw(Gone())
    end
    isempty(line) && eof(h.rd) && throw(Gone())
    msg = try
        Json.parse(line)
    catch
        throw(Fatal("malformed message: not JSON"))
    end
    msg isa Dict && get(msg, "t", nothing) isa AbstractString || throw(Fatal("malformed message: no type"))
    return msg
end

# Sends one request and waits for its answer; unwinds on `abort`.
function request(h::Host, msg, want::String)
    h.aborted && throw(Aborted())
    send(h, msg)
    ans = recv(h)
    if ans["t"] == "abort"
        h.aborted = true
        throw(Aborted())
    end
    ans["t"] == want || throw(Fatal("expected a '$want' answer, got '$(ans["t"])'"))
    return ans
end

"""Serve the driver until `end` or EOF; returns the exit status."""
function run_host(h::Host)
    handler = nothing
    try
        while true
            msg = try
                recv(h)
            catch e
                e isa Gone && return 0
                rethrow()
            end
            t = msg["t"]
            if t == "hello"
                handler = hello(h, msg)
            elseif t == "step"
                handler === nothing && throw(Fatal("step before hello"))
                host_step(h, handler, msg)
            elseif t == "end"
                return 0
            elseif t == "abort"
                continue
            else
                throw(Fatal("unexpected message '$t'"))
            end
        end
    catch e
        e isa Fatal || e isa Gone || rethrow()
        if e isa Fatal
            try; send(h, (t="fatal", message=e.message)); catch; end
        end
        return 1
    end
end

function hello(h::Host, msg::Dict)
    get(msg, "protocol", nothing) == PROTOCOL || throw(Fatal("unsupported protocol $(get(msg, "protocol", nothing))"))
    h.mode = something(get(msg, "mode", nothing), "process")
    h.mode == "sandbox" && throw(Fatal("sandbox mode not supported"))
    handler = try
        h.factory()
    catch e
        throw(Fatal("could not create the handler: " * sprint(showerror, e)))
    end
    if get(msg, "start", nothing) == "snapshot"
        can_snapshot(handler) || throw(Fatal("the journal starts from a snapshot but the handler has no restore!"))
        try
            restore!(handler, unb64(get(msg, "snapshot", "")))
        catch e
            e isa Fatal && rethrow()
            throw(Fatal("could not restore the snapshot: " * sprint(showerror, e)))
        end
    end
    send(h, (t="ready", protocol=PROTOCOL, sdk=PRODUCER, invariants=String[n for (n, _) in invariants(handler)],
             environment=h.environment()))
    return handler
end

function host_step(h::Host, handler, msg::Dict)
    inp = Input(string(get(msg, "source", "")), string(get(msg, "position", "")), unb64(get(msg, "data", "")))
    h.aborted = false
    failure = nothing
    try
        handle!(handler, HostEnv(h), inp)
    catch e
        (e isa Fatal || e isa Gone || e isa InterruptException) && rethrow()
        e isa Aborted || (failure = classify(e, catch_backtrace()))
    end
    if h.aborted
        send(h, (t="done", outcome="aborted"))
        return
    end
    failure === nothing && (failure = check_invariants(handler))
    if failure === nothing
        send(h, (t="done", outcome="ok"))
    elseif isempty(failure.detail)
        send(h, (t="done", outcome=failure.kind, message=failure.message))
    else
        send(h, (t="done", outcome=failure.kind, message=failure.message, detail=failure.detail))
    end
end

struct HostEnv <: Env
    host::Host
end

now_ns(env::HostEnv) = parse(Int64, string(request(env.host, (t="clock",), "clock")["unix_nanos"]))

function random(env::HostEnv, n::Integer)
    n <= 0 && return UInt8[]
    return unb64(get(request(env.host, (t="rand", n=n), "rand"), "data", ""))
end

function query(env::HostEnv, gateway::AbstractString, request_bytes)
    request_bytes = to_bytes(request_bytes)
    gw = resolve_gateway(env.host.gateways, gateway)
    ans = request(env.host, (t="gateway", gateway=gateway, request=b64(request_bytes), scope=gw.islocal ? "local" : "remote"), "gateway")
    if get(ans, "live", false) === true
        resp, err = call_gateway(gw, request_bytes)
        send(env.host, isempty(err) ? (t="observed", response=b64(resp)) : (t="observed", error=err))
    elseif haskey(ans, "error") && !isempty(string(ans["error"]))
        err, resp = string(ans["error"]), UInt8[]
    else
        err, resp = "", unb64(get(ans, "response", ""))
    end
    isempty(err) || throw(GatewayError(err))
    return resp
end

function config(env::HostEnv, key::AbstractString)
    ans = request(env.host, (t="config", key=key), "config")
    get(ans, "present", false) === true || return nothing
    return unb64(get(ans, "value", ""))
end

function emit!(env::HostEnv, sink::AbstractString, data; islocal::Bool=false)
    env.host.aborted && throw(Aborted())
    send(env.host, (t="emit", sink=sink, data=b64(to_bytes(data)), scope=islocal ? "local" : "remote"))
    return nothing
end

"""    maybe_host(args, factory; gateways=nothing)

If this process was started as a replay host (its last argument is
`kavach-host`), serve the driver and exit; otherwise return at once. Call it
first thing in `main`, before consuming input. `factory()` creates a fresh
handler; `gateways` maps names to [`Gateway`](@ref)s as for [`Recorder`](@ref).
"""
function maybe_host(args, factory::Function; gateways=nothing, environment=collect_environment)
    (isempty(args) || args[end] != HOST_ARG) && return
    # Take the protocol stream for ourselves, then point fd 1 at stderr so that
    # a handler that prints cannot corrupt it (§9.1).
    out_fd = ccall(:dup, Cint, (Cint,), 1)
    in_fd = ccall(:dup, Cint, (Cint,), 0)
    proto_out = Base.fdio(out_fd, true)
    proto_in = Base.fdio(in_fd, true)
    flush(stdout)
    ccall(:dup2, Cint, (Cint, Cint), 2, 1)
    redirect_stdout(stderr)
    devnull_fd = ccall(:open, Cint, (Cstring, Cint), "/dev/null", 0)
    ccall(:dup2, Cint, (Cint, Cint), devnull_fd, 0)
    ccall(:close, Cint, (Cint,), devnull_fd)
    code = run_host(Host(factory, proto_in, proto_out, gateways, environment, "process", false))
    try; close(proto_out); catch; end
    flush(stderr)
    exit(code)
end
