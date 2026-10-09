package recstream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
	"unsafe"
)

// The shared-memory ring (SPEC.md §10.7): a header of RingHeader bytes, then
// the data area. write and read sit on cache lines of their own.
const (
	ringMagic    = "KVRING01"
	offCapacity  = 8
	offWrite     = 64
	offRead      = 128
	RingHeader   = 256
	MinRing      = 64 << 10
	DefaultRing  = 8 << 20
	ringPollTime = 5 * time.Millisecond
)

// Ring is a mapped ring. The SDK publishes into it and the recorder consumes
// from it; neither writes the other's header word.
type Ring struct {
	mem   []byte
	data  []byte
	cap   uint64
	write *atomic.Uint64
	read  *atomic.Uint64
}

func newRing(mem []byte, capacity uint64) *Ring {
	return &Ring{
		mem: mem, data: mem[RingHeader:], cap: capacity,
		write: (*atomic.Uint64)(unsafe.Pointer(&mem[offWrite])),
		read:  (*atomic.Uint64)(unsafe.Pointer(&mem[offRead])),
	}
}

// Capacity is the size of the data area.
func (g *Ring) Capacity() uint64 { return g.cap }

// TryPublish copies p into the ring and publishes it with one release store of
// write, so the recorder sees all of it or none. If p does not fit in the free
// space it publishes nothing and returns 0, unless p is larger than the whole
// ring, in which case it publishes as much as fits. used is the number of
// bytes the recorder has not consumed yet, counting the ones just published.
func (g *Ring) TryPublish(p []byte) (n int, used uint64) {
	w := g.write.Load()
	used = w - g.read.Load()
	n = len(p)
	if free := g.cap - used; uint64(n) > free {
		if uint64(n) <= g.cap {
			return 0, used
		}
		n = int(free)
	}
	off := w & (g.cap - 1)
	c := copy(g.data[off:], p[:n])
	copy(g.data, p[c:n])
	g.write.Store(w + uint64(n))
	return n, used + uint64(n)
}

// RingReader reads the ring as a byte stream, so that Reader can decode frames
// from it unchanged.
type RingReader struct {
	g    *Ring
	pos  uint64
	bell chan struct{}
	end  atomic.Bool
	tm   *time.Timer
}

// NewReader returns a reader that starts at the first byte of the stream.
func (g *Ring) NewReader() *RingReader {
	return &RingReader{g: g, bell: make(chan struct{}, 1), tm: time.NewTimer(ringPollTime)}
}

// Bell wakes a Read that is waiting: the SDK rang the doorbell.
func (rr *RingReader) Bell() {
	select {
	case rr.bell <- struct{}{}:
	default:
	}
}

// End says that standard input ended, so the service has. Read returns io.EOF
// once it has drained the ring.
func (rr *RingReader) End() {
	rr.end.Store(true)
	rr.Bell()
}

// Read blocks until the ring holds bytes or the service has ended. It returns
// an error if the header's words are impossible (SPEC.md §10.7).
func (rr *RingReader) Read(p []byte) (int, error) {
	g := rr.g
	for {
		// End is read first: whatever was published before the service ended
		// is then in the ring when write is loaded.
		ended := rr.end.Load()
		w := g.write.Load()
		switch {
		case w < rr.pos:
			return 0, fmt.Errorf("ring: write went backwards, from %d to %d", rr.pos, w)
		case w-rr.pos > g.cap:
			return 0, fmt.Errorf("ring: %d bytes unread in a ring of %d", w-rr.pos, g.cap)
		case w > rr.pos:
			n := min(uint64(len(p)), w-rr.pos)
			off := rr.pos & (g.cap - 1)
			c := copy(p[:n], g.data[off:])
			copy(p[c:n], g.data)
			rr.pos += n
			g.read.Store(rr.pos)
			return int(n), nil
		case ended:
			return 0, io.EOF
		}
		rr.tm.Reset(ringPollTime)
		select {
		case <-rr.bell:
			if !rr.tm.Stop() {
				<-rr.tm.C
			}
		case <-rr.tm.C:
		}
	}
}

func initHeader(mem []byte, capacity uint64) {
	copy(mem, ringMagic)
	binary.LittleEndian.PutUint64(mem[offCapacity:], capacity)
}

func checkHeader(mem []byte, want uint64) error {
	if string(mem[:8]) != ringMagic {
		return errors.New("ring: bad magic")
	}
	if c := binary.LittleEndian.Uint64(mem[offCapacity:]); c != want {
		return fmt.Errorf("ring: header capacity %d, open frame says %d", c, want)
	}
	return nil
}
