//go:build unix

package ring

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// Ring is a lock-free KVRING02 shared-memory ring buffer matching
// kavach::daemon::ShmRingConsumer byte-for-byte.
//
// Layout:
//   - [0..8):      magic "KVRING02"
//   - [8..16):     capacity uint64 LE (power of two >= 65536)
//   - [64..72):    write cursor uint64 atomic (writer release-stores, consumer acquire-loads)
//   - [128..136):  read cursor uint64 atomic (consumer release-stores, writer acquire-loads)
//   - [65536..):   circular data region of length capacity
type Ring struct {
	mem     []byte
	data    []byte
	cap     uint64
	mask    uint64
	write   *atomic.Uint64
	read    *atomic.Uint64
	dropped atomic.Uint64
	closed  atomic.Bool
	path    string
}

func isValidCapacity(cap uint64) bool {
	return cap >= MinCapacity && (cap&(cap-1)) == 0
}

// Create creates and initializes a new KVRING02 ring file at path with 0600
// permissions and maps it shared into memory.
func Create(path string, capacity uint64) (*Ring, error) {
	if capacity == 0 {
		capacity = DefaultCapacity
	}
	if !isValidCapacity(capacity) {
		return nil, fmt.Errorf("ring: capacity %d must be a power of two >= %d", capacity, MinCapacity)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("ring: mkdir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ring: create %s: %w", path, err)
	}
	defer f.Close()

	total := int64(HeaderBytes) + int64(capacity)
	if err := f.Truncate(total); err != nil {
		return nil, fmt.Errorf("ring: truncate %s: %w", path, err)
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, int(total), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("ring: mmap %s: %w", path, err)
	}
	// Zero the 64 KiB header, write KVRING02 magic and capacity.
	clear(mem[:HeaderBytes])
	copy(mem[OffsetMagic:], Magic)
	binary.LittleEndian.PutUint64(mem[OffsetCapacity:], capacity)
	return newRingFromMem(mem, capacity, path), nil
}

// Open opens an existing KVRING02 shared-memory ring file and validates its header.
func Open(path string) (*Ring, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("ring: open %s: %w", path, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("ring: stat %s: %w", path, err)
	}
	if st.Size() < HeaderBytes+MinCapacity {
		return nil, fmt.Errorf("ring: file %s too small (%d bytes)", path, st.Size())
	}
	hdr, err := syscall.Mmap(int(f.Fd()), 0, HeaderBytes, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("ring: mmap header %s: %w", path, err)
	}
	magic := string(hdr[OffsetMagic : OffsetMagic+len(Magic)])
	capVal := binary.LittleEndian.Uint64(hdr[OffsetCapacity:])
	_ = syscall.Munmap(hdr)

	if magic != Magic {
		return nil, fmt.Errorf("ring: invalid magic %q (want %q)", magic, Magic)
	}
	if !isValidCapacity(capVal) || uint64(st.Size()-HeaderBytes) < capVal {
		return nil, fmt.Errorf("ring: invalid capacity %d for file size %d", capVal, st.Size())
	}
	total := int(HeaderBytes + capVal)
	mem, err := syscall.Mmap(int(f.Fd()), 0, total, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("ring: mmap %s: %w", path, err)
	}
	return newRingFromMem(mem, capVal, path), nil
}

// OpenOrCreate attaches to an existing valid KVRING02 ring at path (e.g.
// pre-created by kavach-daemon or kavach dev), or creates a new one if absent
// or empty.
func OpenOrCreate(path string, capacity uint64) (*Ring, error) {
	if st, err := os.Stat(path); err == nil && st.Size() >= HeaderBytes+MinCapacity {
		if r, err := Open(path); err == nil {
			return r, nil
		}
	}
	return Create(path, capacity)
}

func newRingFromMem(mem []byte, capacity uint64, path string) *Ring {
	return &Ring{
		mem:   mem,
		data:  mem[HeaderBytes : HeaderBytes+capacity],
		cap:   capacity,
		mask:  capacity - 1,
		write: (*atomic.Uint64)(unsafe.Pointer(&mem[OffsetWrite])),
		read:  (*atomic.Uint64)(unsafe.Pointer(&mem[OffsetRead])),
		path:  path,
	}
}

// Capacity returns the size in bytes of the circular data area.
func (g *Ring) Capacity() uint64 { return g.cap }

// Path returns the backing file path.
func (g *Ring) Path() string { return g.path }

// WriteCursor returns the current monotonic write offset.
func (g *Ring) WriteCursor() uint64 {
	if g == nil || g.closed.Load() {
		return 0
	}
	return g.write.Load()
}

// ReadCursor returns the current monotonic read offset.
func (g *Ring) ReadCursor() uint64 {
	if g == nil || g.closed.Load() {
		return 0
	}
	return g.read.Load()
}

// Dropped returns the number of frames dropped due to a full ring.
func (g *Ring) Dropped() uint64 { return g.dropped.Load() }

// TryPublish performs a non-blocking, lock-free drop-on-full publish of p into
// the ring. If free space is insufficient, it publishes nothing, increments
// Dropped(), and returns (false, used). Safe for single-producer/single-consumer
// use; concurrent producers should serialize via Writer.
func (g *Ring) TryPublish(p []byte) (ok bool, used uint64) {
	if len(p) == 0 {
		return true, 0
	}
	if g == nil || g.closed.Load() {
		return false, 0
	}
	w := g.write.Load()
	r := g.read.Load()
	used = w - r
	n := uint64(len(p))
	if used > g.cap || n > g.cap-used {
		g.dropped.Add(1)
		return false, used
	}
	pos := w & g.mask
	first := g.cap - pos
	if first >= n {
		copy(g.data[pos:pos+n], p)
	} else {
		copy(g.data[pos:], p[:first])
		copy(g.data[:n-first], p[first:])
	}
	// Release store publishes all copied bytes atomically to the consumer.
	g.write.Store(w + n)
	return true, used + n
}

// PollOnce drains all currently available unread bytes from the ring and
// advances the read cursor with a release store. Matches ShmRingConsumer::poll_once.
func (g *Ring) PollOnce() ([]byte, error) {
	if g == nil || g.closed.Load() {
		return nil, errors.New("ring: closed")
	}
	w := g.write.Load()
	r := g.read.Load()
	if w < r {
		return nil, errors.New("ring: write cursor moved backwards")
	}
	avail := w - r
	if avail > g.cap {
		return nil, fmt.Errorf("ring: unread bytes %d exceed capacity %d", avail, g.cap)
	}
	if avail == 0 {
		return nil, nil
	}
	out := make([]byte, avail)
	pos := r & g.mask
	first := g.cap - pos
	if first >= avail {
		copy(out, g.data[pos:pos+avail])
	} else {
		copy(out[:first], g.data[pos:])
		copy(out[first:], g.data[:avail-first])
	}
	g.read.Store(w)
	return out, nil
}

// Close unmaps the ring memory. Calling Close more than once is a no-op.
func (g *Ring) Close() error {
	if g == nil || !g.closed.CompareAndSwap(false, true) {
		return nil
	}
	return syscall.Munmap(g.mem)
}
