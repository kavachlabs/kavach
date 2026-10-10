# Runs SDK recorder cases (spec/recorder/sdk/README.md) through this SDK:
#   julia --project=sdk/julia sdk/julia/conformance/recorder_case.jl [case.json ...]
# With no arguments every case in $KAVACH_SPEC_DIR/recorder/sdk is run. Exits 1 if any fails.
using Kavach
using Kavach: Json
using Base64
isdefined(@__MODULE__, :KavachConformance) || include(joinpath(@__DIR__, "handler.jl"))
using .KavachConformance

spec_dir() = get(ENV, "KAVACH_SPEC_DIR", normpath(joinpath(@__DIR__, "..", "..", "..", "spec")))

# Serves one step's scripted answers, in order, per kind.
mutable struct Answers
    q::Dict{String,Vector{Any}}
end

function pop_answer!(a::Answers, kind)
    q = get(a.q, kind, Any[])
    isempty(q) && error("the handler read a $kind the case has no answer for")
    return popfirst!(q)
end

"""Runs one case over the ring or the pipe; returns the fake recorder's result.json contents."""
function run_case(path::AbstractString; ring::Bool=true, fake=joinpath(spec_dir(), "recorder", "sdk", "fake_recorder.py"))
    case = Json.parse(read(path, String))
    handler = Conformance()
    haskey(case, "snapshot") && Kavach.restore!(handler, Vector{UInt8}(codeunits(case["snapshot"])))
    answers = Answers(Dict{String,Vector{Any}}())
    flags = [k => v for (k, v) in get(case, "flags", Dict())]
    mktempdir() do tmp
        result_path = joinpath(tmp, "result.json")
        function gateway(request)
            a = pop_answer!(answers, "gateway")
            haskey(a, "error") && throw(Kavach.GatewayError(a["error"]))
            return base64decode(a["response"])
        end
        function rand_bytes(n)
            data = base64decode(pop_answer!(answers, "rand"))
            length(data) == n || error("case rand answer is $(length(data)) bytes, handler asked for $n")
            return data
        end
        function config(_)
            a = pop_answer!(answers, "config")
            return get(a, "unset", false) === true ? nothing : base64decode(a["value"])
        end
        rec = Recorder(handler; service=case["open"]["service"], start=Symbol(case["open"]["start"]),
                       snapshots=case["open"]["snapshots"],
                       recorder_command=["python3", fake, path, result_path],
                       gateways=AnyGateway(gateway), config=config, config_source="case",
                       flags=isempty(flags) ? nothing : () -> flags,
                       clock_ns=() -> parse(Int64, pop_answer!(answers, "clock")), random_bytes=rand_bytes,
                       required=true, ring=ring)
        for action in case["actions"]
            if haskey(action, "step")
                s = action["step"]
                answers.q = Dict{String,Vector{Any}}(k => Vector{Any}(v) for (k, v) in get(action, "answers", Dict()))
                step!(rec, Input(s["source"], s["position"], base64decode(s["data"])))
            elseif haskey(action, "flush")
                flush!(rec; durable=get(action["flush"], "durable", false))
            end
        end
        close(rec)
        isfile(result_path) || return Dict{String,Any}("pass" => false, "error" => "the fake recorder wrote no result (did the SDK close it?)")
        result = Json.parse(read(result_path, String))
        want = ring ? "ring" : "pipe"
        if get(result, "pass", false) === true && get(result, "transport", nothing) != want
            return Dict{String,Any}("pass" => false, "error" => "the case ran over the $(get(result, "transport", "unknown transport")), want the $want")
        end
        return result
    end
end

function main(args)
    paths = isempty(args) ? sort(filter(endswith(".json"), readdir(joinpath(spec_dir(), "recorder", "sdk"); join=true))) : args
    failed = 0
    for p in paths
        for ring in (false, true)
            result = try
                run_case(p; ring)
            catch e
                Dict{String,Any}("pass" => false, "error" => sprint(showerror, e))
            end
            ok = get(result, "pass", false) === true
            println(ok ? "PASS  " : "FAIL  ", basename(p), ring ? " (ring)" : " (pipe)")
            ok || (failed += 1; println(stderr, result["error"]))
        end
    end
    println("$(2 * length(paths) - failed)/$(2 * length(paths)) runs passed")
    return failed == 0 ? 0 : 1
end

abspath(PROGRAM_FILE) == (@__FILE__) && exit(main(ARGS))
