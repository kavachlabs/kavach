# Step overhead of the recorder, as BenchmarkRecorderStep in replay/replay_test.go:
# one clock read, one 8-byte random read, one emit, a 33-byte input, 4 events a step.
#   julia --project=sdk/julia sdk/julia/bench/recorder_step.jl [steps]
# Needs kavach-recorder on PATH or in $KAVACH_RECORDER. Prints the median ns/event of 5 runs.
using Kavach
using Statistics: median

struct BenchHandler end

function Kavach.handle!(::BenchHandler, env::Env, in::Input)
    now_ns(env)
    Kavach.random(env, 8)
    emit!(env, "entries", in.data)
    return nothing
end
Kavach.snapshot(::BenchHandler) = Vector{UInt8}(codeunits("{}"))
Kavach.restore!(::BenchHandler, _) = nothing

function run_once(steps; ring)
    mktempdir() do dir
        rec = Recorder(BenchHandler(); service="bench", dir, ring, random_bytes=n -> fill(0x2a, n), required=true)
        inp = Input("bench", "0", Vector{UInt8}(codeunits("{\"account\":\"alice\",\"amount\":10}   ")))
        for _ in 1:2_000   # warm up
            step!(rec, inp)
        end
        t = @elapsed for _ in 1:steps
            step!(rec, inp)
        end
        close(rec)
        return t * 1e9 / (steps * 4)
    end
end

steps = isempty(ARGS) ? 100_000 : parse(Int, ARGS[1])
for ring in (false, true)
    runs = [run_once(steps; ring) for _ in 1:5]
    println(rpad(ring ? "ring" : "pipe", 5), "median ", round(median(runs); digits=1), " ns/event  (runs: ", join(round.(runs; digits=1), ", "), ")")
end
