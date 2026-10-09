//go:build linux

package kavach

import (
	"os"
	"syscall"
)

// setPipeSize enlarges the record stream's pipe to 1 MiB (SPEC.md §10.2).
func setPipeSize(f *os.File) {
	const fSetpipeSz = 1031 // syscall does not define F_SETPIPE_SZ
	if sc, err := f.SyscallConn(); err == nil {
		sc.Control(func(fd uintptr) { syscall.Syscall(syscall.SYS_FCNTL, fd, fSetpipeSz, 1<<20) })
	}
}
