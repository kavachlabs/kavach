using Test
using Kavach
using Kavach: Json, Wire, Failure, Host
using Logging

const PKG = dirname(@__DIR__)
const SPEC = get(ENV, "KAVACH_SPEC_DIR", normpath(joinpath(@__DIR__, "..", "..", "..", "spec")))
const PYTHON = Sys.which("python3")

include(joinpath(PKG, "conformance", "handler.jl"))
using .KavachConformance

struct Thrower end
Kavach.handle!(::Thrower, env::Kavach.Env, in::Input) = sum(nothing)

@testset "Json" begin
    @test Json.parse("""{"a":[1,2.5,"x\\u00e9\\ud83d\\ude00\\n",true,null],"b":{}}""") ==
          Dict("a" => Any[1, 2.5, "xé😀\n", true, nothing], "b" => Dict{String,Any}())
    @test Json.stringify((t="x", n=1, s="a\"\\\n\u0001é", v=[true, nothing])) == "{\"t\":\"x\",\"n\":1,\"s\":\"a\\\"\\\\\\n\\u0001é\",\"v\":[true,null]}"
    @test_throws Json.ParseError Json.parse("{\"a\":")
    @test_throws Json.ParseError Json.parse("[1] x")
end

@testset "Wire" begin
    @test Wire.frame(Wire.STEP_END) == UInt8[0x01, 0x03]
    @test Wire.close_frame() == UInt8[0x01, 0x07]
    @test Wire.flush_frame(true) == UInt8[0x02, 0x06, 0x01]
    io = IOBuffer()
    Wire.uvarint!(io, 300)
    @test take!(io) == UInt8[0xac, 0x02]
end

@testset "failure messages are location independent (SPEC §4.5)" begin
    text = "MethodError: no method matching f(::Int64)\nClosest candidates are:\n  f(::String)\n   @ Main /home/a/b/file.jl:12"
    @test Kavach.stable_message(text) == "MethodError: no method matching f(::Int64)"
    @test Kavach.stable_message("BoundsError: attempt to access 3-element Vector{Int64} at index [7]") ==
          "BoundsError: attempt to access 3-element Vector{Int64} at index [7]"
    @test Kavach.stable_message("oops @ Main /x/y.jl:3 and at /x/y.jl:4:5 ptr 0x00007f8a1c2d3e40") == "oops and ptr 0x?"
    try
        sum(nothing)
    catch e
        f = Kavach.classify(e, catch_backtrace())
        @test f.kind == "panic"
        @test !occursin(".jl", f.message) && !occursin('\n', f.message) && !occursin("0x", f.message)
        @test occursin(".jl", f.detail) # the full text and the stack are kept
    end
    f = Kavach.classify(Kavach.Panic("exactly"), [])
    @test (f.kind, f.message) == ("panic", "exactly")
    f = Kavach.classify(Kavach.HandlerError("amount is null"), [])
    @test (f.kind, f.message) == ("error", "amount is null")
end

# Runs a host session in memory: the driver's lines are queued, then read back.
function session(factory, msgs)
    driver, out = Base.BufferStream(), Base.BufferStream()
    for m in msgs
        write(driver, Json.stringify(m), '\n')
    end
    close(driver)
    code = Kavach.run_host(Host(factory, driver, out, nothing, () -> Dict{String,Any}(), false))
    close(out)
    return code, [Json.parse(l) for l in eachline(out)]
end

@testset "host" begin
    code, replies = session(() -> Thrower(), [
        (t="hello", protocol=1, service="x", start="genesis", mode="process"),
        (t="step", seq="0", source="s", position="0", data=""), (t="end",)])
    @test code == 0
    @test replies[2]["outcome"] == "panic" && !occursin(".jl", replies[2]["message"])
    @test haskey(replies[2], "detail")

    code, replies = session(() -> Conformance(), [(t="hello", protocol=1, service="x", start="genesis", mode="sandbox")])
    @test code == 1
    @test replies == [Dict("t" => "fatal", "message" => "sandbox mode not supported")]

    code, replies = session(() -> Conformance(), [(t="step", seq="0", source="s", position="0", data="")])
    @test code == 1 && replies[1]["t"] == "fatal"
end

@testset "recorder failure never fails the step" begin
    with_logger(NullLogger()) do
        rec = Recorder(Conformance(); service="x", recorder_command=["/nonexistent/kavach-recorder"])
        r = step!(rec, Input("s", "0", Vector{UInt8}(codeunits("[]"))))
        @test r.ok && !Kavach.recording(rec)
        close(rec)
        @test_throws Kavach.RecorderError Recorder(Conformance(); service="x", required=true,
                                                   recorder_command=["/nonexistent/kavach-recorder"])
    end
end

if PYTHON === nothing || !isdir(SPEC)
    @warn "python3 or the spec directory is missing; skipping the transcript and recorder-case conformance tests" SPEC
else
    @testset "recorder cases (spec/recorder/sdk)" begin
        include(joinpath(PKG, "conformance", "recorder_case.jl"))
        for path in sort(filter(endswith(".json"), readdir(joinpath(SPEC, "recorder", "sdk"); join=true)))
            @testset "$(basename(path))" begin
                result = run_case(path)
                @test get(result, "pass", false) === true
                get(result, "pass", false) === true || @info result["error"]
            end
        end
    end

    @testset "host transcripts (spec/host)" begin
        host = "$(Base.julia_exename()) --project=$PKG $(joinpath(PKG, "conformance", "host.jl"))"
        pass = ["--pass-env=$v" for v in ("JULIA_DEPOT_PATH", "JULIA_LOAD_PATH", "JULIA_PROJECT") if haskey(ENV, v)]
        @test success(pipeline(`$PYTHON $(joinpath(SPEC, "host", "run.py")) --host $host $pass`; stdout=stdout, stderr=stderr))
    end
end
