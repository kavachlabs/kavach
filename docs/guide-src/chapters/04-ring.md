# The Shared-Memory Ring
> internal/recstream/ring*.go, sdk/go/recorder.go — carrying the record stream for tens of nanoseconds a step

## Why a ring

Chapter 3's recorder reads frames; this chapter is about how they get to it. The obvious transport is the recorder's standard input, a pipe, and every SDK supports it. Its cost is the system call: each `write` into a pipe enters the kernel, copies the bytes into a kernel buffer, and may wake the reader. A step publishes twice (its `input` frame before the handler runs, then the rest of the step with its `step_end`), so a step over the pipe costs two system calls. On an Apple M3 Pro that is about 1.5 µs per step at the median.

A **shared-memory ring** removes the kernel from the path. The SDK and the recorder map the same memory. The SDK copies a frame into it and advances a counter; the recorder, polling from another core, sees the counter move and reads the bytes. Publishing a step is a memory copy and one atomic store, about 75 ns per event (four events per step in the benchmark). The frames are exactly those of §10.2; only the transport changes.

## Code map

@graph files internal/recstream/ | Figure 4.1: Files of internal/recstream. ring.go is shared; ring_unix.go, ring_linux*.go and ring_nolinux.go pick how the file is made and mapped.
@graph calls recstream_ringreader_read 1 | Figure 4.2: RingReader.Read, the consumer side, and its callers.
@graphify explain recstream_ring_trypublish

## The specification

@spec 10.7

## The layout

The ring is one file of `65536 + capacity` bytes. The first 64 KiB are the **header**, of which only four `u64` words are used. The rest is the **data area**, where the record stream is written round and round.

<figure class="diagram">
<svg viewBox="0 0 640 150" width="640" xmlns="http://www.w3.org/2000/svg" font-family="Helvetica, Arial, sans-serif" font-size="11" fill="#222">
<g font-size="10" text-anchor="middle">
<text x="20" y="30">0</text><text x="90" y="30">8</text><text x="160" y="30">16</text><text x="210" y="30">64</text><text x="300" y="30">128</text><text x="430" y="30">65536</text><text x="622" y="30" text-anchor="end">65536 + cap</text>
</g>
<g stroke="#555" fill="none">
<line x1="20" y1="34" x2="20" y2="40"/><line x1="90" y1="34" x2="90" y2="40"/><line x1="160" y1="34" x2="160" y2="40"/><line x1="210" y1="34" x2="210" y2="40"/><line x1="300" y1="34" x2="300" y2="40"/><line x1="430" y1="34" x2="430" y2="40"/><line x1="620" y1="34" x2="620" y2="40"/>
</g>
<rect x="20" y="40" width="70" height="40" fill="#dbe8f7" stroke="#555"/>
<rect x="90" y="40" width="70" height="40" fill="#dbe8f7" stroke="#555"/>
<rect x="160" y="40" width="50" height="40" fill="#f2f2f2" stroke="#555"/>
<rect x="210" y="40" width="50" height="40" fill="#fde2c4" stroke="#555"/>
<rect x="260" y="40" width="40" height="40" fill="#f2f2f2" stroke="#555"/>
<rect x="300" y="40" width="50" height="40" fill="#d6efd6" stroke="#555"/>
<rect x="350" y="40" width="80" height="40" fill="#f2f2f2" stroke="#555"/>
<rect x="430" y="40" width="190" height="40" fill="#fff8d6" stroke="#555"/>
<g text-anchor="middle">
<text x="55" y="58">magic</text><text x="55" y="72" font-size="9">KVRING02</text>
<text x="125" y="58">capacity</text><text x="125" y="72" font-size="9">u64</text>
<text x="185" y="64" font-size="9">0</text>
<text x="235" y="58">write</text><text x="235" y="72" font-size="9">u64</text>
<text x="280" y="64" font-size="9">0</text>
<text x="325" y="58">read</text><text x="325" y="72" font-size="9">u64</text>
<text x="390" y="64" font-size="9">unused</text>
<text x="525" y="58">data area</text><text x="525" y="72" font-size="9">byte i of the stream at i mod cap</text>
</g>
<g text-anchor="middle" font-size="10">
<text x="90" y="100">written once by the SDK</text>
<text x="235" y="100">SDK only</text>
<text x="325" y="100">recorder only</text>
<text x="525" y="100">SDK writes, recorder reads</text>
</g>
<text x="20" y="130" font-size="10">Not to scale. write sits alone on the cache line 64–127, read on 128–191; the header pads the data area to 64 KiB.</text>
</svg>
</figure>
<p class="caption">The ring file. The header's four words, then the data area at offset 65536.</p>

`write` and `read` are byte counts since the stream began, not offsets. They only grow. Each lives on a 64-byte **cache line** of its own, because the two processes write them from different cores: if they shared a line, every store by one would invalidate the other's cached copy of both words (*false sharing*), and the line would bounce between cores on every step.

The data area starts at 65536, not at 192, so that it begins on a page boundary for every page size in use (4 KiB on x86, 16 KiB on Apple silicon, 64 KiB on some arm64 Linux kernels). That alignment is what lets it be mapped twice, below. Moving the header from 192 bytes to 64 KiB is also why the magic changed to `KVRING02` in PR #5: an old SDK and a new recorder must not agree on a layout they do not share.

@code internal/recstream/ring.go 13-46
@code internal/recstream/ring.go func:initHeader
@code internal/recstream/ring.go func:checkHeader

`newRing` turns the two header words into `*atomic.Uint64` by pointing into the mapped memory. That is safe because the words are 8-byte aligned (at offsets 64 and 128 of a page-aligned mapping) and the memory is never moved by Go's garbage collector: it was not allocated by Go.

### Counters and masking

Two rules make the arithmetic trivial:

- **The capacity is a power of two.** The position of stream byte `i` in the data area is `i mod capacity`, which is `i & (capacity − 1)`: one AND instead of a division.
- **The counters are 64 bits and never reset.** `write − read` is the number of bytes in the ring, between 0 and `capacity`; `capacity − (write − read)` is the free space. There is no "full versus empty" ambiguity, as there is with two wrapped offsets, because `write == read` can only mean empty. At 10 GB/s a `u64` byte counter takes 58 years to wrap, and even then the unsigned subtraction and the mask stay correct, because 2⁶⁴ is a multiple of every power-of-two capacity.

## Mapping the data area twice

A frame can start near the end of the data area and wrap to its start. With a single mapping, writing it takes two copies (up to the end of the area, then the rest at its start), and so does reading it, and the frame decoder cannot hand out a slice of the ring without first assembling it. The trick of PR #5 is to map the data area a second time, immediately after the first. In virtual memory the data area then appears twice, back to back, and both copies are the same physical pages. A run of up to `capacity` bytes starting at any offset is contiguous in virtual memory, so it is always one copy.

<figure class="diagram">
<svg viewBox="0 0 640 230" width="640" xmlns="http://www.w3.org/2000/svg" font-family="Helvetica, Arial, sans-serif" font-size="11" fill="#222">
<text x="20" y="22" font-size="10">virtual memory: one reservation of 65536 + 2 × cap bytes</text>
<rect x="20" y="30" width="100" height="40" fill="#dbe8f7" stroke="#555"/>
<rect x="120" y="30" width="250" height="40" fill="#fff8d6" stroke="#555"/>
<rect x="370" y="30" width="250" height="40" fill="#fff8d6" stroke="#555" stroke-dasharray="4 3"/>
<rect x="330" y="30" width="80" height="40" fill="#f39c4a" fill-opacity="0.75" stroke="#b35a00"/>
<g text-anchor="middle">
<text x="70" y="54">header</text>
<text x="225" y="54">data</text>
<text x="515" y="54">data again (mirror)</text>
<text x="370" y="54" fill="#fff" font-weight="bold">frame</text>
</g>
<g font-size="9" text-anchor="middle">
<text x="20" y="84" text-anchor="start">base</text><text x="120" y="84">d = base + 65536</text><text x="370" y="84">d + cap</text><text x="622" y="84" text-anchor="end">d + 2 cap</text>
</g>
<g stroke="#777" stroke-width="1" fill="none">
<line x1="120" y1="88" x2="120" y2="160"/><line x1="370" y1="88" x2="370" y2="160"/>
<line x1="370" y1="88" x2="120" y2="158" stroke-dasharray="4 3"/><line x1="620" y1="88" x2="370" y2="158" stroke-dasharray="4 3"/>
</g>
<text x="20" y="152" font-size="10">the file</text>
<rect x="20" y="160" width="100" height="40" fill="#dbe8f7" stroke="#555"/>
<rect x="120" y="160" width="250" height="40" fill="#fff8d6" stroke="#555"/>
<rect x="330" y="160" width="40" height="40" fill="#f39c4a" fill-opacity="0.75" stroke="#b35a00"/>
<rect x="120" y="160" width="40" height="40" fill="#f39c4a" fill-opacity="0.75" stroke="#b35a00"/>
<g text-anchor="middle">
<text x="70" y="184">header</text>
<text x="245" y="184">data (cap bytes)</text>
<text x="140" y="214" font-size="9">frame's last bytes</text>
<text x="350" y="214" font-size="9">frame's first bytes</text>
</g>
<text x="395" y="176" font-size="10">first mapping: file offset 0, 65536 + cap bytes</text>
<text x="395" y="191" font-size="10">second mapping: file offset 65536, cap bytes</text>
</svg>
</figure>
<p class="caption">The same file pages appear twice in virtual memory. A frame that starts at d + off, near the end of the data area, runs into the mirror; one copy of n bytes to d + off puts the frame's first bytes at the end of the file's data area and its last bytes at the start.</p>

`TestRingMirrorWrap`, at the end of this chapter, checks exactly this picture.

### mapRing

@code internal/recstream/ring_unix.go func:mapRing

Read it in three moves:

1. **Reserve.** An anonymous, private, `PROT_NONE` mapping of `65536 + 2 × capacity` bytes. It allocates no memory and cannot be touched; its only purpose is to claim a stretch of address space, so that nothing else (another thread's `mmap`, the Go runtime's heap) can land in the middle of what follows.
2. **Map the file over the start of it.** `MAP_SHARED | MAP_FIXED` at `base`, from file offset 0, for `65536 + capacity` bytes: header and data. `MAP_FIXED` replaces the reserved pages in place.
3. **Map the data area again right after it.** At `base + 65536 + capacity`, from file offset 65536, for `capacity` bytes. The file offset must be a multiple of the page size, which is why the header is 64 KiB.

`Ring.Close` unmaps the whole reservation with one `munmap`, both mappings included. `g.data` is `mem[65536:]`, `2 × capacity` bytes long, so `copy(g.data[off:], p)` is the one-copy publish, for any `off < capacity` and `len(p) ≤ capacity`.

### Creating the file

@code internal/recstream/ring_unix.go func:CreateRing
@code internal/recstream/ring_linux.go all
@code internal/recstream/ring_unix.go func:createTempRingFile

On Linux the file is a **memfd**: `memfd_create` returns a descriptor for anonymous memory that behaves like a file but has no name in any file system. Go's `syscall` package has no wrapper for it, so the call goes through `Syscall` with the system call number from `ring_linux_amd64.go` (319) or `ring_linux_arm64.go` (279); other Linux architectures and kernels without it fall back to the temporary file. Elsewhere (macOS, the BSDs) the file is created in `/dev/shm` if it exists, else the temporary directory, and unlinked immediately. Either way, once the SDK passes the descriptor to the recorder, nothing can delete the ring from under them, and nothing is left behind when both exit.

The SDK passes the descriptor as fd 3 through `exec.Cmd.ExtraFiles`, then closes its own copy of the descriptor: the mapping keeps the memory alive.

### ring_path, for runtimes that cannot pass fd 3

Not every runtime can hand a child an extra descriptor. Java's `ProcessBuilder`, Haskell's `process` library and Julia's `Cmd` cannot. Those SDKs keep the file's name (the file is created readable and writable by its owner only), send it as `ring_path` in the `open` frame, and delete it themselves if the recorder never starts. The recorder opens it, unlinks it at once, and maps it:

@code internal/recorder/recorder.go func:mapRing

## Publishing and consuming

The protocol is **single-producer, single-consumer**: exactly one thread publishes (the SDK, holding its `Recorder`'s mutex) and exactly one reads (the recorder's reader goroutine). Each side writes one counter and only reads the other. No locks are needed; ordering is enough.

- The SDK copies the bytes, *then* stores `write` with **release** ordering. Release means that every memory write before the store is visible to any thread that sees the new value. So the recorder can never see `write` cover bytes that are not there yet. A service that dies mid-copy leaves `write` where it was, and the half-copied frame does not exist.
- The recorder loads `write` with **acquire** ordering (the matching half), copies the bytes out, *then* stores `read` with release ordering. The SDK loads `read` with acquire before computing free space, so it never overwrites bytes the recorder is still copying.

Go's `sync/atomic` operations are sequentially consistent, which is stronger than release and acquire and costs nothing extra for these plain loads and stores on x86 and arm64. The C, Rust and Java SDKs use explicit release and acquire.

@code internal/recstream/ring.go func:TryPublish

`TryPublish` is all-or-nothing for any frame that fits in the ring: if there is not room for all of `p`, it publishes nothing. The recorder therefore never sees part of a frame, except for a frame larger than the whole ring (a big snapshot), which can only go through in pieces. The second result, `used`, tells the caller how full the ring is, which drives the doorbell.

On the other side, `RingReader` makes the ring look like an `io.Reader`, so Chapter 3's frame decoder works on it unchanged. `Read` copies as much as is available, up to the size of the buffer it is given, in one copy through the mirror, and advances `read` immediately: the bytes are now in the recorder's own memory.

@code internal/recstream/ring.go type:RingReader
@code internal/recstream/ring.go func:Read
@code internal/recstream/ring.go func:Bell
@code internal/recstream/ring.go func:End

`Read` also enforces the last sentence of the spec's publishing rules: `write` moving backwards, or running more than `capacity` ahead of `read`, means the header was corrupted (or the SDK is broken), and the recorder stops with a fatal error.

## The SDK side

In the Go SDK, the `Encoder` of Chapter 2 writes into a `pipe` value whose `Write` calls `publish`. `publish` is the only function that knows which transport is in use:

@code sdk/go/recorder.go func:publish
@code sdk/go/recorder.go func:bell
@code sdk/go/recorder.go func:newRing

A detail of `start` (shown whole in Chapter 2): `r.ring` is assigned only *after* the `open` frame is encoded. So the `open` frame goes through `publish` while `r.ring` is still nil, and is written to standard input, as §10.7 requires. Every frame after it goes into the ring.

@code sdk/go/recorder.go 316-324

## Waking the recorder

The recorder does not spin on the ring. `RingReader.Read` waits on two things: the doorbell channel, and a 5 ms timer (`ringPollTime`; the specification allows up to 10 ms). So a frame the SDK publishes is seen within 5 ms even if nobody rings.

The **doorbell** is standard input. After `open`, every byte the SDK writes to it means "look at the ring now". `ringBell` in the recorder (Chapter 3) reads it in 64-byte chunks and calls `Bell` once per read, so a burst of rings costs one wake-up. The SDK rings in exactly three cases:

- after a `flush` or `close` frame, because the SDK is about to wait for the answer and 5 ms would be wasted;
- when it finds the ring more than half full, once per crossing (`belled` is cleared when it drops back to half or below), so the recorder starts draining before the ring fills;
- when the ring is full.

An ordinary step does not ring. That is the point: the common path makes no system call, and the 5 ms poll costs nothing that matters, because the recorder only puts records on disk once a block fills or the flush interval (1 s) passes anyway.

## Ending and draining

The end of standard input still means the service has ended (§10.5). But the mapping outlives the service: the recorder holds its own mapping, so whatever the service published before dying is still there. `ringBell` sees end of input and calls `End`; `Read` then keeps returning data until `write` has been reached, and only then returns `io.EOF`. The recorder's handling of the end (a `crash` marker if a step was open) follows as over the pipe.

The order of the two loads at the top of `Read` matters. `end` is loaded *before* `write`. The service's last publish happens before it dies, which happens before standard input closes, which happens before `End`. So if `Read` sees `ended`, a `write` loaded afterwards includes that last publish. Loading them the other way round could see an old `write`, then `ended`, and return `io.EOF` with the service's last frames still in the ring.

`TestRingEndsInsideStep` in `cmd/kavach-recorder/ring_test.go` checks the whole path: an `input` frame published into the ring, standard input closed, and the recorder writing a `crash` marker and a fixture.

## When the ring is full

§3.6 forbids silently dropping records, and the reference SDKs choose to wait. In `publish`, if `TryPublish` takes nothing, the SDK rings the doorbell once and then retries every 20 µs for as long as recording is active. If the recorder exits, the SDK's control goroutine sees its standard output close and calls `fail`, which clears `active`, and the loop gives up: the frame is dropped and recording has stopped, loudly.

:::trap Common trap
The waiting loop has no time limit, as the `TODO` in `publish` says. A recorder that is alive but not reading (stopped by a debugger, starved of CPU, blocked on a full disk) blocks the service's steps for as long as that lasts. Bounding the wait means choosing between blocking and writing a `dropped` marker (§4.5), and that choice is deferred. If you see a service's steps stall with the recorder still running, this is the first place to look.
:::

### Why the SDK waits for `ready`

An 8 MiB ring holds tens of thousands of steps. A recorder that is slow to start, about 170 ms in the case that was measured, leaves the ring unread for that long, and a service that starts handling events at once fills it. The next step then waits, for 146 to 400 ms, until the recorder catches up. That stall was first blamed on segment rotation, but rotation's close costs about 10 ms and runs off the read loop anyway (Chapter 3). The cause was the cold start. PRs #7 and #8 made every SDK wait for the recorder's `ready`, bounded by 2 s, before its first step:

@code sdk/go/recorder.go 193-208

The wait is in `NewRecorder`, not in a step, so it costs the service's start-up time and never a step's latency. If `ready` does not arrive in 2 s, recording goes on, and the worst case is the old behaviour.

## The ring in the other SDKs

Six SDKs carry the stream over the ring. All use the same layout; they differ in how they pass the file and whether they map it twice.

| SDK | Passes the file as | Wrapped frame |
| --- | --- | --- |
| Go | fd 3 (memfd on Linux) | one copy (mirror) |
| C | fd 3, via `posix_spawn` (memfd on Linux) | one copy (mirror) |
| Rust | fd 3, via `pre_exec` (memfd on Linux) | one copy (mirror) |
| Haskell | `ring_path` | one copy (mirror) |
| Julia | `ring_path` | one copy (mirror) |
| Java 21 | `ring_path` | two copies |

Java cannot map a file at a fixed address without the Foreign Function and Memory API, which became final in Java 22; the SDK targets Java 21, so its `tryPublish` splits a wrapped run into two `put`s. The fake recorder that the SDK conformance suite uses (`spec/recorder/sdk/fake_recorder.py`) reads the ring with Python's `mmap` the same way, in two slices. Both are correct: mapping twice is an optimisation that §10.7 permits (*MAY*), not a requirement. The Python, TypeScript, Ruby, PHP, OCaml and Elixir SDKs use the pipe.

## Performance

| What | Ring | Pipe | Where it comes from |
| --- | --- | --- | --- |
| Throughput, back to back, Apple M3 Pro | 81.2 ns/event (≈ 325 ns/step) | 493.7 ns/event | `BENCHMARKS.md` |
| Throughput, GitHub-hosted 4-vCPU Linux runner | ≈ 109 ns/event (x86), ≈ 140 ns/event (arm64) | — | same benchmark, not yet in `BENCHMARKS.md` |
| Single step, paced, M3 Pro | p50 ≈ 167 ns, p99 250–625 ns | p50 ≈ 1.5 µs, p99 ≈ 8 µs | latency measurement (not checked in) |

The benchmark step journals four events (input, clock read, 8-byte random read, output) and its handler does nothing else, so these are Kavach's costs. `BENCHMARKS.md` explains what the back-to-back number includes: the 8 MiB ring fills within milliseconds, so it is a steady rate, including any wait for the recorder to make room, and it holds only because the recorder processes a step faster than the SDK produces one. Other runs on the same machine have measured 75 to 82 ns/event; Chapter 2 quotes 75. The paced numbers measure one `Step` at a time with gaps between them, so the ring always has room; they are the latency one step adds to a service that is not saturating its recorder. The pipe's p99 of about 8 µs is the kernel: a `write` that has to wake the reader, or is descheduled, pays for it.

:::note Reading the numbers
"ns/event" is the step time divided by four. A step with more reads costs more in encoding, but the publish itself (one copy and one store) costs almost the same for a small step as for a large one, which is why per-step cost on the ring stays far below the pipe's two system calls.
:::

## Tests

`internal/recstream/ring_test.go` tests the ring with no processes, using a 64 KiB ring so that a few hundred frames wrap it many times. The first test publishes 200 rounds of three 1 KiB record frames, about nine trips round the ring, and decodes each frame back. The second checks the mirror directly: it advances both counters to 10 bytes before the end of the data area, publishes 20 bytes with one `TryPublish`, and checks that the last 10 landed at the *start* of the data area through the second mapping, and that the reader gets all 20 back in order.

@code internal/recstream/ring_test.go 25-65

The rest of the file covers a full ring (publishing nothing until there is room), a frame larger than the ring (published in pieces by a goroutine while the reader assembles it), draining after `End`, the fatal header checks, and `OpenRing` rejecting a bad magic or capacity. `cmd/kavach-recorder/ring_test.go` runs the real binary: every fatal ring error, a stream that ends inside a step, and a ring passed by `ring_path`.

:::try Try it
```
go test ./internal/recstream -run Ring -v
go test ./cmd/kavach-recorder -run Ring -v
go test -run '^$' -bench RecorderStep -benchtime 2s -count 5 ./replay
```
:::
