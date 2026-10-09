//go:build unix && !linux

package recstream

import "os"

func createRingFile(dir string) (*os.File, error) { return createTempRingFile(dir) }
