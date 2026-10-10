# The ledger of ../sdk/main.go in Julia, integrated with Kavach without an SDK, as
# INTEGRATING.md describes: it writes the recorder protocol's frames itself
# when live, and answers the host protocol itself in replay. Standard library
# only; it loads nothing from Kavach.
#
#   julia ledger.jl --in ../testdata/events.jsonl        # live, recorded
#   julia ledger.jl --fix --in ../testdata/events.jsonl  # the fixed build
#   kavach replay <fixture> --bin "julia ledger.jl"      # replay, as a host
#
# The build is picked by --fix and not by an environment variable, because
# replay serves environment variables from the journal.
module LedgerProtocol

using Base64: base64decode, base64encode
using Dates: Dates
using Random: RandomDevice

include("json.jl")
include("handler.jl")
include("recorder.jl")
include("host.jl")

function flagvalue(args, name, default)
    i = findlast(==(name), args)
    return i === nothing || i == length(args) ? default : args[i+1]
end

function main(args)
    fix = "--fix" in args
    # Started with kavach-host last, the program is a host and nothing else
    # (§9.1). The host command keeps its own arguments, so --fix reaches it.
    !isempty(args) && last(args) == "kavach-host" && return serve_host(fix)

    path = flagvalue(args, "--in", "")
    isempty(path) && (println(stderr, "ledger: pass --in FILE"); return 2)
    rec = Recorder(Ledger(fix), flagvalue(args, "--fixtures", "kavach"))
    deliver(outs) = for (sink, data) in outs
        println(rpad(sink, 18), " ", String(copy(data)))
    end
    try
        for (n, line) in enumerate(eachline(path))
            isempty(line) && continue
            kind, msg = step!(rec, "file:" * basename(path), string(n), Vector{UInt8}(codeunits(line)), deliver)
            # A panic stops the service; the recorder has the fixture by now.
            # Errors and broken invariants are logged and the event skipped.
            kind == "panic" && (println(stderr, "ledger: line $n: panic: $msg"); return 1)
            kind == "ok" || println(stderr, "ledger: line $n: $kind: $msg")
        end
    finally
        close!(rec)
    end
    return 0
end

end

abspath(PROGRAM_FILE) == (@__FILE__) && exit(LedgerProtocol.main(ARGS))
