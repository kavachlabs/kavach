# Replay: the host protocol (SPEC.md §9). The kavach CLI starts the ledger with
# `kavach-host` as its last argument and drives it over standard input and
# output in JSON Lines, serving every read and capturing every effect. The
# host never consumes events or delivers effects.

mutable struct Host
    in::IO
    out::IO
    aborted::Bool
end

# Fatal stops the session with a `fatal` message; Abort unwinds a step the
# driver aborted (§9.4). The ledger catches neither: it has no try blocks
# around world calls.
struct Fatal <: Exception
    msg::String
end
struct Abort <: Exception end

# serve_host speaks the protocol until the driver sends `end`. Standard output
# carries only protocol messages, so the host keeps a copy of it and points
# fd 1 and `stdout` at standard error: a stray print cannot corrupt the stream.
function serve_host(fix::Bool)
    proto_out = Base.fdio(ccall(:dup, Cint, (Cint,), 1), true)
    ccall(:dup2, Cint, (Cint, Cint), 2, 1)
    redirect_stdout(stderr)
    h = Host(stdin, proto_out, false)
    try
        hello = recv(h)
        get(hello, "t", "") == "hello" && get(hello, "protocol", 0) == 1 ||
            throw(Fatal("want hello for protocol 1"))
        # The ledger never sends snapshots to the recorder, so its journals all
        # start at genesis.
        get(hello, "start", "") == "genesis" || throw(Fatal("cannot start from $(hello["start"])"))
        ledger = Ledger(fix)
        send(h, (t="ready", protocol=1, sdk="ledger-protocol-example", invariants=first.(INVARIANTS),
                 environment=environment()))
        while true
            m = recv(h)
            t = get(m, "t", "")
            t == "end" && return 0
            t == "step" || throw(Fatal("unexpected $t between steps"))
            send(h, step(h, ledger, m))
        end
    catch e
        e isa Fatal || rethrow()
        send(h, (t="fatal", message=e.msg))
        return 1
    end
end

# step runs one input and returns its `done` message. Failures map to outcomes
# exactly as the recorder maps them to markers, so a recorded failure and its
# replay compare equal.
function step(h::Host, ledger::Ledger, m)
    h.aborted = false
    try
        handle!(ledger, h, m["position"], base64decode(m["data"]))
    catch e
        e isa Fatal && rethrow()
        h.aborted && return (t="done", outcome="aborted")
        kind, msg, detail = failure(e, catch_backtrace())
        isempty(detail) && return (t="done", outcome=kind, message=msg)
        return (t="done", outcome=kind, message=msg, detail=detail)
    end
    inv = check_invariants(ledger)
    inv === nothing || return (t="done", outcome="invariant", message=inv.first, detail=inv.second)
    return (t="done", outcome="ok")
end

# request sends one read and waits for its answer.
function request(h::Host, msg, want::String)
    h.aborted && throw(Abort())
    send(h, msg)
    ans = recv(h)
    if ans["t"] == "abort"
        h.aborted = true
        throw(Abort())
    end
    ans["t"] == want || throw(Fatal("want a $want answer, got $(ans["t"])"))
    return ans
end

# The world, in replay: ask the driver.

now_ns(h::Host) = parse(Int64, request(h, (t="clock",), "clock")["unix_nanos"])
randbytes(h::Host, n::Integer) = base64decode(request(h, (t="rand", n=n), "rand")["data"])
function emit!(h::Host, sink::String, data::String)
    h.aborted && throw(Abort())
    send(h, (t="emit", sink=sink, data=base64encode(data), scope="remote"))
end

function send(h::Host, msg)
    writejson(h.out, msg)
    write(h.out, '\n')
    flush(h.out)
end

function recv(h::Host)
    eof(h.in) && throw(Fatal("driver closed the session without end"))
    try
        return parsejson(readline(h.in))
    catch e
        throw(Fatal("malformed message: " * sprint(showerror, e)))
    end
end

# environment is ready.environment: the facts `kavach-recorder facts` prints,
# plus host.runtime. Without a recorder, host.runtime alone (§9.2).
function environment()
    env = Dict{String,Any}()
    bin = get(ENV, "KAVACH_RECORDER", Sys.which("kavach-recorder"))
    if bin !== nothing
        try
            merge!(env, parsejson(read(pipeline(`$bin facts`; stdin=devnull, stderr=devnull), String)))
        catch
        end
    end
    env["host.runtime"] = (value=base64encode("julia-$VERSION"),)
    return env
end
