package recorder

import (
	"os"
	"syscall"
)

// maxPrealloc bounds what a standby segment reserves: segment_bytes can be far
// more than the disk has free.
const maxPrealloc = 64 << 20

// preallocate reserves blocks past the end of f, without changing its size, so
// that writes into the segment do not wait on the allocator. It is a hint:
// filesystems that cannot are skipped.
func preallocate(f *os.File, n int64) {
	const keepSize = 1 // FALLOC_FL_KEEP_SIZE
	syscall.Fallocate(int(f.Fd()), keepSize, 0, n)
}

// trimPrealloc gives back what preallocate reserved and the segment did not use.
func trimPrealloc(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return f.Truncate(fi.Size())
}
