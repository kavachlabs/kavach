# Kavach's demo service, in Julia: a single-writer wallet ledger that folds
# JSON events into balances.
#
#   julia --project=sdk/julia sdk/julia/examples/ledger/ledger.jl          # the buggy build
#   julia --project=sdk/julia sdk/julia/examples/ledger/ledger.jl --fix    # the fixed build
#
# Both take `--in FILE` (default events.jsonl next to this script) and
# `--fixtures DIR`. The planted bug: an event with "amount": null reaches
# `amount <= 0` and throws a MethodError.
#
# The build is chosen by a command-line flag and not an environment variable on
# purpose: environment variables are served from the journal on replay, so a
# replay of the buggy build's fixture would otherwise run the buggy code.
# The replay hosts are therefore `... ledger.jl` (old) and `... ledger.jl --fix`
# (new). The recorder is found through $KAVACH_RECORDER or PATH; without it the
# service says so loudly and runs unrecorded.
module Ledger

using Kavach
using Kavach: Json
using Dates: Dates

mutable struct Handler
    fix::Bool
    balances::Dict{String,Int64}
    net::Int64 # deposits minus withdrawals
end
Handler(fix::Bool) = Handler(fix, Dict{String,Int64}(), 0)

function rfc3339nano(ns::Integer)
    dt = Dates.unix2datetime(ns ÷ 1_000_000_000)
    frac = rstrip(lpad(string(ns % 1_000_000_000), 9, '0'), '0')
    return Dates.format(dt, "yyyy-mm-ddTHH:MM:SS") * (isempty(frac) ? "" : "." * frac) * "Z"
end

function Kavach.handle!(l::Handler, env::Env, input::Input)
    ev = try
        Json.parse(String(copy(input.data)))
    catch e
        throw(HandlerError("decode event at $(input.position): " * sprint(showerror, e)))
    end
    amount = get(ev, "amount", nothing)
    if l.fix && amount === nothing
        return reject(env, ev, "missing amount")
    end
    amount <= 0 && return reject(env, ev, "amount must be positive") # MethodError when amount is null and fix is off
    at = rfc3339nano(now_ns(env))
    account = ev["account"]

    kind = ev["type"]
    if kind == "deposit"
        l.net += amount
        post(l, env, ev, account, amount, at)
    elseif kind == "withdraw"
        get(l.balances, account, 0) < amount && return reject(env, ev, "insufficient funds")
        l.net -= amount
        post(l, env, ev, account, -amount, at)
    elseif kind == "transfer"
        get(l.balances, account, 0) < amount && return reject(env, ev, "insufficient funds")
        post(l, env, ev, account, -amount, at)
        post(l, env, ev, ev["to"], amount, at)
    else
        reject(env, ev, "unknown event type $kind")
    end
    return nothing
end

function post(l::Handler, env::Env, ev, account, delta, at)
    l.balances[account] = get(l.balances, account, 0) + delta
    txn = bytes2hex(Kavach.random(env, 8))
    emit!(env, "ledger.entries", Json.stringify((txn=txn, event=ev["id"], account=account, delta=delta,
                                                 balance=l.balances[account], at=at)))
end

reject(env::Env, ev, reason) = emit!(env, "ledger.rejections", Json.stringify((event=ev["id"], reason=reason)))

function Kavach.invariants(l::Handler)
    non_negative() = for a in sort!(collect(keys(l.balances)))
        l.balances[a] < 0 && error("account $a has balance $(l.balances[a])")
    end
    conserved() = (s = sum(values(l.balances); init=0)) == l.net ||
                  error("balances sum to $s, deposits minus withdrawals is $(l.net)")
    return Pair{String,Function}["balances_non_negative" => non_negative, "money_conserved" => conserved]
end

Kavach.snapshot(l::Handler) = Vector{UInt8}(codeunits(Json.stringify((balances=l.balances, net=l.net))))
function Kavach.restore!(l::Handler, data::Vector{UInt8})
    state = Json.parse(String(copy(data)))
    l.balances = Dict{String,Int64}(k => v for (k, v) in state["balances"])
    l.net = state["net"]
    return nothing
end

function flagvalue(args, name, default)
    i = findlast(==(name), args)
    return i === nothing || i == length(args) ? default : args[i+1]
end

function main(args)
    fix = "--fix" in args
    # The host command has `kavach-host` appended; it must be able to see --fix.
    Kavach.maybe_host(args, () -> Handler(fix))
    path = flagvalue(args, "--in", joinpath(@__DIR__, "events.jsonl"))
    fixtures = flagvalue(args, "--fixtures", "fixtures")

    rec = Recorder(Handler(fix); service="ledger", dir=fixtures, deliver=outs -> begin
        for o in outs
            println(rpad(o.sink, 18), " ", String(copy(o.data)))
        end
    end)
    try
        for (n, line) in enumerate(eachline(path))
            isempty(strip(line)) && continue
            result = step!(rec, Input("file:" * basename(path), string(n), Vector{UInt8}(codeunits(line))))
            if result.kind == "panic"
                # Stop the service, as the Go demo does; the recorder has
                # already marked the failure and cut a fixture.
                close(rec)
                println(stderr, "ledger: line $n: panic: ", result.message)
                return 1
            elseif !result.ok
                println(stderr, "ledger: line $n: $(result.kind): $(result.message)")
            end
        end
    finally
        close(rec)
    end
    return 0
end

end

abspath(PROGRAM_FILE) == (@__FILE__) && exit(Ledger.main(ARGS))
