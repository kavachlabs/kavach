//go:build linux

package recstream

import (
	"os"
	"syscall"
	"unsafe"
)

const mfdCloexec = 1

// createRingFile returns an anonymous memory file, so the ring has no name at
// all (SPEC.md §10.7). Kernels without memfd_create get the temporary file.
func createRingFile(dir string) (*os.File, error) {
	if sysMemfdCreate == 0 {
		return createTempRingFile(dir)
	}
	name, _ := syscall.BytePtrFromString("kavach-ring")
	fd, _, e := syscall.Syscall(sysMemfdCreate, uintptr(unsafe.Pointer(name)), mfdCloexec, 0)
	if e != 0 {
		return createTempRingFile(dir)
	}
	return os.NewFile(fd, "kavach-ring"), nil
}
