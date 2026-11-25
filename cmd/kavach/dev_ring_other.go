//go:build !unix

package main

import (
	"io"
	"os"
	"path/filepath"
)

// devRingConsumer falls back to tail-reading a regular stream file on non-Unix
// platforms where POSIX mmap is unavailable.
type devRingConsumer struct {
	path   string
	offset int64
}

func createDevRing(path string, _ uint64) (*devRingConsumer, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	return &devRingConsumer{path: path}, nil
}

func (c *devRingConsumer) PollOnce() ([]byte, error) {
	f, err := os.Open(c.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(c.offset, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	c.offset += int64(len(b))
	return b, nil
}

func (c *devRingConsumer) Close() error { return nil }
