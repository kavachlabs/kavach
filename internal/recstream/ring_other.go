//go:build !unix

package recstream

import (
	"errors"
	"os"
)

var errNoRing = errors.New("ring: not supported on this platform")

func CreateRing(dir string, capacity int) (*Ring, *os.File, error) { return nil, nil, errNoRing }
func OpenRing(f *os.File, capacity int) (*Ring, error)             { return nil, errNoRing }
func (g *Ring) Close() error                                       { return nil }
