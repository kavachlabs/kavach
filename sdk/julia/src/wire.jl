# Encoding of the record stream (SPEC §10.2) and its record payloads (§4).
module Wire

const OPEN, RECORD, STEP_END, FACTS, SNAPSHOT, FLUSH, CLOSE = (0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07)
const INPUT, CLOCK, RAND, OUTPUT, MARKER, GATEWAY, CONFIG = (0x01, 0x02, 0x03, 0x04, 0x05, 0x07, 0x09)

# A growable byte buffer that the hot path encodes into without allocating.
mutable struct Buf
    data::Vector{UInt8}
    n::Int
end
Buf() = Buf(Vector{UInt8}(undef, 4096), 0)

@inline function reserve!(b::Buf, extra::Int)
    b.n + extra > length(b.data) && resize!(b.data, max(2 * length(b.data), b.n + extra))
    return nothing
end

@inline function put!(b::Buf, x::UInt8)
    reserve!(b, 1)
    @inbounds b.data[b.n+1] = x
    b.n += 1
    return nothing
end

function putvarint!(b::Buf, n::Integer)
    reserve!(b, 10)
    while n >= 0x80
        @inbounds b.data[b.n+1] = UInt8((n & 0x7f) | 0x80)
        b.n += 1
        n >>= 7
    end
    put!(b, UInt8(n))
end

function putraw!(b::Buf, p::Ptr{UInt8}, len::Int)
    reserve!(b, len)
    unsafe_copyto!(pointer(b.data, b.n + 1), p, len)
    b.n += len
    return nothing
end

putbytes!(b::Buf, v::Vector{UInt8}) = (putvarint!(b, length(v)); GC.@preserve v putraw!(b, pointer(v), length(v)))
putstring!(b::Buf, s::String) = (putvarint!(b, sizeof(s)); GC.@preserve s putraw!(b, pointer(s), sizeof(s)))
putstring!(b::Buf, s::AbstractString) = putstring!(b, String(s))
putbytes!(b::Buf, v::AbstractVector{UInt8}) = putbytes!(b, Vector{UInt8}(v))
putraw!(b::Buf, v::Vector{UInt8}) = (GC.@preserve v putraw!(b, pointer(v), length(v)))

# A frame is encoded in place: one byte is reserved for `len` (enough below
# 128 bytes) and the payload is shifted if it turns out longer.
function begin_frame!(b::Buf, kind::UInt8)
    put!(b, 0x00)
    put!(b, kind)
    return b.n - 1
end

function end_frame!(b::Buf, start::Int)
    len = b.n - start
    if len < 0x80
        @inbounds b.data[start] = UInt8(len)
    else
        extra = (len >= 1 << 28 ? 5 : len >= 1 << 21 ? 4 : len >= 1 << 14 ? 3 : 2) - 1
        reserve!(b, extra)
        copyto!(b.data, start + 1 + extra, b.data, start + 1, len)
        b.n += extra
        n, i = len, start
        while n >= 0x80
            @inbounds b.data[i] = UInt8((n & 0x7f) | 0x80)
            n >>= 7
            i += 1
        end
        @inbounds b.data[i] = UInt8(n)
    end
    return nothing
end

function put_input!(b::Buf, source, position, data)
    st = begin_frame!(b, RECORD)
    put!(b, INPUT); put!(b, 0x00)
    putstring!(b, source); putstring!(b, position); putbytes!(b, data)
    end_frame!(b, st)
end

function put_clock!(b::Buf, ns::Integer)
    st = begin_frame!(b, RECORD)
    put!(b, CLOCK); put!(b, 0x00)
    x = htol(Int64(ns))
    reserve!(b, 8)
    unsafe_store!(Ptr{Int64}(pointer(b.data, b.n + 1)), x)
    b.n += 8
    end_frame!(b, st)
end

function put_rand!(b::Buf, data)
    st = begin_frame!(b, RECORD)
    put!(b, RAND); put!(b, 0x00)
    putbytes!(b, data)
    end_frame!(b, st)
end

function put_output!(b::Buf, sink, data, islocal::Bool)
    st = begin_frame!(b, RECORD)
    put!(b, OUTPUT); put!(b, 0x00)
    putstring!(b, sink); putbytes!(b, data); put!(b, islocal ? 0x01 : 0x00)
    end_frame!(b, st)
end

put_step_end!(b::Buf) = (st = begin_frame!(b, STEP_END); end_frame!(b, st))

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
