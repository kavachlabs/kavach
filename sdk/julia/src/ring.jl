# The shared-memory ring of SPEC §10.7, SDK side. The file is passed by name
# (`ring_path`): Julia's `Cmd` cannot hand a child an extra descriptor.
using Mmap: Mmap

const RING_MAGIC = b"KVRING01"
const RING_HEADER = 256
const RING_MIN = 64 << 10
const RING_DEFAULT = 8 << 20

mutable struct Ring
    mem::Vector{UInt8}
    path::String
    cap::UInt64
    belled::Bool    # the doorbell rang since the ring was last under half full
end

function Ring(dir::AbstractString, capacity::Integer)
    (capacity >= RING_MIN && ispow2(capacity)) || throw(ArgumentError("ring capacity must be a power of two of at least 64 KiB"))
    path, io = mktemp(dir)  # owner-only (0600)
    try
        mem = try
            truncate(io, RING_HEADER + capacity)
            Mmap.mmap(io, Vector{UInt8}, RING_HEADER + capacity, 0; shared=true)
        finally
            close(io)
        end
        copyto!(mem, 1, RING_MAGIC, 1, 8)
        unsafe_store!(Ptr{UInt64}(pointer(mem) + 8), htol(UInt64(capacity)))
        return Ring(mem, path, UInt64(capacity), false)
    catch
        rm(path; force=true)
        rethrow()
    end
end

ring_dir() = isdir("/dev/shm") ? "/dev/shm" : tempdir()

ring_word(g::Ring, off) = Ptr{UInt64}(pointer(g.mem) + off)

"""Copies the front of `p[from:last]` into the ring and publishes it with one
release store, so the recorder sees all of it or none. Publishes nothing if it
does not fit, unless it is larger than the whole ring. Returns the bytes
published and the bytes the recorder has not consumed yet."""
function try_publish(g::Ring, p::Vector{UInt8}, from::Int, last::Int)
    w = ltoh(unsafe_load(ring_word(g, 64), :acquire))
    used = w - ltoh(unsafe_load(ring_word(g, 128), :acquire))
    n = last - from + 1
    free = g.cap - used
    if n > free
        n <= g.cap && return 0, used
        n = Int(free)
    end
    off = w & (g.cap - 1)
    first = min(UInt64(n), g.cap - off)
    GC.@preserve p g begin
        base = pointer(g.mem) + RING_HEADER
        unsafe_copyto!(base + off, pointer(p, from), first)
        n > first && unsafe_copyto!(base, pointer(p, from + Int(first)), n - first)
    end
    unsafe_store!(ring_word(g, 64), htol(w + n), :release)
    return n, used + n
end
