# The shared-memory ring of SPEC §10.7, SDK side. The file is passed by name
# (`ring_path`): Julia's `Cmd` cannot hand a child an extra descriptor.

const RING_MAGIC = b"KVRING02"
const RING_HEADER = 65536
const RING_MIN = 64 << 10
const RING_DEFAULT = 8 << 20

# libc mmap constants; only MAP_ANON differs between the BSDs (and macOS) and Linux.
const PROT_NONE, PROT_RW = Cint(0), Cint(3)
const MAP_SHARED, MAP_PRIVATE, MAP_FIXED = Cint(0x1), Cint(0x2), Cint(0x10)
const MAP_ANON = Sys.islinux() ? Cint(0x20) : Cint(0x1000)

mmap_at(addr, len, prot, flags, fd, off) =
    ccall(:mmap, Ptr{Cvoid}, (Ptr{Cvoid}, Csize_t, Cint, Cint, Cint, Int64), addr, len, prot, flags, fd, off)

mutable struct Ring
    base::Ptr{UInt8}    # the file's header and data area, then the data area again
    path::String
    cap::UInt64
    belled::Bool    # the doorbell rang since the ring was last under half full
end

# Maps the data area twice back to back (SPEC §10.7), so a run that wraps is one copy.
function map_mirrored(fd::Cint, capacity::Integer)
    failed = Ptr{Cvoid}(-1)
    total = RING_HEADER + 2capacity
    base = mmap_at(C_NULL, total, PROT_NONE, MAP_PRIVATE | MAP_ANON, Cint(-1), 0)
    base == failed && systemerror("mmap")
    first = mmap_at(base, RING_HEADER + capacity, PROT_RW, MAP_SHARED | MAP_FIXED, fd, 0)
    second = first == failed ? failed : mmap_at(base + RING_HEADER + capacity, capacity, PROT_RW, MAP_SHARED | MAP_FIXED, fd, RING_HEADER)
    if second == failed
        err = Libc.errno()
        ccall(:munmap, Cint, (Ptr{Cvoid}, Csize_t), base, total)
        systemerror("mmap", err)
    end
    return Ptr{UInt8}(base)
end

function Ring(dir::AbstractString, capacity::Integer)
    (capacity >= RING_MIN && ispow2(capacity)) || throw(ArgumentError("ring capacity must be a power of two of at least 64 KiB"))
    path, io = mktemp(dir)  # owner-only (0600)
    try
        base = try
            truncate(io, RING_HEADER + capacity)
            map_mirrored(Base.cconvert(Cint, Base.fd(io)), capacity)
        finally
            close(io)
        end
        for (i, b) in enumerate(RING_MAGIC)
            unsafe_store!(base, b, i)
        end
        unsafe_store!(Ptr{UInt64}(base + 8), htol(UInt64(capacity)))
        g = Ring(base, path, UInt64(capacity), false)
        finalizer(g) do g
            ccall(:munmap, Cint, (Ptr{Cvoid}, Csize_t), g.base, RING_HEADER + 2g.cap)
        end
        return g
    catch
        rm(path; force=true)
        rethrow()
    end
end

ring_dir() = isdir("/dev/shm") ? "/dev/shm" : tempdir()

ring_word(g::Ring, off) = Ptr{UInt64}(g.base + off)

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
    GC.@preserve p unsafe_copyto!(g.base + RING_HEADER + (w & (g.cap - 1)), pointer(p, from), n)
    unsafe_store!(ring_word(g, 64), htol(w + n), :release)
    return n, used + n
end
