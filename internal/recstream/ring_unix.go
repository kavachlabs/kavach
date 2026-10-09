//go:build unix

package recstream

import (
	"fmt"
	"os"
	"syscall"
)

// CreateRing creates the ring file of SPEC.md §10.7 in dir, unlinks it, maps
// it shared and initializes its header. The file is what the SDK passes to the
// recorder as file descriptor 3.
func CreateRing(dir string, capacity int) (*Ring, *os.File, error) {
	f, err := os.CreateTemp(dir, "kavach-ring-*")
	if err != nil {
		return nil, nil, err
	}
	os.Remove(f.Name())
	if err := f.Truncate(RingHeader + int64(capacity)); err != nil {
		f.Close()
		return nil, nil, err
	}
	g, err := mapRing(f, uint64(capacity))
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	initHeader(g.mem, uint64(capacity))
	return g, f, nil
}

// OpenRing maps the ring file the SDK passed and validates its header against
// the capacity in the open frame.
func OpenRing(f *os.File, capacity int) (*Ring, error) {
	g, err := mapRing(f, uint64(capacity))
	if err != nil {
		return nil, err
	}
	if err := checkHeader(g.mem, uint64(capacity)); err != nil {
		g.Close()
		return nil, err
	}
	return g, nil
}

func mapRing(f *os.File, capacity uint64) (*Ring, error) {
	size := RingHeader + int64(capacity)
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("ring: %w", err)
	}
	if fi.Size() < size {
		return nil, fmt.Errorf("ring: file is %d bytes, want %d", fi.Size(), size)
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("ring: %w", err)
	}
	return newRing(mem, capacity), nil
}

// Close unmaps the ring. Nothing may use it afterwards.
func (g *Ring) Close() error { return syscall.Munmap(g.mem) }
