"""
    Kavach

The Julia SDK for Kavach: a flight recorder for a journal-driven handler
(SPEC.md §10) and a replay host (§9). Standard library only.
"""
module Kavach

using Base64: Base64
using Dates: Dates
using Logging
using Random: Random

include("json.jl")
include("wire.jl")
include("ring.jl")

const PRODUCER = "kavach-julia/0.1.0"

include("api.jl")
include("recorder.jl")
include("host.jl")

export Env, Input, Output, Gateway, GatewayError, Panic, HandlerError, RecorderError, Recorder, StepResult
export handle!, snapshot, restore!, invariants, now_ns, random, query, config, emit!, step!, flush!, maybe_host

include("precompile.jl")

end
