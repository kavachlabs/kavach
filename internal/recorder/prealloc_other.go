//go:build !linux

package recorder

import "os"

const maxPrealloc = 0

func preallocate(*os.File, int64) {}

func trimPrealloc(*os.File) error { return nil }
