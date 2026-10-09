# Precompile workload (stdlib only). A host's first `ready` has to reach the
# driver within its per-message timeout, and Julia would otherwise compile
# the protocol code on that first message.
struct PrecompileProbe end

function handle!(::PrecompileProbe, env::Env, inp::Input)
    now_ns(env)
    random(env, 2)
    try
        query(env, "g", UInt8[0x41])
    catch e
        e isa GatewayError || rethrow()
    end
    config(env, "k")
    emit!(env, "s", UInt8[0x78])
    inp.data == UInt8[0x31] && throw(Panic("boom"))
    inp.data == UInt8[0x32] && throw(HandlerError("bad"))
    inp.data == UInt8[0x33] && sum(nothing)
    return nothing
end
snapshot(::PrecompileProbe) = UInt8[0x30]
restore!(::PrecompileProbe, ::Vector{UInt8}) = nothing
invariants(::PrecompileProbe) = Pair{String,Function}["ok" => () -> true]

function precompile_workload()
    with_logger(NullLogger()) do
        driver = Base.BufferStream()
        out = Base.BufferStream()
        step(d) = (t="step", seq="0", source="s", position="0", data=b64(d))
        for m in (
            (t="hello", protocol=1, service="p", start="snapshot", snapshot=b64(UInt8[0x30]), mode="process"),
            step(UInt8[0x30]), (t="clock", unix_nanos="1759752000000000000"), (t="rand", data=b64(UInt8[1, 2])),
            (t="gateway", response=b64(UInt8[1])), (t="config", present=true, value=b64(UInt8[1])),
            step(UInt8[0x30]), (t="clock", unix_nanos="1"), (t="rand", data=b64(UInt8[1, 2])),
            (t="gateway", error="timeout"), (t="config", present=false),
            step(UInt8[0x31]), (t="abort", detail="x"),
            step(UInt8[0x32]), (t="clock", unix_nanos="1"), (t="rand", data=b64(UInt8[1, 2])),
            (t="gateway", response=b64(UInt8[1])), (t="config", present=false),
            step(UInt8[0x33]), (t="clock", unix_nanos="1"), (t="rand", data=b64(UInt8[1, 2])),
            (t="gateway", response=b64(UInt8[1])), (t="config", present=false),
            (t="end",),
        )
            write(driver, Json.stringify(m), '\n')
        end
        close(driver)
        run_host(Host(() -> PrecompileProbe(), driver, out, nothing, () -> Dict{String,Any}(), false))
        close(out)

        rec = Recorder(PrecompileProbe(); service="p", snapshots=true, gateways=n -> (_ -> UInt8[1]), config=k -> UInt8[1],
                       flags=() -> ["flag.x" => "on"], recorder_command=["sh", "-c", "cat >/dev/null"], close_timeout=0.05)
        for d in (UInt8[0x30], UInt8[0x31], UInt8[0x33])
            step!(rec, Input("s", "0", d))
        end
        flush!(rec)
        close(rec)
    end
    return nothing
end

if ccall(:jl_generating_output, Cint, ()) == 1
    try
        precompile_workload()
    catch e
        @warn "Kavach precompile workload failed" exception = e
    end
end
