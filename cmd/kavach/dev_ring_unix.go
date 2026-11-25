//go:build unix

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/kavachlabs/kavach/internal/recstream"
)

const (
	devRingMagic   = "KVRING02"
	devOffCapacity = 8
	devOffWrite    = 64
	devOffRead     = 128
	devRingHeader  = recstream.RingHeader // 65536
	devMinRing     = recstream.MinRing    // 65536
	devDefaultRing = recstream.DefaultRing
)

// devRingConsumer maps a single KVRING02 shared-memory ring file matching
// kavach::daemon::ShmRingConsumer and sdk/kavach-go/ring byte-for-byte.
type devRingConsumer struct {
	path  string
	mem   []byte
	data  []byte
	cap   uint64
	mask  uint64
	write *atomic.Uint64
	read  *atomic.Uint64
}

func createDevRing(path string, capacity uint64) (*devRingConsumer, error) {
	if capacity == 0 {
		capacity = devDefaultRing
	}
	if capacity < devMinRing || capacity&(capacity-1) != 0 {
		return nil, fmt.Errorf("ring size %d must be a power of two >= %d", capacity, devMinRing)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create ring %s: %w", path, err)
	}
	defer f.Close()

	total := int64(devRingHeader) + int64(capacity)
	if err := f.Truncate(total); err != nil {
		return nil, fmt.Errorf("truncate ring %s: %w", path, err)
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, int(total), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap ring %s: %w", path, err)
	}
	clear(mem[:devRingHeader])
	copy(mem[:len(devRingMagic)], devRingMagic)
	binary.LittleEndian.PutUint64(mem[devOffCapacity:], capacity)

	return &devRingConsumer{
		path:  path,
		mem:   mem,
		data:  mem[devRingHeader : devRingHeader+capacity],
		cap:   capacity,
		mask:  capacity - 1,
		write: (*atomic.Uint64)(unsafe.Pointer(&mem[devOffWrite])),
		read:  (*atomic.Uint64)(unsafe.Pointer(&mem[devOffRead])),
	}, nil
}

// PollOnce drains any unread bytes published into the KVRING02 ring and
// advances the read cursor with a release store.
func (c *devRingConsumer) PollOnce() ([]byte, error) {
	if c == nil || c.mem == nil {
		return nil, errors.New("dev ring closed")
	}
	w := c.write.Load()
	r := c.read.Load()
	if w < r {
		return nil, fmt.Errorf("ring write cursor moved backwards (%d < %d)", w, r)
	}
	avail := w - r
	if avail > c.cap {
		return nil, fmt.Errorf("ring unread bytes %d exceed capacity %d", avail, c.cap)
	}
	if avail == 0 {
		return nil, nil
	}
	out := make([]byte, avail)
	pos := r & c.mask
	first := c.cap - pos
	if first >= avail {
		copy(out, c.data[pos:pos+avail])
	} else {
		copy(out[:first], c.data[pos:])
		copy(out[first:], c.data[:avail-first])
	}
	c.read.Store(w)
	return out, nil
}

func (c *devRingConsumer) Close() error {
	if c == nil || c.mem == nil {
		return nil
	}
	err := syscall.Munmap(c.mem)
	c.mem = nil
	c.data = nil
	return err
}
