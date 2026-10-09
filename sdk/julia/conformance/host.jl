# Conformance host (SPEC.md §9.6): run with `kavach-host` as its last argument.
#   julia --project=sdk/julia sdk/julia/conformance/host.jl kavach-host
using Kavach
include(joinpath(@__DIR__, "handler.jl"))
using .KavachConformance

Kavach.maybe_host(ARGS, () -> Conformance(); gateways=AnyGateway())
