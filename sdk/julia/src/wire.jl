# Encoding of the record stream (SPEC §10.2) and its record payloads (§4).
module Wire

const OPEN, RECORD, STEP_END, FACTS, SNAPSHOT, FLUSH, CLOSE = (0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07)
const INPUT, CLOCK, RAND, OUTPUT, MARKER, GATEWAY, CONFIG = (0x01, 0x02, 0x03, 0x04, 0x05, 0x07, 0x09)

function uvarint!(io::IO, n::Integer)
    n >= 0 || throw(ArgumentError("uvarint of a negative number"))
    while n >= 0x80
        write(io, UInt8((n & 0x7f) | 0x80))
        n >>= 7
    end
    write(io, UInt8(n))
end

bytes!(io::IO, b::AbstractVector{UInt8}) = (uvarint!(io, length(b)); write(io, b))
string!(io::IO, s::AbstractString) = bytes!(io, codeunits(s))

function frame(kind::UInt8, fill!::Function = io -> nothing)
    body = IOBuffer()
    fill!(body)
    payload = take!(body)
    out = IOBuffer()
    uvarint!(out, 1 + length(payload))
    write(out, kind)
    write(out, payload)
    return take!(out)
end

record(rtype::UInt8, critical::Bool, fill!::Function) =
    frame(RECORD, io -> (write(io, rtype); write(io, critical ? 0x01 : 0x00); fill!(io)))

input_record(source, position, data) =
    record(INPUT, false, io -> (string!(io, source); string!(io, position); bytes!(io, data)))
clock_record(ns::Integer) = record(CLOCK, false, io -> write(io, htol(Int64(ns))))
rand_record(data) = record(RAND, false, io -> bytes!(io, data))
output_record(sink, data, islocal::Bool) =
    record(OUTPUT, false, io -> (string!(io, sink); bytes!(io, data); write(io, islocal ? 0x01 : 0x00)))
marker_record(kind, message, data) =
    record(MARKER, false, io -> (string!(io, kind); string!(io, message); bytes!(io, data)))
gateway_record(gateway, request, response, error, islocal::Bool) =
    record(GATEWAY, true, io -> (string!(io, gateway); bytes!(io, request); bytes!(io, response); string!(io, error);
                                  write(io, islocal ? 0x01 : 0x00)))
config_record(key, value::Union{Vector{UInt8},Nothing}, source) =
    record(CONFIG, true, io -> (string!(io, key); write(io, value === nothing ? 0x00 : 0x01);
                                 bytes!(io, value === nothing ? UInt8[] : value); string!(io, source)))

step_end_frame() = frame(STEP_END)
snapshot_frame(data) = frame(SNAPSHOT, io -> bytes!(io, data))
flush_frame(durable::Bool) = frame(FLUSH, io -> write(io, durable ? 0x01 : 0x00))
close_frame() = frame(CLOSE)

function facts_frame(facts)
    frame(FACTS, io -> begin
        uvarint!(io, length(facts))
        for (k, v) in facts
            string!(io, k)
            write(io, 0x00)
            bytes!(io, v)
        end
    end)
end

end
