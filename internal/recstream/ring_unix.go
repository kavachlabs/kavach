//go:build unix

package recstream

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// CreateRing creates the ring file of SPEC.md §10.7, anonymous where the
// platform allows and otherwise in dir and unlinked at once, maps it shared and
// initializes its header. The file is what the SDK passes to the recorder as
// file descriptor 3.
func CreateRing(dir string, capacity int) (*Ring, *os.File, error) {
	f, err := createRingFile(dir)
	if err != nil {
		return nil, nil, err
	}
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

func createTempRingFile(dir string) (*os.File, error) {
	f, err := os.CreateTemp(dir, "kavach-ring-*")
	if err != nil {
		return nil, err
	}
	os.Remove(f.Name())
	return f, nil
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

// mapRing maps the file's header and data area, then the data area again right
// after it, inside one reserved region so nothing else can land between them.
func mapRing(f *os.File, capacity uint64) (*Ring, error) {
	size := RingHeader + int64(capacity)
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("ring: %w", err)
	}
	if fi.Size() < size {
		return nil, fmt.Errorf("ring: file is %d bytes, want %d", fi.Size(), size)
	}
	mem, err := syscall.Mmap(-1, 0, int(size)+int(capacity), syscall.PROT_NONE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("ring: %w", err)
	}
	base := uintptr(unsafe.Pointer(&mem[0]))
	const prot, flags = syscall.PROT_READ | syscall.PROT_WRITE, syscall.MAP_SHARED | syscall.MAP_FIXED
	fd := f.Fd()
	for _, m := range []struct{ at, n, off uintptr }{
		{base, uintptr(size), 0},
		{base + uintptr(size), uintptr(capacity), RingHeader},
	} {
		if _, _, e := syscall.Syscall6(syscall.SYS_MMAP, m.at, m.n, prot, flags, fd, m.off); e != 0 {
			syscall.Munmap(mem)
			return nil, fmt.Errorf("ring: %w", e)
		}
	}
	return newRing(mem, capacity), nil
}

// Close unmaps the ring. Nothing may use it afterwards.
func (g *Ring) Close() error { return syscall.Munmap(g.mem) }
