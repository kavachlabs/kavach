# Live: the recorder protocol (SPEC.md §10). The ledger runs here, and every
# step is streamed as frames into kavach-recorder's standard input; the
# recorder writes the journal and, when a step fails, a fixture.

# Frame kinds (§10.2) and record types (§4).
const FRAME_OPEN, FRAME_RECORD, FRAME_STEP_END, FRAME_FACTS, FRAME_CLOSE = 0x01, 0x02, 0x03, 0x04, 0x07
const REC_INPUT, REC_CLOCK, REC_RAND, REC_OUTPUT, REC_MARKER = 0x01, 0x02, 0x03, 0x04, 0x05

mutable struct Recorder
    ledger::Ledger
    proc::Union{Base.Process,Nothing}
    off::Bool              # recording stopped; the ledger keeps running
    ready::Bool
    closed::Bool
    buf::IOBuffer          # frames of the current step after its input
    outs::Vector{Pair{String,Vector{UInt8}}} # the step's effects
end

# Start kavach-recorder from $KAVACH_RECORDER or PATH. If it cannot start, the
# ledger runs unrecorded: recording must never fail the service (§10.1).
function Recorder(ledger::Ledger, dir::String)
    r = Recorder(ledger, nothing, false, false, false, IOBuffer(), [])
    bin = get(ENV, "KAVACH_RECORDER", Sys.which("kavach-recorder"))
    if bin === nothing
        @warn "ledger: not recording: kavach-recorder not found"
        r.off = true
        return r
    end
    # The process's environment goes to the recorder unchanged: it collects
    # the env. facts from it.
    r.proc = open(pipeline(`$bin`; stderr=stderr), "r+")
    @async read_control(r)

    open_json = tojson((protocol=1, service="ledger", start="genesis", producer="ledger-protocol-example", dir=dir))
    frames = IOBuffer()
    frame!(frames, FRAME_OPEN, codeunits(open_json))
    # host.runtime is the one fact only the service knows (§10.4). A facts
    # payload is an environment record: a count, then key, form 0, value.
    facts = IOBuffer()
    uvarint!(facts, 1)
    string!(facts, "host.runtime")
    write(facts, 0x00)
    string!(facts, "julia-$VERSION")
    frame!(frames, FRAME_FACTS, take!(facts))
    send!(r, take!(frames))
    # Wait a bounded time for ready, so the first step does not fill the pipe
    # before the recorder reads it.
    timedwait(() -> r.ready || r.closed, 2.0)
    return r
end

# read_control reads the recorder's JSON Lines on a task of its own, so a step
# never waits on them (§10.3).
function read_control(r::Recorder)
    for line in eachline(r.proc)
        m = try
            parsejson(line)
        catch
            continue
        end
        t = get(m, "t", "")
        if t == "ready"
            r.ready = true
            @info "ledger: recording to $(m["file"])"
        elseif t == "fixture"
            @info "ledger: fixture for $(m["failure"]) at seq $(m["seq"]): $(m["file"])"
        elseif t == "error"
            @warn "ledger: recorder error" fatal = m["fatal"] message = m["message"]
        elseif t == "closed"
            break
        end
    end
    r.closed = true
end

# send! writes frames, waiting if the pipe is full: a record is never dropped
# silently (§3.6). If the recorder has gone, recording stops.
function send!(r::Recorder, frames::Vector{UInt8})
    r.off && return
    try
        write(r.proc, frames)
        flush(r.proc)
    catch e
        @warn "ledger: recorder gone, not recording" exception = e
        r.off = true
    end
end

# step! runs the handler on one input and returns (kind, message): kind is
# "ok", "panic", "error" or "invariant". A failed step ends with a marker, and
# the recorder writes a fixture for it. Effects are delivered only when the
# step succeeds.
function step!(r::Recorder, source::String, position::String, data::Vector{UInt8}, deliver)
    # The input goes out before the handler runs, so a step that kills the
    # process still leaves it on record (§10.2).
    input = IOBuffer()
    string!(input, source)
    string!(input, position)
    bytes!(input, data)
    send!(r, record_frame(REC_INPUT, take!(input)))

    empty!(r.outs)
    result = ("ok", "")
    try
        handle!(r.ledger, r, position, data)
        inv = check_invariants(r.ledger)
        if inv !== nothing
            marker!(r, "invariant", inv.first, inv.second)
            result = ("invariant", inv.first)
        end
    catch e
        kind, msg, detail = failure(e, catch_backtrace())
        marker!(r, kind, msg, detail)
        result = (kind, msg)
    end
    frame!(r.buf, FRAME_STEP_END, UInt8[])
    send!(r, take!(r.buf))
    result[1] == "ok" && deliver(r.outs)
    return result
end

# close! ends the journal and waits, bounded, for the recorder to finish it.
function close!(r::Recorder)
    r.proc === nothing && return
    send!(r, frame!(IOBuffer(), FRAME_CLOSE, UInt8[]) |> take!)
    r.off || timedwait(() -> r.closed, 10.0) == :ok || @warn "ledger: recorder did not close in time"
    close(r.proc.in)
    wait(r.proc)
end

# The world, live: do the real thing and record it.

function now_ns(r::Recorder)
    tv = Libc.TimeVal()
    ns = Int64(tv.sec) * 1_000_000_000 + Int64(tv.usec) * 1_000
    write(r.buf, record_frame(REC_CLOCK, reinterpret(UInt8, [htol(ns)])))
    return ns
end

function randbytes(r::Recorder, n::Integer)
    b = rand(RandomDevice(), UInt8, n)
    p = IOBuffer()
    bytes!(p, b)
    write(r.buf, record_frame(REC_RAND, take!(p)))
    return b
end

function emit!(r::Recorder, sink::String, data::String)
    d = Vector{UInt8}(codeunits(data))
    p = IOBuffer()
    string!(p, sink)
    bytes!(p, d)
    write(p, 0x00) # scope: remote
    write(r.buf, record_frame(REC_OUTPUT, take!(p)))
    push!(r.outs, sink => d)
end

function marker!(r::Recorder, kind, message, data)
    p = IOBuffer()
    string!(p, kind)
    string!(p, message)
    string!(p, data)
    write(r.buf, record_frame(REC_MARKER, take!(p)))
end

# The wire format (§2, §10.2).

# A record frame's payload is the record's type, its flags (0: none of the
# ledger's records is critical) and its fields. The recorder assigns seq.
record_frame(typ::UInt8, payload) = take!(frame!(IOBuffer(), FRAME_RECORD, [typ; 0x00; payload]))

# frame! writes uvarint len, kind, payload; len counts the kind byte and the
# payload.
function frame!(io::IO, kind::UInt8, payload)
    uvarint!(io, 1 + length(payload))
    write(io, kind, payload)
    return io
end

function uvarint!(io::IO, n::Integer)
    while n >= 0x80
        write(io, UInt8(n & 0x7f | 0x80))
        n >>= 7
    end
    write(io, UInt8(n))
end

bytes!(io::IO, b) = (uvarint!(io, length(b)); write(io, b))
string!(io::IO, s::AbstractString) = bytes!(io, codeunits(s))
