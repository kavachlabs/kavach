# Helper process of the crash test: records one step that completes, then one
# that kills the process outright once its input is on record.
#   julia --project=sdk/julia killed.jl <dir> ring|pipe
using Kavach

struct Killer end

function Kavach.handle!(::Killer, env::Env, in::Input)
    if String(copy(in.data)) == "die"
        ccall(:kill, Cint, (Cint, Cint), getpid(), 9)
        sleep(10)
    end
    now_ns(env)
    return nothing
end

rec = Recorder(Killer(); service="killed", dir=ARGS[1], ring=ARGS[2] == "ring")
step!(rec, Input("t", "0", Vector{UInt8}(codeunits("fine"))))
step!(rec, Input("t", "1", Vector{UInt8}(codeunits("die"))))
