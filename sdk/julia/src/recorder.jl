"""The recorder argument vector: `command`, else `\$KAVACH_RECORDER`, else
`kavach-recorder` on `PATH`; `nothing` if there is none."""
function find_recorder(command=nothing)
    command !== nothing && return String[command...]
    env = get(ENV, "KAVACH_RECORDER", "")
    isempty(env) || return [env]
    found = Sys.which("kavach-recorder")
    return found === nothing ? nothing : [found]
end

"""What happened in one [`step!`](@ref)."""
struct StepResult
    ok::Bool
    kind::String       # "", "panic", "error" or "invariant"
    message::String
    detail::String
    outputs::Vector{Output}
end

mutable struct Recorder{H}
    handler::H
    deliver::Union{Function,Nothing}
    gateways::Any
    config::Function
    config_source::String
    clock_ns::Function
    random_bytes::Function
    on_fixture::Union{Function,Nothing}
    close_timeout::Float64
    snapshots::Bool
    lock::ReentrantLock
    active::Threads.Atomic{Bool}
    failed_logged::Bool
    closing::Bool
    closed::Bool
    closed_ack::Threads.Atomic{Bool}
    ready::Threads.Atomic{Bool}
    durable_count::Threads.Atomic{Int}
    snapshot_requested::Threads.Atomic{Bool}
    buf::Union{IOBuffer,Nothing}
    outputs::Vector{Output}
    ring::Union{Ring,Nothing}
    proc::Any
    reader::Union{Task,Nothing}
    file::Union{String,Nothing}
    run::Union{String,Nothing}
end

"""    Recorder(handler; service, start=:genesis, ...)

Runs `handler` step by step and writes everything to `kavach-recorder`.

Reads are recorded live (real clock, system random, the `gateways` and the
`config` provider); outputs go to `deliver(::Vector{Output})` only after the
step succeeded. If the recorder cannot be started, dies or reports a fatal
error, the problem is logged with `@error`, recording stops and steps carry on
unrecorded; with `required=true` a recorder that cannot be started throws
[`RecorderError`](@ref) instead.

Keywords: `service` (required), `start` (`:genesis` or `:snapshot`),
`snapshots`, `deliver`, `gateways` (a `Dict` or a function from name to
[`Gateway`](@ref) or connection function), `config` (key to bytes, string or
`nothing`; default reads `ENV`), `config_source`, `flags` (a function returning
`flag.*` key/value pairs), `recorder_command` (argument vector), `required`,
`handler_id`, `dir`, `compression`, `level`, `block_bytes`, `flush_ms`,
`segment_bytes`, `segment_seconds`, `retain_segments`, `secret_keys`,
`on_fixture`, `clock_ns`, `random_bytes`, `ring` (default `true`: carry the record stream over a
shared-memory ring, SPEC §10.7; `false` forces the pipe), `ring_bytes` (a power of two, at least 64 KiB), `ready_timeout`,
`close_timeout`.
"""
function Recorder(handler; service::AbstractString, start::Symbol=:genesis, snapshots=nothing, deliver=nothing,
                  gateways=nothing, config=nothing, config_source=nothing, flags=nothing, recorder_command=nothing,
                  required::Bool=false, handler_id=nothing, dir=nothing, compression=nothing, level=nothing,
                  block_bytes=nothing, flush_ms=nothing, segment_bytes=nothing, segment_seconds=nothing,
                  retain_segments=nothing, secret_keys=nothing, on_fixture=nothing, clock_ns=nothing,
                  random_bytes=nothing, ring::Bool=true, ring_bytes::Integer=RING_DEFAULT, ready_timeout::Real=10.0, close_timeout::Real=10.0)
    start in (:genesis, :snapshot) || throw(ArgumentError("start must be :genesis or :snapshot"))
    cs = can_snapshot(handler)
    start === :snapshot && !cs && throw(ArgumentError("start=:snapshot needs snapshot and restore! methods"))
    snaps = snapshots === nothing ? cs : snapshots
    snaps && !cs && throw(ArgumentError("snapshots=true needs snapshot and restore! methods"))
    rec = Recorder{typeof(handler)}(
        handler, deliver, gateways,
        config === nothing ? (k -> get(ENV, k, nothing)) : config,
        something(config_source, config === nothing ? "env" : "config"),
        clock_ns === nothing ? unix_nanos : clock_ns,
        random_bytes === nothing ? (n -> Random.rand(Random.RandomDevice(), UInt8, n)) : random_bytes,
        on_fixture, Float64(close_timeout), snaps, ReentrantLock(),
        Threads.Atomic{Bool}(false), false, false, false, Threads.Atomic{Bool}(false), Threads.Atomic{Bool}(false),
        Threads.Atomic{Int}(0), Threads.Atomic{Bool}(false), nothing, Output[], nothing, nothing, nothing, nothing, nothing)

    open_obj = Dict{String,Any}("protocol" => 1, "service" => service, "start" => String(start),
                                "producer" => PRODUCER, "snapshots" => snaps)
    for (k, v) in ("handler" => handler_id, "dir" => dir, "compression" => compression, "level" => level,
                   "block_bytes" => block_bytes, "flush_ms" => flush_ms, "segment_bytes" => segment_bytes,
                   "segment_seconds" => segment_seconds, "retain_segments" => retain_segments,
                   "secret_keys" => secret_keys === nothing ? nothing : collect(String, secret_keys))
        v === nothing || (open_obj[k] = v)
    end
    cmd = find_recorder(recorder_command)
    try
        cmd === nothing && throw(RecorderError("kavach-recorder not found (set recorder_command, \$KAVACH_RECORDER or put it on PATH)"))
        g = ring && Sys.isunix() ? new_ring(ring_bytes) : nothing
        if g !== nothing
            open_obj["ring"] = Int(g.cap)
            open_obj["ring_path"] = g.path
        end
        rec.ring = g
        spawn!(rec, cmd)
        # The open frame goes over standard input, every later one into the ring.
        write(rec.proc, Wire.frame(Wire.OPEN, io -> write(io, Json.stringify(open_obj))))
        flush(rec.proc)
        facts = Pair{String,Vector{UInt8}}["host.runtime" => to_bytes(runtime())]
        if flags !== nothing
            for (k, v) in flags()
                push!(facts, String(k) => to_bytes(v))
            end
        end
        write_frame(rec, Wire.facts_frame(facts))
        start === :snapshot && write_frame(rec, Wire.snapshot_frame(to_bytes(snapshot(handler))))
        if required
            timedwait(() -> rec.ready[], Float64(ready_timeout); pollint=0.005)
            rec.ready[] && rec.active[] || throw(RecorderError("kavach-recorder did not become ready"))
        end
    catch e
        if required
            kill_recorder(rec)
            e isa RecorderError && rethrow()
            throw(RecorderError("kavach-recorder could not be started: " * sprint(showerror, e)))
        end
        fail(rec, "could not start the recorder: " * sprint(showerror, e))
    end
    atexit(() -> close(rec))
    return rec
end

# A ring that cannot be set up is not an error: the pipe carries the stream.
function new_ring(capacity)
    try
        return Ring(ring_dir(), capacity)
    catch e
        @warn "kavach: no shared-memory ring, recording over the pipe: $(sprint(showerror, e))"
        return nothing
    end
end

# The recorder unlinks the ring file once it has opened it; this covers one that never did.
rm_ring(rec::Recorder) = rec.ring === nothing || rm(rec.ring.path; force=true)

unix_nanos() = (tv = Libc.TimeVal(); Int64(tv.sec) * 1_000_000_000 + Int64(tv.usec) * 1_000)

# shortcut: the pipe keeps its default size (§10.2 asks for 1 MiB on Linux); upgrade if steps block on a full pipe.
function spawn!(rec::Recorder, cmd::Vector{String})
    # The environment is left unchanged: the recorder reads env. facts from it (§10.1).
    rec.proc = open(pipeline(Cmd(cmd); stderr=stderr), "r+")
    rec.active[] = true
    rec.reader = Threads.@spawn control_loop(rec)
end

function kill_recorder(rec::Recorder)
    rec.active[] = false
    rm_ring(rec)
    p = rec.proc
    p === nothing && return
    process_running(p) && kill(p)
    try; close(p); catch; end
end

"""Stop recording, loudly, once. Never throws."""
function fail(rec::Recorder, reason::AbstractString)
    was = rec.active[]
    rec.active[] = false
    if !rec.failed_logged && (was || !rec.closing)
        rec.failed_logged = true
        @error "kavach: $reason; recording has stopped and steps run unrecorded"
    end
end

function control_loop(rec::Recorder)
    try
        io = rec.proc
        while !eof(io)
            line = readline(io)
            isempty(line) && continue
            msg = try
                Json.parse(line)
            catch
                @warn "kavach: unreadable message from the recorder" line = first(line, 200)
                continue
            end
            msg isa Dict && haskey(msg, "t") && on_control(rec, string(msg["t"]), msg)
        end
    catch e
        @warn "kavach: control stream failed: $(sprint(showerror, e))"
    end
    rec.closing || fail(rec, "the recorder exited unexpectedly")
    rec.closed_ack[] = true
    rec.ready[] = true
end

function on_control(rec::Recorder, t::String, msg::Dict)
    if t == "ready"
        rec.file, rec.run = get(msg, "file", nothing), get(msg, "run", nothing)
        rec.ready[] = true
    elseif t == "snapshot_request"
        rec.snapshot_requested[] = true
    elseif t == "durable"
        Threads.atomic_add!(rec.durable_count, 1)
    elseif t == "fixture"
        @warn "kavach: wrote fixture $(get(msg, "file", "?")) (input seq $(get(msg, "seq", "?")), $(get(msg, "failure", "?")))"
        if rec.on_fixture !== nothing
            try
                rec.on_fixture(msg)
            catch e
                @error "kavach: on_fixture callback failed: $(sprint(showerror, e))"
            end
        end
    elseif t == "error"
        fatal = get(msg, "fatal", false) === true
        @error "kavach: recorder $(fatal ? "fatal error" : "error"): $(get(msg, "message", ""))"
        fatal && fail(rec, "the recorder reported a fatal error: $(get(msg, "message", ""))")
    elseif t == "closed"
        rec.closed_ack[] = true
    end
end

function write_frame(rec::Recorder, data::Vector{UInt8}; bell::Bool=false)
    (rec.active[] && !isempty(data)) || return
    try
        g = rec.ring
        if g === nothing
            write(rec.proc, data)
            flush(rec.proc)
        else
            publish(rec, g, data)
            bell && ring_bell(rec)
        end
    catch e
        fail(rec, "could not write to the recorder: " * sprint(showerror, e))
    end
end

function publish(rec::Recorder, g::Ring, data::Vector{UInt8})
    from = 1
    while from <= length(data)
        n, used = try_publish(g, data, from)
        if n == 0
            # Full: the recorder drains the ring on the doorbell. If it has
            # exited, the control task stops recording and ends the wait.
            ring_bell(rec)
            while n == 0 && rec.active[]
                sleep(0.0001)
                n, used = try_publish(g, data, from)
            end
            n == 0 && return
        end
        from += n
        if used > g.cap ÷ 2
            g.belled || ring_bell(rec)
            g.belled = true
        else
            g.belled = false
        end
    end
end

# Wakes a recorder that reads the ring (SPEC §10.7).
function ring_bell(rec::Recorder)
    rec.active[] || return
    try
        write(rec.proc, 0x01)
        flush(rec.proc)
    catch
        # A recorder that has read `close` may already be gone.
        rec.closing || rethrow()
    end
end

buffer_frame(rec::Recorder, data::Vector{UInt8}) = (rec.buf !== nothing && rec.active[] && write(rec.buf, data); nothing)

"""    step!(rec, input::Input)::StepResult

Run the handler on one input. A handler failure is returned, not thrown (an
`InterruptException` is recorded as a panic and then rethrown)."""
function step!(rec::Recorder, inp::Input)
    lock(rec.lock) do
        rec.closed && error("kavach: step on a closed Recorder")
        answer_snapshot_request(rec)
        rec.buf = IOBuffer()
        empty!(rec.outputs)
        # The input goes in before the handler runs, so that a step that kills
        # the process still leaves it on record (§10.2).
        write_frame(rec, Wire.input_record(inp.source, inp.position, inp.data))
        failure = nothing
        interrupted = nothing
        try
            handle!(rec.handler, RecordEnv(rec), inp)
        catch e
            failure = classify(e, catch_backtrace())
            e isa InterruptException && (interrupted = e)
        end
        failure === nothing && (failure = check_invariants(rec.handler))
        failure === nothing || buffer_frame(rec, Wire.marker_record(failure.kind, failure.message, Vector{UInt8}(codeunits(failure.detail))))
        buffer_frame(rec, Wire.step_end_frame())
        data = take!(rec.buf)
        outputs = copy(rec.outputs)
        rec.buf = nothing
        write_frame(rec, data)
        if failure === nothing
            rec.deliver === nothing || isempty(outputs) || rec.deliver(outputs)
            result = StepResult(true, "", "", "", outputs)
        else
            result = StepResult(false, failure.kind, failure.message, failure.detail, outputs)
        end
        interrupted === nothing || throw(interrupted)
        return result
    end
end

function answer_snapshot_request(rec::Recorder)
    rec.snapshot_requested[] || return
    rec.snapshot_requested[] = false
    (rec.active[] && rec.snapshots) || return
    data = try
        to_bytes(snapshot(rec.handler))
    catch e
        @error "kavach: snapshot failed; staying in the current segment: $(sprint(showerror, e))"
        return
    end
    write_frame(rec, Wire.snapshot_frame(data))
end

"""    flush!(rec; durable=false, timeout=10)

Ask the recorder to close its open block now. With `durable=true` also wait
until it is on disk. Returns false if recording has stopped or the wait timed
out. Call it between steps."""
function flush!(rec::Recorder; durable::Bool=false, timeout::Real=10.0)
    before = 0
    sent = lock(rec.lock) do
        rec.active[] || return false
        before = rec.durable_count[]
        write_frame(rec, Wire.flush_frame(durable); bell=true)
        true
    end
    sent || return false
    durable || return rec.active[]
    timedwait(() -> rec.durable_count[] > before || !rec.active[], Float64(timeout); pollint=0.002)
    return rec.durable_count[] > before
end

"""Orderly shutdown: send `close` and wait for `closed`."""
function Base.close(rec::Recorder)
    lock(rec.lock) do
        rec.closed && return
        rec.closed = true
        rec.proc === nothing && return rm_ring(rec)
        if rec.active[]
            rec.closing = true
            write_frame(rec, Wire.close_frame(); bell=true)
            timedwait(() -> rec.closed_ack[], rec.close_timeout; pollint=0.002) == :ok ||
                @warn "kavach: the recorder did not answer close within $(rec.close_timeout)s"
        end
        rec.closing = true
        rec.active[] = false
        try; close(rec.proc.in); catch; end
        timedwait(() -> !process_running(rec.proc), 2.0; pollint=0.005) == :ok ||
            @warn "kavach: the recorder is still running after close"
        rec.reader === nothing || timedwait(() -> istaskdone(rec.reader), 2.0; pollint=0.005)
        try; close(rec.proc); catch; end
        rm_ring(rec)
    end
    return nothing
end

"""Whether the recorder is still recording."""
recording(rec::Recorder) = rec.active[]

struct RecordEnv <: Env
    rec::Recorder
end

function now_ns(env::RecordEnv)
    ns = Int64(env.rec.clock_ns())
    buffer_frame(env.rec, Wire.clock_record(ns))
    return ns
end

function random(env::RecordEnv, n::Integer)
    n <= 0 && return UInt8[]
    data = to_bytes(env.rec.random_bytes(n))
    buffer_frame(env.rec, Wire.rand_record(data))
    return data
end

function query(env::RecordEnv, gateway::AbstractString, request)
    request = to_bytes(request)
    gw = resolve_gateway(env.rec.gateways, gateway)
    resp, err = call_gateway(gw, request)
    buffer_frame(env.rec, Wire.gateway_record(gateway, request, resp, err, gw.islocal))
    isempty(err) || throw(GatewayError(err))
    return resp
end

function config(env::RecordEnv, key::AbstractString)
    v = env.rec.config(key)
    v = v === nothing ? nothing : to_bytes(v)
    buffer_frame(env.rec, Wire.config_record(key, v, env.rec.config_source))
    return v
end

function emit!(env::RecordEnv, sink::AbstractString, data; islocal::Bool=false)
    data = to_bytes(data)
    push!(env.rec.outputs, Output(sink, data, islocal))
    buffer_frame(env.rec, Wire.output_record(sink, data, islocal))
    return nothing
end
