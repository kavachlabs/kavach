//go:build !unix

package ring

import (
	"errors"
	"sync/atomic"
)

// Ring is unavailable on non-Unix platforms; callers fall back to StreamWriter.
type Ring struct {
	dropped atomic.Uint64
}

func Create(path string, capacity uint64) (*Ring, error) {
	return nil, errors.New("ring: shared-memory ring requires unix")
}

func Open(path string) (*Ring, error) {
	return nil, errors.New("ring: shared-memory ring requires unix")
}

func OpenOrCreate(path string, capacity uint64) (*Ring, error) {
	return nil, errors.New("ring: shared-memory ring requires unix")
}

func (g *Ring) Capacity() uint64                 { return 0 }
func (g *Ring) Path() string                     { return "" }
func (g *Ring) WriteCursor() uint64              { return 0 }
func (g *Ring) ReadCursor() uint64               { return 0 }
func (g *Ring) Dropped() uint64                  { return g.dropped.Load() }
func (g *Ring) TryPublish([]byte) (bool, uint64) { return false, 0 }
func (g *Ring) PollOnce() ([]byte, error)        { return nil, errors.New("ring: unsupported") }
func (g *Ring) Close() error                     { return nil }
