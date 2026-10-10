# The ledger handler: the same single-writer wallet ledger as ../sdk/ledger.go. It
# touches the world only through three functions of its `world`, which the
# recorder implements live and the host in replay:
#
#   now_ns(world)            the clock, Unix nanoseconds
#   randbytes(world, n)      n random bytes
#   emit!(world, sink, data) an effect, delivered only if the step succeeds

mutable struct Ledger
    fix::Bool # the planted bug's switch: false is the buggy build
    balances::Dict{String,Int64}
    net::Int64 # deposits minus withdrawals
end
Ledger(fix::Bool) = Ledger(fix, Dict{String,Int64}(), 0)

# HandlerError fails a step as an `error`; any other exception is a `panic`.
struct HandlerError <: Exception
    msg::String
end
Base.showerror(io::IO, e::HandlerError) = print(io, e.msg)

function handle!(l::Ledger, world, position::String, data::Vector{UInt8})
    ev = try
        parsejson(String(copy(data)))
    catch e
        throw(HandlerError("decode event at $position: " * sprint(showerror, e)))
    end
    amount = get(ev, "amount", nothing)
    l.fix && amount === nothing && return reject(world, ev, "missing amount")
    # The planted bug: a null amount throws a MethodError here.
    amount <= 0 && return reject(world, ev, "amount must be positive")
    at = rfc3339nano(now_ns(world))
    account = get(ev, "account", "")

    kind = get(ev, "type", "")
    if kind == "deposit"
        l.net += amount
        post(l, world, ev, account, amount, at)
    elseif kind == "withdraw"
        get(l.balances, account, 0) < amount && return reject(world, ev, "insufficient funds")
        l.net -= amount
        post(l, world, ev, account, -amount, at)
    elseif kind == "transfer"
        get(l.balances, account, 0) < amount && return reject(world, ev, "insufficient funds")
        post(l, world, ev, account, -amount, at)
        post(l, world, ev, get(ev, "to", ""), amount, at)
    else
        reject(world, ev, "unknown event type $kind")
    end
    return nothing
end

function post(l::Ledger, world, ev, account, delta, at)
    l.balances[account] = get(l.balances, account, 0) + delta
    txn = bytes2hex(randbytes(world, 8))
    emit!(world, "ledger.entries", tojson((txn=txn, event=get(ev, "id", ""), account=account, delta=delta,
                                           balance=l.balances[account], at=at)))
end

reject(world, ev, reason) = emit!(world, "ledger.rejections", tojson((event=get(ev, "id", ""), reason=reason)))

# Invariants, in check order. Each returns nothing when it holds, or why not.
const INVARIANTS = [
    "balances_non_negative" => function (l::Ledger)
        for a in sort!(collect(keys(l.balances)))
            l.balances[a] < 0 && return "account $a has balance $(l.balances[a])"
        end
    end,
    "money_conserved" => function (l::Ledger)
        s = sum(values(l.balances); init=0)
        s == l.net ? nothing : "balances sum to $s, deposits minus withdrawals is $(l.net)"
    end,
]

# check_invariants returns the name and failure of the first invariant that
# does not hold, or nothing.
function check_invariants(l::Ledger)
    for (name, check) in INVARIANTS
        why = check(l)
        why === nothing || return name => why
    end
    return nothing
end

# failure maps an exception to a marker's kind, message and data (SPEC.md §4.5),
# the same way live and in replay. A panic's message is the first line of the
# error with file:line and addresses removed, so it is the same in production
# and in any checkout; the full text and stack go in data.
function failure(e, bt)
    e isa HandlerError && return ("error", e.msg, "")
    text = sprint(showerror, e)
    msg = replace(first(split(text, '\n')), r"\s*@\s*\S*\.jl:\d+" => "", r"\S*\.jl:\d+" => "", r"0x[0-9a-fA-F]{6,}" => "0x?")
    return ("panic", msg, text * "\n" * sprint(Base.show_backtrace, bt))
end

function rfc3339nano(ns::Integer)
    dt = Dates.unix2datetime(ns ÷ 1_000_000_000)
    frac = rstrip(lpad(string(ns % 1_000_000_000), 9, '0'), '0')
    return Dates.format(dt, "yyyy-mm-ddTHH:MM:SS") * (isempty(frac) ? "" : "." * frac) * "Z"
end
